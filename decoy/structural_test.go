//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package decoy

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// mockSource is an in-memory Source for the structural-emission mechanics, so the
// cohort/session/revisit paths get deterministic coverage without a sqlite fixture.
type mockSource struct {
	cohort     []querylog.CohortMember
	nextInSess map[string]string // primary -> successor
	seed       string            // SessionSeed result
	revisit    time.Duration
	revisitOK  bool
	blocked    map[string]bool        // IsBlockedDomain lookup
	listDomain string                 // SampleList / nextQuery fallback
	persona    querylog.ClientPersona // SampleClient result
	class      string                 // ClientClass result (device-class routing)
}

func (m *mockSource) SeedIfEmpty(io.Reader) (int, error)                 { return 0, nil }
func (m *mockSource) HourlyRealCounts() ([24]int64, error)               { return [24]int64{}, nil }
func (m *mockSource) SampleList() (string, error)                        { return m.listDomain, nil }
func (m *mockSource) SampleCorpus() (string, error)                      { return "", nil }
func (m *mockSource) SampleRecentReal(int) ([]querylog.RealQuery, error) { return nil, nil }

func (m *mockSource) SampleFingerprintForName(string) (querylog.FpSample, error) {
	return querylog.FpSample{}, nil
}

func (m *mockSource) IsBlockedDomain(d string) (bool, error)         { return m.blocked[d], nil }
func (m *mockSource) SampleCohort() ([]querylog.CohortMember, error) { return m.cohort, nil }

func (m *mockSource) NextInSession(primary string) (string, error) {
	return m.nextInSess[primary], nil
}

func (m *mockSource) SessionSeed() (string, error)                  { return m.seed, nil }
func (m *mockSource) RevisitInterval(string) (time.Duration, bool)  { return m.revisit, m.revisitOK }
func (m *mockSource) SampleClient() (querylog.ClientPersona, error) { return m.persona, nil }
func (m *mockSource) ClientClass(string) (string, error) {
	if m.class == "" {
		return querylog.ClassUnknown, nil
	}

	return m.class, nil
}

