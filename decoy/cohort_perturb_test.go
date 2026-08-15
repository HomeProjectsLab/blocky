package decoy

import (
	"math/rand"
	"sort"

	"github.com/miekg/dns"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("cohort perturbation", func() {
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
		e := &Engine{rnd: rand.New(rand.NewSource(1))} //nolint:gosec // test determinism
		out := e.perturbCohort(baseCohort())

		Expect(out[0].name).To(Equal("main.example."), "primary (delay 0) must still lead")
		Expect(sort.SliceIsSorted(out, func(i, j int) bool { return out[i].delayMs < out[j].delayMs })).
			To(BeTrue(), "emission must follow the perturbed timeline")

		for _, q := range out {
			if base, ok := realDelays[q.name]; ok {
				Expect(q.delayMs).To(BeNumerically("~", base, cohortDelayJitterMs),
					"a real member's offset only jitters by a small bounded amount")
				Expect(q.delayMs).To(BeNumerically(">=", 1))
			}
		}
	})

	It("occasionally splices in one unrelated companion but never drops a real member", func() {
		injected := 0

		for seed := int64(0); seed < 60; seed++ {
			e := &Engine{rnd: rand.New(rand.NewSource(seed))} //nolint:gosec // test determinism
			out := e.perturbCohort(baseCohort())

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

	It("leaves a trivial single-member cohort untouched", func() {
		e := &Engine{rnd: rand.New(rand.NewSource(1))} //nolint:gosec // test determinism
		one := []decoyQuery{{name: "solo.example.", source: provCohort, delayMs: 0}}
		Expect(e.perturbCohort(one)).To(HaveLen(1))
	})
})
