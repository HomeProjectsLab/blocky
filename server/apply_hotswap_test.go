package server

// Config-apply hot-swap: an apply rebuilds only the resolver chain and swaps it
// atomically behind the live listeners, so :80 and :53 keep serving across the
// swap. These specs prove HTTP and DNS stay up straddling an apply, that
// blocking reflects the NEW config immediately, that a bad-config apply keeps
// the old chain serving, that repeated applies leak no goroutines/FDs, and that
// a listener-affecting change is refused for hot-swap (full-restart fallback).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xERR0R/blocky/config"
	. "github.com/0xERR0R/blocky/helpertest"
	"github.com/0xERR0R/blocky/resolver"
	"github.com/0xERR0R/blocky/util"

	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// srvResolver / setSrvResolver / newBundleServer are in-package test seams for the
// live resolver bundle, so existing specs that used to poke srv.queryResolver keep
// working after the field moved behind the atomic live pointer.
func srvResolver(s *Server) resolver.ChainedResolver {
	return s.live.Load().resolver
}

func setSrvResolver(s *Server, r resolver.ChainedResolver) {
	b := s.live.Load()
	nb := *b
	nb.resolver = r
	s.live.Store(&nb)
}

func newBundleServer(r resolver.ChainedResolver, cfg *config.Config) *Server {
	s := &Server{}
	s.live.Store(&resolverBundle{resolver: r, cfg: cfg})

	return s
}

const hotswapDNSPort = 57500

// recordingFlusher is a logFlushers stand-in that counts Flush calls, for the
// retired-bundle drain spec.
type recordingFlusher struct{ calls *int32 }

func (f recordingFlusher) Flush() error {
	atomic.AddInt32(f.calls, 1)

	return nil
}

