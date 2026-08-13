package server

// Rebuild spike: the upcoming supervisor loop rebuilds the Server in-process
// on every config apply (Stop old -> NewServer -> Start). This spec proves
// that two full NewServer/Start/Stop cycles work in one process and that the
// prometheus registry serves the collectors of the *newest* server after a
// rebuild (see metrics.RegisterMetric replace-on-conflict semantics).

import (
	"context"
	"net"
	"time"

	"github.com/0xERR0R/blocky/config"
	. "github.com/0xERR0R/blocky/helpertest"
	"github.com/0xERR0R/blocky/metrics"
	"github.com/0xERR0R/blocky/util"

	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const rebuildDNSBasePort = 57000

var _ = Describe("Server rebuild", func() {
	newRebuildConfig := func() *config.Config {
		return &config.Config{
			Upstreams: config.Upstreams{
				// the per-request context deadline is derived from this timeout
				Timeout: config.Duration(time.Second),
				Groups: map[string][]config.Upstream{
					// never contacted: the test only queries the custom DNS mapping
					"default": {config.Upstream{Net: config.NetProtocolTcpUdp, Host: "4.4.4.4", Port: 53}},
				},
			},
			CustomDNS: config.CustomDNS{
				CustomTTL: config.Duration(time.Hour),
				Mapping: config.CustomDNSMapping{
					"custom.lan": {&dns.A{A: net.ParseIP("192.168.178.55")}},
				},
			},
			Blocking: config.Blocking{BlockType: "zeroIp"},
			Ports: config.Ports{
				DNS:     config.ListenConfig{GetHostPort("127.0.0.1", rebuildDNSBasePort)},
				DOHPath: "/dns-query",
			},
			Prometheus: config.Metrics{
				Enable: true,
				Path:   "/metrics",
			},
		}
	}

	queryCustomLan := func(ctx context.Context) {
		c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
		msg := util.NewMsgWithQuestion("custom.lan.", A)

		var (
			resp *dns.Msg
			err  error
		)

		// the listener goroutine may not have entered its accept loop yet
		Eventually(ctx, func() error {
			resp, _, err = c.ExchangeContext(ctx, msg, GetHostPort("127.0.0.1", rebuildDNSBasePort))

			return err
		}, "2s", "100ms").Should(Succeed())

		Expect(resp).Should(BeDNSRecord("custom.lan.", A, "192.168.178.55"))
	}

	// sums all samples of the blocky_query_total counter in the global registry
	gatheredQueryTotal := func() float64 {
		mfs, err := metrics.Reg.Gather()
		Expect(err).Should(Succeed())

		var sum float64

		for _, mf := range mfs {
			if mf.GetName() == "blocky_query_total" {
				for _, m := range mf.GetMetric() {
					sum += m.GetCounter().GetValue()
				}
			}
		}

		return sum
	}

	It("runs NewServer/Start/Stop twice in one process and serves live metrics", func() {
		for cycle := 1; cycle <= 2; cycle++ {
			ctx, cancel := context.WithCancel(context.Background())

			srv, err := NewServer(ctx, newRebuildConfig(), nil)
			Expect(err).Should(Succeed(), "NewServer failed in cycle %d", cycle)

			errChan := make(chan error, 10)
			go srv.Start(ctx, errChan)

			queryCustomLan(ctx)

			Consistently(errChan, "200ms").ShouldNot(Receive())

			// The global registry must serve the collectors of THIS server:
			// each rebuild replaces the previous collectors (counter reset),
			// so exactly the one query of the current cycle is visible.
			Expect(gatheredQueryTotal()).Should(BeNumerically("==", 1),
				"registry should serve the live server's counters in cycle %d", cycle)

			Expect(srv.Stop(ctx)).Should(Succeed(), "Stop failed in cycle %d", cycle)
			cancel()
		}
	})
})