var _ = Describe("Structural emission", func() {
	var cfg config.DecoyConfig

	BeforeEach(func() {
		var e error
		cfg, e = config.WithDefaults[config.DecoyConfig]()
		Expect(e).Should(Succeed())
		cfg.Enable = true
	})

	capturing := func(src Source) (*Engine, func() []*model.Request) {
		var mu sync.Mutex
		var captured []*model.Request
		eng := NewEngine(cfg, src, func(_ context.Context, req *model.Request) (*model.Response, error) {
			mu.Lock()
			captured = append(captured, req)
			mu.Unlock()

			return &model.Response{Res: new(dns.Msg)}, nil
		})

		return eng, func() []*model.Request {
			mu.Lock()
			defer mu.Unlock()

			return append([]*model.Request(nil), captured...)
		}
	}

	Describe("recorded-cohort replay (7G)", func() {
		It("fires every recorded cohort member (blocked included) as Bypass+Decoy, primary first", func() {
			src := &mockSource{
				cohort: []querylog.CohortMember{
					{Domain: "news.example", Qtype: dns.TypeA, DelayMs: 0},
					{Domain: "tracker.example", Qtype: dns.TypeA, DelayMs: 5, Blocked: true},
					{Domain: "cdn.example", Qtype: dns.TypeAAAA, DelayMs: 10},
				},
				// tracker.example is blocked: it must STILL egress because it's a
				// recorded-cohort member (allowBlocked), proving the guard is bypassed.
				blocked: map[string]bool{"tracker.example": true},
			}

			cfg.MissChaffPct = 0
			cfg.ChatterPct = 0
			cfg.FailChaffPct = 0
			cfg.CohortPct = 100 // force the recorded-cohort path
			eng, snapshot := capturing(src)

			eng.emit(context.Background())

			// perturbCohort now jitters the sub-resource timing/order and may splice in
			// one unrelated companion, so assert the invariants rather than an exact
			// list: every real member still fires, the primary still leads, all
			// Bypass+Decoy.
			Eventually(func() int { return len(snapshot()) }, "2s", "10ms").Should(BeNumerically(">=", 3))
			time.Sleep(400 * time.Millisecond) // past the bounded burst spread (<= ~130ms)

			reqs := snapshot()
			Expect(len(reqs)).To(BeElementOf(3, 4), "3 cohort members + at most one spliced companion")

			names := map[string]bool{}
			for _, r := range reqs {
				names[r.Req.Question[0].Name] = true
				Expect(r.Bypass).Should(BeTrue())
				Expect(r.Decoy).Should(BeTrue())
			}

			Expect(reqs[0].Req.Question[0].Name).Should(Equal("news.example."), "the main document still leads")
			Expect(names).Should(HaveKey("tracker.example."), "blocked cohort member must still egress (allowBlocked)")
			Expect(names).Should(HaveKey("cdn.example."))
		})

		It("falls back to the synthetic path at cold start (empty cohort)", func() {
			src := &mockSource{listDomain: "fallback.example"} // no cohort, no session
			cfg.MissChaffPct = 0
			cfg.ChatterPct = 0
			cfg.FailChaffPct = 0
			cfg.CohortPct = 100
			cfg.SessionCoherence = false // straight to the source pick
			cfg.ClusterPct = 0
			cfg.DualStackPct = 0
			eng, snapshot := capturing(src)

			eng.emit(context.Background())

			Eventually(snapshot, "1s", "10ms").Should(HaveLen(1))
			Expect(snapshot()[0].Req.Question[0].Name).Should(Equal("fallback.example."))
		})
	})

	Describe("session walk (#1)", func() {
		It("advances via NextInSession, then reseeds when a domain has no successor", func() {
			src := &mockSource{
				nextInSess: map[string]string{"a.com": "b.com"}, // b.com has no successor
				seed:       "seed.com",
			}
			cfg.StepPct = 100 // always attempt a step when an anchor is set
			eng := NewEngine(cfg, src, nil)

			eng.setSession("a.com", 1)
			Expect(eng.walkSession()).Should(Equal("b.com")) // a.com -> b.com

			// b.com has no successor -> reseed a fresh session
			Expect(eng.walkSession()).Should(Equal("seed.com"))
		})

		It("returns empty at cold start (no successor, no seed) so emit falls back", func() {
			src := &mockSource{}
			eng := NewEngine(cfg, src, nil)
			Expect(eng.walkSession()).Should(Equal(""))
		})
	})

	Describe("revisit cadence (#5)", func() {
		It("prefers a due domain over a fresh source pick", func() {
			src := &mockSource{listDomain: "fresh.example", revisit: time.Hour, revisitOK: true}
			cfg.RevisitCadence = true
			eng := NewEngine(cfg, src, nil)

			base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
			eng.now = func() time.Time { return base }

			// due.example became due a minute ago -> reviseOrNext must emit it.
			eng.dueMap["due.example"] = base.Add(-time.Minute)
			Expect(eng.reviseOrNext().name).Should(Equal("due.example"))
		})

		It("takes a fresh pick when nothing is due, and schedules it", func() {
			src := &mockSource{listDomain: "fresh.example", revisit: time.Hour, revisitOK: true}
			cfg.RevisitCadence = true
			eng := NewEngine(cfg, src, nil)

			base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
			eng.now = func() time.Time { return base }

			// only a not-yet-due entry exists -> fall through to the source pick.
			eng.dueMap["future.example"] = base.Add(time.Hour)
			Expect(eng.reviseOrNext().name).Should(Equal("fresh.example"))
			// the fresh pick got scheduled for its learned interval.
			_, tracked := eng.dueMap["fresh.example"]
			Expect(tracked).Should(BeTrue())
		})
	})

	Describe("compensating persona cover (#8)", func() {
		feed := func(eng *Engine, n int) {
			for range n {
				eng.recordReal()
			}
		}

		It("fills the gap to the target curve, and drops to zero above it", func() {
			eng := NewEngine(cfg, &mockSource{}, nil)
			eng.cfg.PersonaCover = true
			eng.cfg.TargetQPMTrough = 6
			eng.cfg.TargetQPMPeak = 40

			// 19:00 local -> personaShape peak 1.0 -> target == TargetQPMPeak (40).
			base := time.Date(2026, 8, 13, 19, 0, 0, 0, time.UTC)
			eng.now = func() time.Time { return base }
			Expect(eng.targetCurve(base)).Should(BeNumerically("~", 40, 0.001))

			// real well below target -> decoy fills the remainder.
			feed(eng, 10) // 10 real in a 60s window == 10 QPM
			Expect(eng.effectiveQPM()).Should(BeNumerically("~", 30, 0.001))

			// real above target -> no cover needed.
			feed(eng, 50) // now 60 QPM > 40 target
			Expect(eng.effectiveQPM()).Should(Equal(0.0))
		})
	})
})
