package config

import (
	"fmt"

	"github.com/sirupsen/logrus"
)

// ListsConfig configures the unified list-updater subsystem that keeps the
// Tranco decoy list and the blocklistproject category lists fresh and persisted
// across restart. The embedded assets are the offline cold-start floor; the
// updater only replaces a source when its upstream version differs from the
// version stored in the query-log database.
type ListsConfig struct {
	Updater ListUpdaterConfig `yaml:"updater"`
}

// ListUpdaterConfig controls the background refresh loop. Requires the sqlite
// query log (the seeded tables live in that database).
type ListUpdaterConfig struct {
	// Enable the background updater. Seeding from embedded assets still happens
	// on startup regardless, so downstream consumers always have data.
	Enable bool `yaml:"enable" default:"true"`
	// IntervalHours between upstream version checks (default weekly).
	IntervalHours uint `yaml:"intervalHours" default:"168"`
	// TrancoURL is the Tranco "latest list" API base. The updater GETs
	// <TrancoURL>/api/lists/date/latest for the list id, then downloads
	// <TrancoURL>/download/<id>/1000000.
	TrancoURL string `yaml:"trancoUrl" default:"https://tranco-list.eu"`
	// BlocklistRepo is the blocklistproject GitHub repo (owner/name) whose main
	// commit SHA gates blocklist refresh.
	BlocklistRepo string `yaml:"blocklistRepo" default:"blocklistproject/Lists"`
}

func (c *ListsConfig) validate(_ *logrus.Entry) error {
	if !c.Updater.Enable {
		return nil
	}

	if c.Updater.IntervalHours == 0 {
		return fmt.Errorf("lists.updater.intervalHours must be > 0 when enabled")
	}

	if c.Updater.TrancoURL == "" {
		return fmt.Errorf("lists.updater.trancoUrl must not be empty when enabled")
	}

	if c.Updater.BlocklistRepo == "" {
		return fmt.Errorf("lists.updater.blocklistRepo must not be empty when enabled")
	}

	return nil
}
