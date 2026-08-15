package decoy

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/0xERR0R/blocky/config"
)

// perturbCohort's contract, distilled into machine-checkable invariants that must
// hold for ANY cohort shape, seed, jitter and companion%. Exercised both by a
// table of random cohorts (TestPerturbCohortInvariants) and by the fuzzer
// (FuzzPerturbCohort). A recorded cohort always has its primary (the main
// document) at index 0 with delayMs 0; buildCohort preserves that.
func buildCohort(delays []int) []decoyQuery {
	qs := []decoyQuery{{name: "m0.example.", source: provCohort, delayMs: 0}} // primary
	for i, d := range delays {
		qs = append(qs, decoyQuery{name: fmt.Sprintf("m%d.example.", i+1), source: provCohort, delayMs: d})
	}

	return qs
}

// checkInvariants asserts the four properties that must ALWAYS hold. in is the
// cohort as built (before perturbation mutates delays in place); primaryName is
// in[0].name.
func checkInvariants(t *testing.T, primaryName string, realNames map[string]bool, out []decoyQuery) {
	t.Helper()

	if len(out) == 0 {
		t.Fatalf("output is empty")
	}

	// primary (orig delay 0) leads
	if out[0].name != primaryName {
		t.Fatalf("primary %q not leading; got %q (delay %d)", primaryName, out[0].name, out[0].delayMs)
	}

	// output time-sorted (ascending delayMs)
	if !sort.SliceIsSorted(out, func(i, j int) bool { return out[i].delayMs < out[j].delayMs }) {
		t.Fatalf("output not time-sorted: %v", out)
	}

	seen := map[string]bool{}
	for _, q := range out {
		if q.source == provCompanion {
			// injected companion delay >= 1 (must trail the delay-0 primary)
			if q.delayMs < 1 {
				t.Fatalf("companion %q has delay %d < 1", q.name, q.delayMs)
			}

			continue
		}
		seen[q.name] = true
	}

	// every real member present
	for n := range realNames {
		if !seen[n] {
			t.Fatalf("real member %q was dropped", n)
		}
	}
}

func namesOf(qs []decoyQuery) map[string]bool {
	m := make(map[string]bool, len(qs))
	for _, q := range qs {
		m[q.name] = true
	}

	return m
}

// TestPerturbCohortInvariants is the property/table test: it crosses cohort
// shapes (empty, single, rising, all-equal-0, all-equal-N, random) with a spread
// of seeds and jitter/companion% knobs, and asserts the invariants hold on every
// combination. The all-equal and zero-delay rows are the panic-prone paths
// (maxDelay==0 -> rnd.Intn(0)) and the identical-delay sort paths.
func TestPerturbCohortInvariants(t *testing.T) {
	shapes := map[string]func(rng *rand.Rand) []int{
		"empty":      func(*rand.Rand) []int { return nil },
		"single-sub": func(*rand.Rand) []int { return []int{200} },
		"rising": func(rng *rand.Rand) []int {
			n := 1 + rng.Intn(40)
			out := make([]int, n)
			d := 0
			for i := range out {
				d += rng.Intn(50)
				out[i] = d
			}

			return out
		},
		"all-equal-zero": func(rng *rand.Rand) []int {
			return make([]int, 1+rng.Intn(40)) // every sub-resource at delay 0
		},
		"all-equal-n": func(rng *rand.Rand) []int {
			n := 1 + rng.Intn(40)
			out := make([]int, n)
			for i := range out {
				out[i] = 300
			}

			return out
		},
		"random": func(rng *rand.Rand) []int {
			n := rng.Intn(80)
			out := make([]int, n)
			for i := range out {
				out[i] = rng.Intn(2000)
			}

			return out
		},
	}

	knobs := []struct{ jitter, pct uint }{
		{0, 0}, {0, 100}, {120, 15}, {120, 100}, {5000, 50}, {1, 1},
	}

	for name, shape := range shapes {
		for _, k := range knobs {
			for seed := int64(0); seed < 200; seed++ {
				rng := rand.New(rand.NewSource(seed)) //nolint:gosec // test determinism
				in := buildCohort(shape(rng))
				realNames := namesOf(in)
				primary := in[0].name

				e := &Engine{
					rnd: rand.New(rand.NewSource(seed)), //nolint:gosec // test determinism
					cfg: config.DecoyConfig{CohortJitterMs: k.jitter, CohortCompanionPct: k.pct},
				}

				var out []decoyQuery
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("panic %s seed=%d jitter=%d pct=%d: %v", name, seed, k.jitter, k.pct, r)
						}
					}()
					out = e.perturbCohort(in)
				}()

				checkInvariants(t, primary, realNames, out)
			}
		}
	}
}

// FuzzPerturbCohort drives perturbCohort with fuzz-derived cohort shapes and
// knobs. Sub-resource delays come from the raw bytes (0..255, so all-equal and
// all-zero cohorts are reachable); the primary stays at delay 0. Every generated
// input must satisfy the same invariants — any panic or violation the fuzzer
// finds is a real bug. Left as a seed-corpus regression; run with
// -fuzz=FuzzPerturbCohort to actively fuzz.
func FuzzPerturbCohort(f *testing.F) {
	f.Add(int64(1), []byte{200, 210, 44}, uint(120), uint(15))
	f.Add(int64(0), []byte{}, uint(0), uint(0))
	f.Add(int64(3), []byte{0, 0, 0}, uint(0), uint(100))    // all-zero delays, always splice
	f.Add(int64(7), []byte{5, 5, 5, 5}, uint(120), uint(0)) // all-equal nonzero
	f.Add(int64(9), []byte{255}, uint(5000), uint(50))
	f.Add(int64(2), make([]byte, 100), uint(1), uint(1)) // large all-zero cohort

	f.Fuzz(func(t *testing.T, seed int64, delayBytes []byte, jitter, pct uint) {
		jitter %= 10001 // keep jitter sane; still spans 0 and large values
		pct %= 101      // config invariant: companion% in [0,100]

		delays := make([]int, len(delayBytes))
		for i, b := range delayBytes {
			delays[i] = int(b)
		}

		in := buildCohort(delays)
		realNames := namesOf(in)
		primary := in[0].name

		e := &Engine{
			rnd: rand.New(rand.NewSource(seed)), //nolint:gosec // test determinism
			cfg: config.DecoyConfig{CohortJitterMs: jitter, CohortCompanionPct: pct},
		}

		out := e.perturbCohort(in) // a panic here fails the fuzz case
		checkInvariants(t, primary, realNames, out)
	})
}