var _ = Describe("Config apply hot-swap", func() {
	var (
		ctx         context.Context
		cancelFn    context.CancelFunc
		upstreamCfg config.Upstream
	)

	// upstream answers every A query with 9.9.9.9; a blocked domain instead
	// yields 0.0.0.0 (BlockType zeroIp), so the two are trivially distinguishable.
	// Started ONCE per spec so repeated applies don't spawn a new mock per config
	// (which would swamp the goroutine-leak spec's baseline).
	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		upstream := resolver.NewMockUDPUpstreamServer().WithAnswerFn(func(request *dns.Msg) *dns.Msg {
			resp, err := util.NewMsgWithAnswer(request.Question[0].Name, 123, A, "9.9.9.9")
			Expect(err).Should(Succeed())

			return resp
		})
		upstreamCfg = upstream.Start()
		DeferCleanup(upstream.Close)
	})

	buildCfg := func(blocked ...string) *config.Config {
		denylist := TempFile(strings.Join(blocked, "\n") + "\n")
		DeferCleanup(func() { _ = denylist.Close() })

		return &config.Config{
			Upstreams: config.Upstreams{
				Timeout: config.Duration(2 * time.Second),
				Groups:  map[string][]config.Upstream{"default": {upstreamCfg}},
			},
			Blocking: config.Blocking{
				Denylists:         map[string][]config.BytesSource{"ads": config.NewBytesSources(denylist.Name())},
				ClientGroupsBlock: map[string][]string{"default": {"ads"}},
				BlockType:         "zeroIp",
				BlockTTL:          config.Duration(time.Minute),
			},
			Ports: config.Ports{
				DNS:     config.ListenConfig{GetHostPort("127.0.0.1", hotswapDNSPort)},
				HTTP:    config.ListenConfig{"127.0.0.1:0"},
				DOHPath: "/dns-query",
			},
		}
	}

	startServer := func(cfg *config.Config) *Server {
		srv, cErr := NewServer(ctx, cfg, nil)
		Expect(cErr).Should(Succeed())

		errChan := make(chan error, 10)
		go srv.Start(ctx, errChan)
		DeferCleanup(func() { _ = srv.Stop(ctx) })
		Consistently(errChan, "200ms").ShouldNot(Receive())

		return srv
	}

	// resolveA queries the live UDP listener and returns the first A record's IP.
	resolveA := func(name string) string {
		c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
		msg := util.NewMsgWithQuestion(name, A)

		var resp *dns.Msg

		Eventually(ctx, func() error {
			var err error
			resp, _, err = c.ExchangeContext(ctx, msg, GetHostPort("127.0.0.1", hotswapDNSPort))

			return err
		}, "2s", "20ms").Should(Succeed())

		Expect(resp.Answer).ShouldNot(BeEmpty(), "no answer for %s", name)

		return resp.Answer[0].(*dns.A).A.String()
	}

	It("keeps the HTTP server answering on the same connection across an apply", func() {
		srv := startServer(buildCfg("blocked.example"))
		addr := firstHTTPListenerAddr(srv)
		listenerBefore := firstHTTPListenerAddr(srv)

		// default transport keeps connections alive; httptrace records the local
		// socket so we can prove the SECOND request reused the FIRST connection
		client := &http.Client{}
		var localAddrs []string

		trace := &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				localAddrs = append(localAddrs, info.Conn.LocalAddr().String())
			},
		}

		call := func() int {
			req, _ := http.NewRequestWithContext(
				httptrace.WithClientTrace(ctx, trace), http.MethodGet,
				fmt.Sprintf("http://%s/api/blocking/status", addr), nil)
			resp, err := client.Do(req)
			Expect(err).Should(Succeed())
			// body must be drained+closed for the keep-alive connection to be reused
			_, _ = resp.Body.Read(make([]byte, 4096))
			_ = resp.Body.Close()

			return resp.StatusCode
		}

		Expect(call()).Should(Equal(http.StatusOK))

		Expect(srv.ApplyConfig(ctx, buildCfg())).Should(Succeed())

		Expect(call()).Should(Equal(http.StatusOK))

		Expect(localAddrs).Should(HaveLen(2))
		Expect(localAddrs[0]).Should(Equal(localAddrs[1]),
			"apply dropped the HTTP connection; same keep-alive socket should have carried both requests")
		Expect(firstHTTPListenerAddr(srv)).Should(Equal(listenerBefore),
			"apply must not rebind the HTTP listener")
	})

	It("keeps DNS answering continuously across an apply", func() {
		srv := startServer(buildCfg("blocked.example"))

		// warm up
		Expect(resolveA("resolvable.example.")).Should(Equal("9.9.9.9"))

		var (
			failures atomic.Int64
			polls    atomic.Int64
			stop     atomic.Bool
			wg       sync.WaitGroup
		)

		wg.Add(1)
		go func() {
			defer wg.Done()

			c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
			msg := util.NewMsgWithQuestion("resolvable.example.", A)

			for !stop.Load() {
				polls.Add(1)
				resp, _, err := c.ExchangeContext(ctx, msg, GetHostPort("127.0.0.1", hotswapDNSPort))
				if err != nil || resp == nil || len(resp.Answer) == 0 {
					failures.Add(1)
				}

				time.Sleep(5 * time.Millisecond)
			}
		}()

		// let the poller run, apply in the middle, keep polling past the swap
		time.Sleep(100 * time.Millisecond)
		Expect(srv.ApplyConfig(ctx, buildCfg("other.example"))).Should(Succeed())
		time.Sleep(100 * time.Millisecond)
		stop.Store(true)
		wg.Wait()

		Expect(polls.Load()).Should(BeNumerically(">", 10), "poller should have run across the swap")
		Expect(failures.Load()).Should(BeZero(), "DNS dropped queries during the apply")
	})

	It("reflects the new blocking config immediately after an apply", func() {
		srv := startServer(buildCfg("ads.example"))

		// A blocks ads.example, resolves safe.example
		Expect(resolveA("ads.example.")).Should(Equal("0.0.0.0"))
		Expect(resolveA("safe.example.")).Should(Equal("9.9.9.9"))

		// B blocks safe.example, unblocks ads.example — the OpenAPI/blocking getters
		// pick up the new chain, so the very next query sees the swapped rules
		Expect(srv.ApplyConfig(ctx, buildCfg("safe.example"))).Should(Succeed())

		Expect(resolveA("ads.example.")).Should(Equal("9.9.9.9"))
		Expect(resolveA("safe.example.")).Should(Equal("0.0.0.0"))
	})

	It("keeps the old chain serving when an apply fails to build", func() {
		srv := startServer(buildCfg("ads.example"))
		Expect(resolveA("ads.example.")).Should(Equal("0.0.0.0"))

		before := srv.live.Load()

		// a required-but-unreachable Redis makes buildResolverBundle fail
		bad := buildCfg()
		bad.Redis.Address = "test-fail"
		bad.Redis.Required = true

		Expect(srv.ApplyConfig(ctx, bad)).Should(HaveOccurred())

		Expect(srv.live.Load()).Should(BeIdenticalTo(before), "live bundle must be unchanged after a failed apply")
		// old behavior still served: ads.example still blocked, DNS + HTTP still up
		Expect(resolveA("ads.example.")).Should(Equal("0.0.0.0"))

		resp, err := http.Get(fmt.Sprintf("http://%s/api/blocking/status", firstHTTPListenerAddr(srv)))
		Expect(err).Should(Succeed())
		DeferCleanup(resp.Body.Close)
		Expect(resp.StatusCode).Should(Equal(http.StatusOK))
	})

	It("leaks no goroutines across many applies", func() {
		// shorten the retire grace so retired bundles are fully reaped within the test
		old := retireGrace
		retireGrace = 20 * time.Millisecond
		DeferCleanup(func() { retireGrace = old })

		srv := startServer(buildCfg("ads.example"))
		Expect(resolveA("ads.example.")).Should(Equal("0.0.0.0"))

		time.Sleep(100 * time.Millisecond)
		runtime.GC()
		baseline := runtime.NumGoroutine()

		for i := 0; i < 50; i++ {
			Expect(srv.ApplyConfig(ctx, buildCfg("ads.example"))).Should(Succeed())
		}

		Expect(resolveA("ads.example.")).Should(Equal("0.0.0.0"))

		// wait past the grace so every retired bundle's cancel + delayed close ran
		Eventually(func() int {
			runtime.GC()

			return runtime.NumGoroutine()
		}, "5s", "100ms").Should(BeNumerically("<", baseline+20),
			"goroutines should return near baseline after retired bundles are reaped")
	})

	It("leaks no sqlite query-log DB connections across many applies", func() {
		// shorten the retire grace so retired bundles' DB conns close within the test
		old := retireGrace
		retireGrace = 20 * time.Millisecond
		DeferCleanup(func() { retireGrace = old })

		dbFile := TempFile("")
		DeferCleanup(func() { _ = dbFile.Close() })
		dbPath := dbFile.Name()

		cfg := buildCfg("ads.example")
		cfg.QueryLog = config.QueryLog{
			Type:             config.QueryLogTypeSqlite,
			Target:           config.Secret(dbPath),
			CreationAttempts: 1,
			CreationCooldown: config.Duration(time.Millisecond),
			FlushInterval:    config.Duration(50 * time.Millisecond),
		}

		srv := startServer(cfg)
		Expect(resolveA("ads.example.")).Should(Equal("0.0.0.0"))

		// count of open FDs the process holds against the sqlite file (main + wal/shm)
		dbFDs := func() int {
			entries, err := os.ReadDir("/proc/self/fd")
			Expect(err).Should(Succeed())

			n := 0
			for _, e := range entries {
				if target, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil &&
					strings.HasPrefix(target, dbPath) {
					n++
				}
			}

			return n
		}

		// warm up to steady state first: the -wal/-shm files only exist once the db
		// has been written, and the persistent RO readers then map them too, so an
		// FD count taken before any write undercounts. A few applies + a query reach
		// the steady state we then hold constant across many more applies.
		for i := 0; i < 3; i++ {
			Expect(srv.ApplyConfig(ctx, cfg)).Should(Succeed())
		}
		Expect(resolveA("ads.example.")).Should(Equal("0.0.0.0"))

		var baseline int
		Eventually(func() int { baseline = dbFDs(); return baseline }, "2s", "100ms").
			Should(BeNumerically(">", 0))

		for i := 0; i < 30; i++ {
			Expect(srv.ApplyConfig(ctx, cfg)).Should(Succeed())
		}

		// past the grace, every retired bundle's DB conn must be closed again; the
		// count returns to steady state and does NOT scale with the 30 applies
		// (each leaked conn would add ~3 FDs — main + wal + shm)
		Eventually(dbFDs, "5s", "100ms").Should(BeNumerically("<=", baseline+3),
			"sqlite DB connections should not accumulate across applies")
	})

	It("flushes bundles retired within retireGrace on Stop (no lost query-log entries)", func() {
		var liveCalls, retiredCalls int32

		s := &Server{}
		s.live.Store(&resolverBundle{
			logFlushers: []interface{ Flush() error }{recordingFlusher{&liveCalls}},
		})

		// a bundle retired less than retireGrace ago: its delayed AfterFunc flush
		// hasn't fired yet, and process-exit would drop that timer
		retired := &resolverBundle{
			logFlushers: []interface{ Flush() error }{recordingFlusher{&retiredCalls}},
		}
		s.retired = map[*resolverBundle]struct{}{retired: {}}

		Expect(s.Stop(context.Background())).Should(Succeed())

		Expect(atomic.LoadInt32(&liveCalls)).Should(BeNumerically("==", 1), "live bundle flushed on Stop")
		Expect(atomic.LoadInt32(&retiredCalls)).Should(BeNumerically("==", 1),
			"a bundle retired within retireGrace must be flushed on Stop, not dropped with its AfterFunc")
	})

	It("refuses a listener-affecting change for hot-swap", func() {
		a := buildCfg("ads.example")
		b := buildCfg("ads.example")
		b.Ports.HTTP = config.ListenConfig{"127.0.0.1:12345"}

		Expect(ListenersCompatible(a, b)).Should(BeFalse(),
			"a ports change must force the full-restart fallback")

		// a pure resolver-config change stays hot-swappable
		Expect(ListenersCompatible(a, buildCfg("other.example"))).Should(BeTrue())
	})

	It("refuses a prometheus change for hot-swap (the router is built once)", func() {
		a := buildCfg("ads.example")

		enabled := buildCfg("ads.example")
		enabled.Prometheus.Enable = true
		Expect(ListenersCompatible(a, enabled)).Should(BeFalse(),
			"toggling prometheus must force the full restart that rebuilds the router")

		pathChanged := buildCfg("ads.example")
		pathChanged.Prometheus.Path = "/other-metrics"
		Expect(ListenersCompatible(a, pathChanged)).Should(BeFalse(),
			"a prometheus path change must force the full restart")
	})
})
