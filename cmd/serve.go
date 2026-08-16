package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/evt"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/server"
	"github.com/0xERR0R/blocky/util"

	"github.com/spf13/cobra"
)

//nolint:gochecknoglobals
var (
	signals = make(chan os.Signal, 1)

	// raiseNetBindService is a seam so tests can stub the capability raise.
	raiseNetBindService = util.RaiseNetBindService
)

const (
	shutdownTimeout = 10 * time.Second
	errChanSize     = 10

	// bindGracePeriod is how long a freshly-started server may report an async
	// listener bind error (DNS ListenAndServe binds in a goroutine) before the
	// config is promoted to lastGood and marked applied. Bind failures
	// (EADDRINUSE, EACCES) surface immediately, well within this window.
	// ponytail: a grace window, not a positive listeners-up signal; wire
	// dns.Server.NotifyStartedFunc through server.Start if a slow bind ever slips past.
	bindGracePeriod = 500 * time.Millisecond
)

func newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "serve",
		Args:         cobra.NoArgs,
		Short:        "start JungleBlock DNS server (default command)",
		RunE:         startServer,
		SilenceUsage: true,
	}
}

// privilegedPortCapHint describes how to satisfy the CAP_NET_BIND_SERVICE
// requirement for binding ports below 1024.
const privilegedPortCapHint = "grant CAP_NET_BIND_SERVICE (Kubernetes " +
	"securityContext capabilities.add, or docker run --cap-add NET_BIND_SERVICE) " +
	"or use a port >= 1024"

// warnMissingPrivilegedPortCapability raises CAP_NET_BIND_SERVICE if it is
// available, and warns when a privileged port (< 1024) is configured but the
// capability could not be obtained. It never fails: any real bind error
// surfaces later from server.NewServer.
func warnMissingPrivilegedPortCapability(ports config.Ports) {
	effective, err := raiseNetBindService()
	if err != nil {
		if privileged := ports.PrivilegedPorts(); len(privileged) > 0 {
			log.Log().Warnf("could not adjust process capabilities (%v); binding "+
				"privileged port(s) %s may fail — %s",
				err, strings.Join(privileged, ", "), privilegedPortCapHint)

			return
		}

		log.Log().Warnf("could not adjust process capabilities: %v", err)

		return
	}

	if effective {
		return
	}

	if privileged := ports.PrivilegedPorts(); len(privileged) > 0 {
		log.Log().Warnf("configured to listen on privileged port(s) %s without "+
			"CAP_NET_BIND_SERVICE; %s", strings.Join(privileged, ", "), privilegedPortCapHint)
	}
}

func startServer(_ *cobra.Command, _ []string) error {
	printBanner()

	store, err := configstore.Open(dbDir)
	if err != nil {
		return fmt.Errorf("can't open config store: %w", err)
	}
	defer store.Close()

	seedDefaultPassword(store)

	return runSupervisor(store)
}

// seedDefaultPassword gives a freshly-provisioned appliance a working web-UI
// login out of the box, so there is no first-run setup dance. It acts ONLY when
// no password has been set yet; the value defaults to "jungle" and can be
// overridden with JUNGLEBLOCK_DEFAULT_PASSWORD. This is a LAN-appliance
// convenience — the UI is LAN-only — so it logs loudly to nudge a change in the
// Settings page. ponytail: a known default on a LAN box is acceptable for the
// threat model; change it (or set the env) if the box is ever exposed wider.
func seedDefaultPassword(store *configstore.Store) {
	if store.IsAuthConfigured() {
		return
	}

	pw := os.Getenv("JUNGLEBLOCK_DEFAULT_PASSWORD")
	source := "env"
	if pw == "" {
		pw = "jungle"
		source = "built-in default"
	}

	if err := store.SetPassword(pw); err != nil {
		log.PrefixedLog("supervisor").Warnf("could not seed the default web-UI password: %v", err)
		return
	}

	// never log the password value — the console mirrors this line to the web UI.
	log.PrefixedLog("supervisor").WithField("source", source).
		Warn("seeded a DEFAULT web-UI password — change it in the Settings page")
}

