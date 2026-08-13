package cmd

import (
	"time"

	"github.com/0xERR0R/blocky/tui"

	"github.com/spf13/cobra"
)

// newDashboardCommand runs the full-screen htop-style console dashboard. It is a
// pure consumer of the local /api/ui/* HTTP + SSE endpoints, so it renders on a
// bare framebuffer TTY (the Pi's HDMI console) with no X11 and no browser.
func newDashboardCommand() *cobra.Command {
	var (
		apiURL  string
		refresh time.Duration
	)

	c := &cobra.Command{
		Use:          "dashboard",
		Args:         cobra.NoArgs,
		Short:        "full-screen console dashboard (htop-style live view over the local API)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return tui.New(tui.NewClient(apiURL), refresh).Run()
		},
	}

	c.Flags().StringVar(&apiURL, "api-url", "http://localhost:80",
		"base URL of the blocky web API to read from")
	c.Flags().DurationVar(&refresh, "refresh", time.Second,
		"meter refresh interval (the query stream updates live via SSE)")

	return c
}
