//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package decoy

import (
	"context"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// allVendorDomains flattens the embedded vendor-telemetry set for membership tests.
func allVendorDomains() map[string]bool {
	out := map[string]bool{}
	for _, hosts := range vendorTelemetry {
		for _, h := range hosts {
			out[h] = true
		}
	}

	return out
}

// domainFamily reverse-maps a beacon domain back to its vendor family.
func domainFamily(name string) string {
	name = strings.TrimSuffix(name, ".")
	for fam, hosts := range vendorTelemetry {
		for _, h := range hosts {
			if h == name {
				return fam
			}
		}
	}

	return ""
}

var _ = Describe("Device-class personas (task E)", func() {
	var cfg config.DecoyConfig

	BeforeEach(func() {
		var e error
		cfg, e = config.WithDefaults[config.DecoyConfig]()
		Expect(e).Should(Succeed())
		cfg.Enable = true
	})

	// capture returns a deterministic engine (seeded rng) recording every request.
	capture := func(src Source) (*Engine, func() []*model.Request) {
		var mu sync.Mutex
		var got []*model.Request
		eng := NewEngine(cfg, src, func(_ context.Context, req *model.Request) (*model.Response, error) {
			mu.Lock()
			got = append(got, req)
			mu.Unlock()

			return &model.Response{Res: new(dns.Msg)}, nil
		})
		eng.rnd = rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test

		return eng, func() []*model.Request {
			mu.Lock()
			defer mu.Unlock()

			return append([]*model.Request(nil), got...)
		}
	}

	It("shapes an iot-attributed decoy as a vendor-telemetry beacon, not a cohort", func() {
		src := &mockSource{persona: querylog.ClientPersona{IP: "10.0.0.9"}, class: querylog.ClassIoT}
		eng, snap := capture(src)

		vendor := allVendorDomains()
		for i := 0; i < 50; i++ {
			eng.emit(context.Background())
		}

		reqs := snap()
		Expect(reqs).ShouldNot(BeEmpty())
		for _, r := range reqs {
			name := strings.TrimSuffix(r.Req.Question[0].Name, ".")
			Expect(vendor).Should(HaveKey(name), "iot decoy must beacon to vendor telemetry, got "+name)
			Expect(r.DecoySource).Should(Equal(provBeacon))
			Expect(r.Req.Question[0].Qtype).Should(BeElementOf(dns.TypeA, dns.TypeAAAA)) // low qtype diversity
		}
	})

	It("shapes a server-attributed decoy as a registry/update lookup", func() {
		src := &mockSource{persona: querylog.ClientPersona{IP: "10.0.0.10"}, class: querylog.ClassServer}
		eng, snap := capture(src)

		server := map[string]bool{}
		for _, d := range serverTelemetry {
			server[d] = true
		}

		for i := 0; i < 50; i++ {
			eng.emit(context.Background())
		}

		reqs := snap()
		Expect(reqs).ShouldNot(BeEmpty())
		for _, r := range reqs {
			name := strings.TrimSuffix(r.Req.Question[0].Name, ".")
			Expect(server).Should(HaveKey(name), "server decoy must hit a registry/update endpoint, got "+name)
			Expect(r.DecoySource).Should(Equal(provServer))
		}
	})

	It("still browses a workstation-attributed decoy", func() {
		// isolate the plain structural path: no fan-out branches, so emit lands on the
		// list fallback ("browse.example") rather than any beacon/registry endpoint.
		cfg.MissChaffPct, cfg.ChatterPct, cfg.FailChaffPct = 0, 0, 0
		cfg.CohortPct, cfg.ClusterPct, cfg.DualStackPct = 0, 0, 0
		cfg.SessionCoherence, cfg.ShadowTTL = false, false

		src := &mockSource{persona: querylog.ClientPersona{IP: "10.0.0.11"}, class: querylog.ClassWorkstation, listDomain: "browse.example"}
		eng, snap := capture(src)

		for i := 0; i < 20; i++ {
			eng.emit(context.Background())
		}

		reqs := snap()
		Expect(reqs).Should(HaveLen(20))
		for _, r := range reqs {
			Expect(r.Req.Question[0].Name).Should(Equal("browse.example."))
			Expect(r.DecoySource).ShouldNot(BeElementOf(provBeacon, provServer))
		}
	})

	It("falls back to browsing when device-class shaping is disabled", func() {
		cfg.DeviceClass.Enable = false
		cfg.MissChaffPct, cfg.ChatterPct, cfg.FailChaffPct = 0, 0, 0
		cfg.CohortPct, cfg.ClusterPct, cfg.DualStackPct = 0, 0, 0
		cfg.SessionCoherence, cfg.ShadowTTL = false, false

		src := &mockSource{persona: querylog.ClientPersona{IP: "10.0.0.12"}, class: querylog.ClassIoT, listDomain: "browse.example"}
		eng, snap := capture(src)

		eng.emit(context.Background())

		Expect(snap()[0].Req.Question[0].Name).Should(Equal("browse.example."))
	})

	It("emits multiple vendor families incl. a phantom one not configured as real", func() {
		cfg.DeviceClass.VendorFamilies = []string{"solaredge"} // the only "real" vendor
		cfg.DeviceClass.PhantomDevicesPct = 20
		eng, _ := capture(&mockSource{})

		fams := map[string]bool{}
		phantom := false
		for i := 0; i < 400; i++ {
			fam := domainFamily(eng.beaconQuery().name)
			Expect(fam).ShouldNot(BeEmpty())
			fams[fam] = true
			if fam != "solaredge" {
				phantom = true
			}
		}

		Expect(len(fams)).Should(BeNumerically(">", 1), "beacons must span multiple families")
		Expect(phantom).Should(BeTrue(), "must beacon to a family the config didn't list as real")
	})

	It("raises the target curve under the enterprise preset", func() {
		at := func(profile string, h int) float64 {
			c := cfg
			c.PersonaProfile = profile
			eng := NewEngine(c, &mockSource{}, nil)

			return eng.targetCurve(time.Date(2026, 1, 1, h, 0, 0, 0, time.UTC))
		}

		homePeak := at("home", 19)      // home evening peak → 40
		entPeak := at("enterprise", 9)  // office daytime peak → 300
		entFloor := at("enterprise", 3) // 24/7 overnight floor
		homeFloor := at("home", 3)      // home pre-dawn trough

		Expect(entPeak).Should(BeNumerically(">", homePeak*2))
		Expect(entFloor).Should(BeNumerically(">", homeFloor), "enterprise floor stands in for 24/7 servers/IoT")
	})
})