// runSupervisor builds and starts the server, then serves until a termination
// signal or fatal error. Each ApplyRequested signal rebuilds ONLY the resolver
// chain and swaps it atomically behind the live listeners (Server.ApplyConfig) —
// :80/:53 keep serving across the apply. A bad config is logged and dropped; the
// running config keeps serving.
//
// A config that changes the listeners/router (ports, TLS, HTTP/3, query-log
// target) can't be hot-swapped: ListenersCompatible guards this and the outer
// loop does a full rebuild (brief downtime). If that rebuild fails it rolls back
// to the last applied config so the box never ends up with nothing serving.
func runSupervisor(store *configstore.Store) error {
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	cfg, err := store.LoadConfig()
	if err != nil {
		return fmt.Errorf("unable to load configuration: %w", err)
	}

	cfgLoadedAt := time.Now()

	lastGood := cfg
	rolledBack := false // cfg is lastGood, not the (broken) stored config

	slog := log.PrefixedLog("supervisor")
	slog.WithField("version", util.Version).Info("supervisor starting")

	// (re)build loop — each iteration binds fresh listeners for cfg; a normal
	// config apply never reaches here, it hot-swaps inside the inner serve loop.
	for {
		log.Configure(&cfg.Log)
		warnMissingPrivilegedPortCapability(cfg.Ports)

		slog.Info("building server (full rebuild, binding listeners)")

		serverCtx, shutdown := context.WithCancel(context.Background())

		srv, err := server.NewServer(serverCtx, cfg, store)
		if err != nil {
			shutdown()

			if cfg == lastGood {
				return fmt.Errorf("can't start server: %w", err)
			}

			slog.WithField("error", err.Error()).Warn("can't apply new config, rolling back to last applied config")
			cfg = lastGood
			rolledBack = true

			continue
		}

		errChan := make(chan error, errChanSize)
		srv.Start(serverCtx, errChan)

		// DNS listeners bind asynchronously inside Start; don't promote cfg to
		// lastGood (or mark it applied) until the bind grace period passes, or a
		// config whose ports can't bind would be recorded clean with no rollback
		// → systemd crash-loop on a broken config.
		select {
		case err := <-errChan:
			shutdown()
			stopServerGracefully(srv)

			if cfg == lastGood {
				return fmt.Errorf("can't start server: %w", err)
			}

			slog.WithField("error", err.Error()).
				Warn("listener bind failed for new config, rolling back to last applied config")
			cfg = lastGood
			rolledBack = true

			continue
		case <-time.After(bindGracePeriod):
		}

		lastGood = cfg

		// after a rollback the STORED config is still the broken one that never
		// built — marking it applied would report it clean and brick the next boot
		if !rolledBack {
			markAppliedIfUnchanged(store, cfgLoadedAt)
		}

		rolledBack = false
		evt.Bus().Publish(evt.ApplicationStarted, util.Version, util.BuildTime)
		slog.Info("server started, listeners bound")

		restartCfg, err := serve(store, srv, serverCtx, &lastGood, errChan)
		// restartCfg (if any) was loaded inside serve just before it returned;
		// stamp it here, before the potentially slow graceful stop below.
		cfgLoadedAt = time.Now()

		shutdown()
		stopServerGracefully(srv)

		if restartCfg == nil {
			// signal (nil err) or fatal server error
			return err
		}

		// listener-affecting change: rebuild with the new config (rolling back
		// to lastGood above if the rebuild fails)
		cfg = restartCfg
	}
}

