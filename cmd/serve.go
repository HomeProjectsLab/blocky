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
)

func newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "serve",
		Args:         cobra.NoArgs,
		Short:        "start blocky DNS server (default command)",
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

	return runSupervisor(store)
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

	lastGood := cfg

	// (re)build loop — each iteration binds fresh listeners for cfg; a normal
	// config apply never reaches here, it hot-swaps inside the inner serve loop.
	for {
		log.Configure(&cfg.Log)
		warnMissingPrivilegedPortCapability(cfg.Ports)

		serverCtx, shutdown := context.WithCancel(context.Background())

		srv, err := server.NewServer(serverCtx, cfg, store)
		if err != nil {
			shutdown()

			if cfg == lastGood {
				return fmt.Errorf("can't start server: %w", err)
			}

			log.Log().Errorf("can't apply new config, rolling back to last applied config: %v", err)
			cfg = lastGood

			continue
		}

		lastGood = cfg

		errChan := make(chan error, errChanSize)
		srv.Start(serverCtx, errChan)
		store.MarkApplied()
		evt.Bus().Publish(evt.ApplicationStarted, util.Version, util.BuildTime)

		restartCfg, err := serve(store, srv, serverCtx, &lastGood, errChan)

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
	for {
		select {
		case <-signals:
			log.Log().Infof("Terminating...")

			return nil, nil

		case err := <-errChan:
			log.Log().Error("server start failed: ", err)

			return nil, err

		case <-store.ApplyRequested():
			newCfg, err := store.LoadConfig()
			if err != nil {
				log.Log().Errorf("stored config is invalid, keeping the running config: %v", err)

				continue
			}

			if !server.ListenersCompatible(*lastGood, newCfg) {
				log.Log().Info("listener-affecting config changed; full restart")

				return newCfg, nil
			}

			if err := srv.ApplyConfig(serverCtx, newCfg); err != nil {
				log.Log().Errorf("can't apply new config, keeping the running config: %v", err)

				continue
			}

			*lastGood = newCfg
			store.MarkApplied()
			log.Log().Info("configuration applied without dropping listeners")
		}
	}
}

// stopServerGracefully stops srv with the shutdown timeout, logging any error.
func stopServerGracefully(srv *server.Server) {
	stopCtx, stopCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer stopCancel()

	util.LogOnError(stopCtx, "can't stop server: ", srv.Stop(stopCtx))
}

func printBanner() {
	log.Log().Info("_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/")
	log.Log().Info("_/                                                              _/")
	log.Log().Info("_/                                                              _/")
	log.Log().Info("_/       _/        _/                      _/                   _/")
	log.Log().Info("_/      _/_/_/    _/    _/_/      _/_/_/  _/  _/    _/    _/    _/")
	log.Log().Info("_/     _/    _/  _/  _/    _/  _/        _/_/      _/    _/     _/")
	log.Log().Info("_/    _/    _/  _/  _/    _/  _/        _/  _/    _/    _/      _/")
	log.Log().Info("_/   _/_/_/    _/    _/_/      _/_/_/  _/    _/    _/_/_/       _/")
	log.Log().Info("_/                                                    _/        _/")
	log.Log().Info("_/                                               _/_/           _/")
	log.Log().Info("_/                                                              _/")
	log.Log().Info("_/                                                              _/")
	log.Log().Infof("_/  Version: %-18s Build time: %-18s  _/", util.Version, util.BuildTime)
	log.Log().Info("_/                                                              _/")
	log.Log().Info("_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/_/")
}
