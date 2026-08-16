//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package decoy

import (
	"context"
	"sync"

	"github.com/miekg/dns"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// countingSource counts SampleClient calls so the fill-attempt cap is observable.
type countingSource struct {
	mockSource

	mu    sync.Mutex
	calls int
}

func (c *countingSource) SampleClient() (querylog.ClientPersona, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()

	return c.persona, nil
}

func (c *countingSource) sampleCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls
}

var _ = Describe("Persona pool fill", func() {
	var cfg config.DecoyConfig

	BeforeEach(func() {
		var e error
		cfg, e = config.WithDefaults[config.DecoyConfig]()
		Expect(e).Should(Succeed())
		cfg.Enable = true
		cfg.PersonaAttribution = true
	})

	It("caps pool-fill sampling by attempts for a household smaller than the pool", func() {
		// One distinct device key: the pool can never reach personaPoolSize, so
		// without the attempt cap every pickPersona would sample the DB forever.
		src := &countingSource{mockSource: mockSource{
			persona: querylog.ClientPersona{IP: "192.168.1.5", Key: "k1"},
		}}
		eng := NewEngine(cfg, src, func(_ context.Context, _ *model.Request) (*model.Response, error) {
			return &model.Response{Res: new(dns.Msg)}, nil
		})

		for range personaFillAttempts + 20 {
			_, ok := eng.pickPersona()
			Expect(ok).Should(BeTrue())
		}

		Expect(src.sampleCalls()).Should(Equal(personaFillAttempts))
	})

	It("skips the pool sample entirely for a routed emission (q.persona set)", func() {
		src := &countingSource{mockSource: mockSource{
			persona: querylog.ClientPersona{IP: "192.168.1.5", Key: "k1"},
		}}

		var mu sync.Mutex

		var captured []*model.Request

		eng := NewEngine(cfg, src, func(_ context.Context, req *model.Request) (*model.Response, error) {
			mu.Lock()
			captured = append(captured, req)
			mu.Unlock()

			return &model.Response{Res: new(dns.Msg)}, nil
		})

		routed := &querylog.ClientPersona{IP: "192.168.1.9", Key: "k9"}
		eng.resolveOne(context.Background(), decoyQuery{name: "x.example", qtype: dns.TypeA, persona: routed})

		Expect(src.sampleCalls()).Should(BeZero(), "routed emission must not sample the pool")

		mu.Lock()
		defer mu.Unlock()
		Expect(captured).ShouldNot(BeEmpty())
		Expect(captured[0].ClientIP.String()).Should(Equal("192.168.1.9"))
	})
})
