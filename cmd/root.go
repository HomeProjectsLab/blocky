package cmd

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"

	"github.com/0xERR0R/blocky/api"
	"github.com/0xERR0R/blocky/log"
	"github.com/spf13/cobra"
)

//nolint:gochecknoglobals
var (
	dbDir   string
	apiHost string
	apiPort uint16
)

const (
	defaultPort = 4000
	defaultHost = "localhost"
	dbDirEnvVar = "BLOCKY_DB_DIR"
)

// NewRootCommand creates a new root cli command instance
func NewRootCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "blocky",
		Short: "blocky is a DNS proxy ",
		Long: `A fast and configurable DNS Proxy
and ad-blocker for local network.

Complete documentation is available at https://github.com/0xERR0R/blocky`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return newServeCommand().RunE(cmd, args)
		},
		SilenceUsage: true,
	}

	c.PersistentFlags().StringVar(&dbDir, "db-dir", defaultDBDir(),
		"directory containing blocky's config database (config.db), created if missing (env "+dbDirEnvVar+")")
	c.PersistentFlags().StringVar(&apiHost, "apiHost", defaultHost, "host of blocky (API)")
	c.PersistentFlags().Uint16Var(&apiPort, "apiPort", defaultPort, "port of blocky (API)")

	c.AddCommand(newRefreshCommand(),
		NewQueryCommand(),
		NewVersionCommand(),
		newServeCommand(),
		newBlockingCommand(),
		NewListsCommand(),
		NewHealthcheckCommand(),
		newCacheCommand(),
		NewStatsCommand(),
		newImportCommand(),
		NewValidateCommand())

	return c
}

func defaultDBDir() string {
	if val, present := os.LookupEnv(dbDirEnvVar); present {
		return val
	}

	return "."
}

func apiURL() string {
	return fmt.Sprintf("http://%s%s", net.JoinHostPort(apiHost, strconv.Itoa(int(apiPort))), "/api")
}

// newAPIClient creates a new API client with the configured URL
func newAPIClient() (*api.ClientWithResponses, error) {
	client, err := api.NewClientWithResponses(apiURL())
	if err != nil {
		return nil, fmt.Errorf("can't create client: %w", err)
	}

	return client, nil
}

// Execute starts the command
func Execute() {
	if err := NewRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

type codeWithStatus interface {
	StatusCode() int
	Status() string
}

func printOkOrError(resp codeWithStatus, body string) error {
	if resp.StatusCode() == http.StatusOK {
		log.Log().Info("OK")
	} else {
		return fmt.Errorf("response NOK, %s %s", resp.Status(), body)
	}

	return nil
}
