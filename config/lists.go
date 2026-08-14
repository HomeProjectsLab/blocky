package config

import (
	"errors"
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
	Enable bool `default:"true" yaml:"enable"`
	// IntervalHours between upstream version checks (default weekly).
	IntervalHours uint `default:"168" yaml:"intervalHours"`
	// TrancoURL is the Tranco "latest list" API base. The updater GETs
	// <TrancoURL>/api/lists/date/latest for the list id, then downloads
	// <TrancoURL>/download/<id>/1000000.
	TrancoURL string `default:"https://tranco-list.eu" yaml:"trancoUrl"`
	// BlocklistRepo is the blocklistproject GitHub repo (owner/name) whose main
	// commit SHA gates blocklist refresh.
	BlocklistRepo string `default:"blocklistproject/Lists" yaml:"blocklistRepo"`
}

// MaxIntervalHours bounds any hours-valued config that becomes a time.Duration.
// Beyond roughly 292 years the hours*time.Hour multiplication overflows int64
// into a negative duration, and time.NewTicker panics on a non-positive
// interval. One year is far past any sane refresh cadence.
const MaxIntervalHours = 24 * 365

func (c *ListsConfig) validate(_ *logrus.Entry) error {
	if !c.Updater.Enable {
		return nil
	}

	// Upper bound matters: the interval is turned into a time.Duration in hours,
	// and a large enough value overflows int64 into a negative duration, which
	// makes time.NewTicker panic and takes the process down at startup.
	if c.Updater.IntervalHours == 0 || c.Updater.IntervalHours > MaxIntervalHours {
		return fmt.Errorf("lists.updater.intervalHours must be in [1, %d] when enabled", MaxIntervalHours)
	}

	if c.Updater.TrancoURL == "" {
		return errors.New("lists.updater.trancoUrl must not be empty when enabled")
	}

	if c.Updater.BlocklistRepo == "" {
		return errors.New("lists.updater.blocklistRepo must not be empty when enabled")
	}

	return nil
}
