package cmd

import (
	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/log"

	"github.com/spf13/cobra"
)

// NewValidateCommand creates new command instance
func NewValidateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Args:  cobra.NoArgs,
		Short: "Validates the configuration stored in the config database",
		RunE:  validateConfiguration,
	}
}

func validateConfiguration(_ *cobra.Command, _ []string) error {
	log.Log().Infof("Validating configuration database in: %s", dbDir)

	store, err := configstore.Open(dbDir)
	if err != nil {
		return err
	}
	defer store.Close()

	if _, err := store.LoadConfig(); err != nil {
		return err
	}

	log.Log().Info("Configuration is valid")

	return nil
}
