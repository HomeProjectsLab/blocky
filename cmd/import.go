package cmd

import (
	"errors"
	"fmt"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/log"

	"github.com/spf13/cobra"
)

func newImportCommand() *cobra.Command {
	var force bool

	c := &cobra.Command{
		Use:   "import <config.yml|directory>",
		Args:  cobra.ExactArgs(1),
		Short: "Imports a YAML config file (or directory of files) into the config database",
		RunE: func(_ *cobra.Command, args []string) error {
			return importConfig(args[0], force)
		},
		SilenceUsage: true,
	}

	c.Flags().BoolVar(&force, "force", false, "overwrite a config database that was already modified")

	return c
}

func importConfig(path string, force bool) error {
	// prove the source parses through the full pipeline before touching the store
	if _, err := config.LoadConfig(path, true); err != nil {
		return fmt.Errorf("can't import '%s': %w", path, err)
	}

	raw, err := config.ReadRawSource(path)
	if err != nil {
		return fmt.Errorf("can't read '%s': %w", path, err)
	}

	store, err := configstore.Open(dbDir)
	if err != nil {
		return fmt.Errorf("can't open config store: %w", err)
	}
	defer store.Close()

	fresh, err := store.IsFresh()
	if err != nil {
		return err
	}

	if !fresh && !force {
		return errors.New("config database already contains a modified configuration, use --force to overwrite")
	}

	if err := store.SetRawYAML(string(raw)); err != nil {
		return err
	}

	log.Log().Infof("imported %d bytes of configuration from '%s' into '%s/config.db'", len(raw), path, dbDir)

	return nil
}
