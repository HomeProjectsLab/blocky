package cmd

import (
	"net"
	"time"

	"github.com/0xERR0R/blocky/tui"

	"github.com/spf13/cobra"
)

// newDashboardCommand runs the full-screen htop-style console dashboard. It is a
// pure consumer of the local /api/ui/* HTTP + SSE endpoints, so it renders on a
// bare framebuffer TTY (the Pi's HDMI console) with no X11 and no browser.
func newDashboardCommand() *cobra.Command {
	var (
		apiURL     string
		refresh    time.Duration
		telnetAddr string
		cols, rows int
	)

	c := &cobra.Command{
		Use:          "dashboard",
		Args:         cobra.NoArgs,
		Short:        "full-screen console dashboard (htop-style live view over the local API)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			d := tui.New(tui.NewClient(apiURL), refresh)

			// --telnet: headless, broadcast the rendered dashboard to every TCP
			// client read-only (no local TTY). Otherwise drive the local console.
			if telnetAddr != "" {
				ln, err := net.Listen("tcp", telnetAddr)
				if err != nil {
					return err
				}
				defer ln.Close()

				return d.ServeTelnet(ln, cols, rows)
			}

			return d.Run()
		},
	}

	c.Flags().StringVar(&apiURL, "api-url", "http://localhost:80",
		"base URL of the JungleBlock web API to read from")
	c.Flags().DurationVar(&refresh, "refresh", time.Second,
		"meter refresh interval (the query stream updates live via SSE)")
	c.Flags().StringVar(&telnetAddr, "telnet", "",
		"serve the dashboard read-only over TCP on this addr (e.g. 127.0.0.1:2323); input is ignored. Empty = local console")
	c.Flags().IntVar(&cols, "cols", 100, "virtual terminal width for --telnet")
	c.Flags().IntVar(&rows, "rows", 30, "virtual terminal height for --telnet")

	return c
}
