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

// runSupervisor runs the build/start/wait server cycle until a termination
// signal arrives or the server fails fatally. Each ApplyRequested signal stops
// the running server and rebuilds it from the store; when the new config can't
// be applied, it rolls back to the last successfully applied one.
func runSupervisor(store *configstore.Store) error {
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	var lastGood *config.Config

	for {
		cfg, err := store.LoadConfig()
		if err != nil {
			if lastGood == nil {
				return fmt.Errorf("unable to load configuration: %w", err)
			}

			log.Log().Errorf("stored config is invalid, keeping the running config: %v", err)
			cfg = lastGood
		}

		log.Configure(&cfg.Log)

		warnMissingPrivilegedPortCapability(cfg.Ports)

		srvCtx, cancelFn := context.WithCancel(context.Background())

		srv, err := server.NewServer(srvCtx, cfg, store)
		if err != nil {
			if lastGood == nil || cfg == lastGood {
				cancelFn()

				return fmt.Errorf("can't start server: %w", err)
			}

			log.Log().Errorf("can't apply new config, rolling back to last applied config: %v", err)

			cfg = lastGood

			srv, err = server.NewServer(srvCtx, cfg, store)
			if err != nil {
				cancelFn()

				return fmt.Errorf("can't start server with last applied config: %w", err)
			}
		}

		lastGood = cfg

		errChan := make(chan error, errChanSize)

		srv.Start(srvCtx, errChan)
		store.MarkApplied()

		evt.Bus().Publish(evt.ApplicationStarted, util.Version, util.BuildTime)

		select {
		case <-signals:
			log.Log().Infof("Terminating...")

			// Cancel background operations (periodic refresh, etc.)
			cancelFn()
			stopServerGracefully(srv)

			return nil

		case err := <-errChan:
			log.Log().Error("server start failed: ", err)
			cancelFn()

			return err

		case <-store.ApplyRequested():
			log.Log().Info("configuration change requested, restarting server")

			cancelFn()
			stopServerGracefully(srv)
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
