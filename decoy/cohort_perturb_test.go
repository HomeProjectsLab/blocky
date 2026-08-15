package decoy

import (
	"math/rand"
	"sort"

	"github.com/0xERR0R/blocky/config"

	"github.com/miekg/dns"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("cohort perturbation", func() {
	const jitter = 120

	// engine with the cohort-perturbation knobs on and a deterministic rng
	engine := func(seed int64) *Engine {
		return &Engine{
			rnd: rand.New(rand.NewSource(seed)), //nolint:gosec // test determinism
			cfg: config.DecoyConfig{CohortJitterMs: jitter, CohortCompanionPct: 15},
		}
	}

	// a recorded cohort: primary at delay 0, sub-resources at rising offsets
	baseCohort := func() []decoyQuery {
		return []decoyQuery{
			{name: "main.example.", qtype: dns.TypeA, source: provCohort, delayMs: 0},
			{name: "img.example.", qtype: dns.TypeA, source: provCohort, delayMs: 200},
			{name: "cdn.example.", qtype: dns.TypeA, source: provCohort, delayMs: 210},
			{name: "api.example.", qtype: dns.TypeA, source: provCohort, delayMs: 800},
		}
	}
	realDelays := map[string]int{"img.example.": 200, "cdn.example.": 210, "api.example.": 800}

	It("keeps the primary leading, stays time-ordered, and bounds the jitter", func() {
		out := engine(1).perturbCohort(baseCohort())

		Expect(out[0].name).To(Equal("main.example."), "primary (delay 0) must still lead")
		Expect(sort.SliceIsSorted(out, func(i, j int) bool { return out[i].delayMs < out[j].delayMs })).
			To(BeTrue(), "emission must follow the perturbed timeline")

		for _, q := range out {
			if base, ok := realDelays[q.name]; ok {
				Expect(q.delayMs).To(BeNumerically("~", base, jitter),
					"a real member's offset only jitters by a small bounded amount")
				Expect(q.delayMs).To(BeNumerically(">=", 1))
			}
		}
	})

	It("occasionally splices in one unrelated companion but never drops a real member", func() {
		injected := 0

		for seed := int64(0); seed < 60; seed++ {
			out := engine(seed).perturbCohort(baseCohort())

			names := map[string]bool{}
			for _, q := range out {
				names[q.name] = true
				if q.source == provCompanion {
					injected++
					Expect(q.delayMs).To(BeNumerically(">=", 1))
				}
			}

			for _, n := range []string{"main.example.", "img.example.", "cdn.example.", "api.example."} {
				Expect(names[n]).To(BeTrue(), "cohort member %s was dropped", n)
			}
		}

		Expect(injected).To(BeNumerically(">", 0), "a companion should be spliced in on some runs")
		Expect(injected).To(BeNumerically("<", 60), "but NOT every run — it is a small %")
	})

	It("does an exact 1:1 replay when jitter and companion% are zero", func() {
		e := &Engine{
			rnd: rand.New(rand.NewSource(1)), //nolint:gosec // test determinism
			cfg: config.DecoyConfig{CohortJitterMs: 0, CohortCompanionPct: 0},
		}
		out := e.perturbCohort(baseCohort())

		Expect(out).To(HaveLen(4), "no companion spliced when CohortCompanionPct=0")
		names := make([]string, len(out))
		for i, q := range out {
			names[i] = q.name
		}
		Expect(names).To(Equal([]string{"main.example.", "img.example.", "cdn.example.", "api.example."}),
			"zero jitter keeps the exact recorded order")
	})

	It("leaves a trivial single-member cohort untouched", func() {
		one := []decoyQuery{{name: "solo.example.", source: provCohort, delayMs: 0}}
		Expect(engine(1).perturbCohort(one)).To(HaveLen(1))
	})
})