// serve runs the inner event loop for one running server: it hot-swaps the
// resolver chain on every compatible apply and reports back to runSupervisor
// only when the process must stop (returns nil restartCfg) or when a
// listener-affecting apply requires a full rebuild (returns the new config).
// lastGood is updated in place on each successful hot-swap so a later failed
// rebuild rolls back to the genuinely last-applied config.
func serve(
	store *configstore.Store, srv *server.Server, serverCtx context.Context,
	lastGood **config.Config, errChan <-chan error,
) (*config.Config, error) {
	slog := log.PrefixedLog("supervisor")

	for {
		select {
		case <-signals:
			slog.Info("shutdown signal received, terminating")

			return nil, nil

		case err := <-errChan:
			slog.WithField("error", err.Error()).Error("server start failed")

			return nil, err

		case <-store.ApplyRequested():
			slog.Info("applying config change")

			loadedAt := time.Now()

			newCfg, err := store.LoadConfig()
			if err != nil {
				slog.WithField("error", err.Error()).Warn("stored config is invalid, keeping the running config")

				continue
			}

			if !server.ListenersCompatible(*lastGood, newCfg) {
				slog.Info("listener-affecting config changed; full rebuild (brief downtime)")

				return newCfg, nil
			}

			// logging is global and not part of ListenersCompatible, so a
			// log-only change takes this hot-swap path; reapply it here or the
			// level/format/timestamp change would be ignored until a full restart.
			log.Configure(&newCfg.Log)

			if err := srv.ApplyConfig(serverCtx, newCfg); err != nil {
				slog.WithField("error", err.Error()).Warn("can't apply new config, keeping the running config")

				continue
			}

			*lastGood = newCfg
			markAppliedIfUnchanged(store, loadedAt)
			slog.Info("config applied via zero-downtime hot-swap without dropping listeners")
		}
	}
}

// markAppliedIfUnchanged marks the stored config applied unless it was edited
// after loadedAt (the moment the now-running config was loaded). MarkApplied
// stamps time.Now(), so marking unconditionally would absorb any edit that
// landed during the bind grace period or a slow ApplyConfig, silently reporting
// it clean. Skipping keeps the store dirty — the pending-apply banner shows and
// the next apply picks the edit up. A Status error also skips (fail closed:
// stays dirty). ponytail: tiny TOCTOU window between Status and MarkApplied
// remains; a MarkAppliedAt(loadedAt) in configstore would close it.
func markAppliedIfUnchanged(store *configstore.Store, loadedAt time.Time) {
	_, _, updatedAt, err := store.Status()
	if err != nil || updatedAt.After(loadedAt) {
		return
	}

	store.MarkApplied()
}

// stopServerGracefully stops srv with the shutdown timeout, logging any error.
func stopServerGracefully(srv *server.Server) {
	stopCtx, stopCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer stopCancel()

	util.LogOnError(stopCtx, "can't stop server: ", srv.Stop(stopCtx))
}

// banner is the JungleBlock startup wordmark (ANSI Shadow, "JUNGLE" over
// "BLOCK"). Box-drawing + block glyphs only — no backticks/backslashes — so it
// lives happily in a raw string literal.
const banner = `
     ██╗██╗   ██╗███╗   ██╗ ██████╗ ██╗     ███████╗
     ██║██║   ██║████╗  ██║██╔════╝ ██║     ██╔════╝
     ██║██║   ██║██╔██╗ ██║██║  ███╗██║     █████╗
██   ██║██║   ██║██║╚██╗██║██║   ██║██║     ██╔══╝
╚█████╔╝╚██████╔╝██║ ╚████║╚██████╔╝███████╗███████╗
 ╚════╝  ╚═════╝ ╚═╝  ╚═══╝ ╚═════╝ ╚══════╝╚══════╝
██████╗ ██╗      ██████╗  ██████╗██╗  ██╗
██╔══██╗██║     ██╔═══██╗██╔════╝██║ ██╔╝
██████╔╝██║     ██║   ██║██║     █████╔╝
██╔══██╗██║     ██║   ██║██║     ██╔═██╗
██████╔╝███████╗╚██████╔╝╚██████╗██║  ██╗
╚═════╝ ╚══════╝ ╚═════╝  ╚═════╝╚═╝  ╚═╝`

func printBanner() {
	for _, line := range strings.Split(strings.Trim(banner, "\n"), "\n") {
		log.Log().Info(line)
	}
	log.Log().Info("  🌴 JungleBlock — a wilder DNS  🦍")
	log.Log().Infof("     Version: %s   Build time: %s", util.Version, util.BuildTime)
}
