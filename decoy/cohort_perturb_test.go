package decoy

import (
	"fmt"
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

	// --- added hardening: perturbCohort / emitCohort edge cases ---

	It("survives an all-zero-delay cohort with companions on (no rnd.Intn(0) panic)", func() {
		// every member recorded at offset 0 -> maxDelay 0. This is the exact path the
		// max(maxDelay,1) guard protects: without it, 1+rnd.Intn(0) would panic.
		// jitter 0 + companion 100% force the splice through the maxDelay==0 branch.
		e := &Engine{
			rnd: rand.New(rand.NewSource(3)), //nolint:gosec // test determinism
			cfg: config.DecoyConfig{CohortJitterMs: 0, CohortCompanionPct: 100},
		}
		flat := []decoyQuery{
			{name: "main.example.", source: provCohort, delayMs: 0},
			{name: "a.example.", source: provCohort, delayMs: 0},
			{name: "b.example.", source: provCohort, delayMs: 0},
		}

		var out []decoyQuery
		Expect(func() { out = e.perturbCohort(flat) }).ToNot(Panic())

		Expect(out).To(HaveLen(4), "one companion spliced at 100%")
		Expect(out[0].name).To(Equal("main.example."), "a real zero-delay member still leads")
		for _, q := range out {
			if q.source == provCompanion {
				Expect(q.delayMs).To(Equal(1), "companion delay == 1 + Intn(max(0,1)) == 1, so it trails the primary")
			}
		}
	})

	It("keeps the primary leading and stays time-sorted for a large cohort across many seeds", func() {
		large := func() []decoyQuery {
			qs := []decoyQuery{{name: "primary.example.", source: provCohort, delayMs: 0}}
			for i := 1; i <= 60; i++ {
				qs = append(qs, decoyQuery{name: fmt.Sprintf("sub%02d.example.", i), source: provCohort, delayMs: i * 10})
			}

			return qs
		}

		for seed := int64(0); seed < 40; seed++ {
			out := engine(seed).perturbCohort(large())

			Expect(out[0].name).To(Equal("primary.example."), "primary (delay 0) must lead, seed %d", seed)
			Expect(sort.SliceIsSorted(out, func(i, j int) bool { return out[i].delayMs < out[j].delayMs })).
				To(BeTrue(), "emission stays time-ordered, seed %d", seed)

			real := 0
			for _, q := range out {
				if q.source == provCohort {
					real++
				}
			}
			Expect(real).To(Equal(61), "no recorded member dropped, seed %d", seed)
		}
	})

	It("keeps identical-delay sub-resources sorted with the primary leading", func() {
		same := func() []decoyQuery {
			qs := []decoyQuery{{name: "primary.example.", source: provCohort, delayMs: 0}}
			for i := 0; i < 10; i++ {
				qs = append(qs, decoyQuery{name: fmt.Sprintf("r%d.example.", i), source: provCohort, delayMs: 100})
			}

			return qs
		}

		for seed := int64(0); seed < 30; seed++ {
			out := engine(seed).perturbCohort(same())

			Expect(out[0].name).To(Equal("primary.example."), "seed %d", seed)
			Expect(sort.SliceIsSorted(out, func(i, j int) bool { return out[i].delayMs < out[j].delayMs })).
				To(BeTrue(), "seed %d", seed)

			for _, q := range out {
				if q.source == provCohort && q.name != "primary.example." {
					Expect(q.delayMs).To(BeNumerically("~", 100, jitter), "identical members jitter within bound")
					Expect(q.delayMs).To(BeNumerically(">=", 1))
				}
			}
		}
	})

	It("bounds every jittered offset within ±CohortJitterMs and actually spans the range", func() {
		const base = 500 // well above jitter so the max(...,1) floor never truncates the bound
		minSeen, maxSeen := base, base

		for seed := int64(0); seed < 500; seed++ {
			e := &Engine{
				rnd: rand.New(rand.NewSource(seed)), //nolint:gosec // test determinism
				cfg: config.DecoyConfig{CohortJitterMs: jitter, CohortCompanionPct: 0},
			}
			out := e.perturbCohort([]decoyQuery{
				{name: "p.example.", source: provCohort, delayMs: 0},
				{name: "s.example.", source: provCohort, delayMs: base},
			})

			for _, q := range out {
				if q.name == "s.example." {
					Expect(q.delayMs).To(BeNumerically(">=", base-jitter), "seed %d underflows the bound", seed)
					Expect(q.delayMs).To(BeNumerically("<=", base+jitter), "seed %d overflows the bound", seed)
					minSeen = min(minSeen, q.delayMs)
					maxSeen = max(maxSeen, q.delayMs)
				}
			}
		}

		Expect(maxSeen-minSeen).To(BeNumerically(">", jitter), "jitter must span a meaningful range, not a constant")
	})

	It("splices a companion at roughly CohortCompanionPct of replays", func() {
		const pct = 15
		const runs = 4000
		injected := 0

		for seed := int64(0); seed < runs; seed++ {
			e := &Engine{
				rnd: rand.New(rand.NewSource(seed)), //nolint:gosec // test determinism
				cfg: config.DecoyConfig{CohortJitterMs: 0, CohortCompanionPct: pct},
			}
			out := e.perturbCohort(baseCohort())

			for _, q := range out {
				if q.source == provCompanion {
					injected++
					Expect(q.delayMs).To(BeNumerically(">=", 1), "companion must trail the primary")
				}
			}
		}

		rate := float64(injected) / float64(runs) * 100
		Expect(rate).To(BeNumerically("~", pct, 3), "injection rate ~ configured %% (got %.1f%%)", rate)
	})

	It("with jitter 0 leaves recorded offsets untouched even while splicing a companion", func() {
		// companion 100% always fires (Intn(100) < 100); jitter 0 must not move any
		// recorded member — it stays an exact replay apart from the one splice.
		e := &Engine{
			rnd: rand.New(rand.NewSource(7)), //nolint:gosec // test determinism
			cfg: config.DecoyConfig{CohortJitterMs: 0, CohortCompanionPct: 100},
		}
		out := e.perturbCohort(baseCohort())

		Expect(out).To(HaveLen(5), "4 members + 1 companion")
		Expect(out[0].name).To(Equal("main.example."), "primary still leads")
		for _, q := range out {
			if b, ok := realDelays[q.name]; ok {
				Expect(q.delayMs).To(Equal(b), "jitter 0 must not move recorded member %s", q.name)
			}
		}
	})
})
