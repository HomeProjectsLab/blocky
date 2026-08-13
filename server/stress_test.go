package server

// Concurrency stress harness. Gated behind BLOCKY_STRESS=1 so normal `go test`
// (and CI) skips it. It boots a REAL server (fresh --db-dir, sqlite query log,
// decoy engine at a high fixed rate, blocking seeded from the embedded floor,
// forwarding to an in-process mock upstream so we're CPU/DB-bound not network-
// bound) and then, for a sustained window, concurrently:
//
//  1. floods DNS from many clients over UDP+TCP with mixed qtypes;
//  2. hammers the /api/ui stats/noise/queries endpoints and holds SSE streams;
//  3. rebuilds the whole server (supervisor-style Stop+NewServer) under load;
//  4. swaps a group's upstream entry at runtime under load.
//
// It measures DNS success/servfail/error, latency percentiles, throughput,
// goroutine + heap growth across rebuilds, FD growth, and counts SQLite
// "database is locked" / SQLITE_BUSY log events (the prime suspect).
//
//	Run: BLOCKY_STRESS=1 go test ./server/ -run TestStress -timeout 20m -v
//	Knobs (env): BLOCKY_STRESS_DURATION=90s BLOCKY_STRESS_CLIENTS=64
//	             BLOCKY_STRESS_HTTP=16 BLOCKY_STRESS_SSE=8
//	             BLOCKY_STRESS_REBUILD=8s BLOCKY_STRESS_QPM=6000

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
)

// lockHook counts logrus records that report a SQLite lock/busy condition, from
// any component (writer flush, decoy source, aggregates, updater, prewarm).
type lockHook struct{ locked, busy atomic.Int64 }

func (h *lockHook) Levels() []logrus.Level { return logrus.AllLevels }

func (h *lockHook) Fire(e *logrus.Entry) error {
	m := e.Message
	if e.Data != nil {
		if err, ok := e.Data[logrus.ErrorKey].(error); ok && err != nil {
			m += " " + err.Error()
		}
	}

	if containsFold(m, "database is locked") || containsFold(m, "database table is locked") {
		h.locked.Add(1)
	}

	if containsFold(m, "SQLITE_BUSY") || containsFold(m, "database is busy") {
		h.busy.Add(1)
	}

	return nil
}

func containsFold(s, sub string) bool {
	// tiny case-insensitive contains without importing strings for one call site
	ls, lsub := []byte(s), []byte(sub)
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 32
		}
		return b
	}
	if len(lsub) == 0 {
		return true
	}
outer:
	for i := 0; i+len(lsub) <= len(ls); i++ {
		for j := 0; j < len(lsub); j++ {
			if lower(ls[i+j]) != lower(lsub[j]) {
				continue outer
			}
		}
		return true
	}
	return false
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func freePort(tb testing.TB) int {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("free port: %v", err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

// reservoir is a bounded, lock-protected latency sample (random replacement) so
// a multi-million-query run keeps O(cap) memory while still giving percentiles.
type reservoir struct {
	mu   sync.Mutex
	buf  []float64 // milliseconds
	seen int64
	cap  int
	rnd  *rand.Rand
}

func newReservoir(capacity int) *reservoir {
	return &reservoir{buf: make([]float64, 0, capacity), cap: capacity, rnd: rand.New(rand.NewSource(1))}
}

func (r *reservoir) add(ms float64) {
	r.mu.Lock()
	r.seen++
	if len(r.buf) < r.cap {
		r.buf = append(r.buf, ms)
	} else if j := r.rnd.Int63n(r.seen); j < int64(r.cap) {
		r.buf[j] = ms
	}
	r.mu.Unlock()
}

func (r *reservoir) percentiles() (p50, p95, p99 float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		return 0, 0, 0
	}
	s := append([]float64(nil), r.buf...)
	sort.Float64s(s)
	at := func(p float64) float64 {
		i := int(p / 100 * float64(len(s)))
		if i >= len(s) {
			i = len(s) - 1
		}
		return s[i]
	}
	return at(50), at(95), at(99)
}

func fdCount() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

// stressCounters are the global DNS outcome tallies.
type stressCounters struct {
	ok, servfail, other, dialErr atomic.Int64
}

func TestStress(t *testing.T) {
	if os.Getenv("BLOCKY_STRESS") != "1" {
		t.Skip("set BLOCKY_STRESS=1 to run the concurrency stress harness")
	}

	duration := envDur("BLOCKY_STRESS_DURATION", 60*time.Second)
	nClients := envInt("BLOCKY_STRESS_CLIENTS", 48)
	nHTTP := envInt("BLOCKY_STRESS_HTTP", 12)
	nSSE := envInt("BLOCKY_STRESS_SSE", 6)
	rebuildEvery := envDur("BLOCKY_STRESS_REBUILD", 8*time.Second)
	swapEvery := envDur("BLOCKY_STRESS_SWAP", 3*time.Second)
	qpm := float64(envInt("BLOCKY_STRESS_QPM", 6000))

	hook := &lockHook{}
	log.Log().AddHook(hook)
	log.Log().SetLevel(logrus.WarnLevel) // keep the flood quiet but capture warns/errors

	tmp := t.TempDir()
	dbPath := tmp + "/querylog.db"

	// mock upstream: instant answer for anything (no network), UDP+TCP.
	mockUp, stopMock := startMockUpstream(t)
	t.Cleanup(stopMock)

	dnsPort := freePort(t)
	httpPort := freePort(t)
	dnsAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(dnsPort))
	httpAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(httpPort))

	buildCfg := func() *config.Config {
		cfg := &config.Config{
			Upstreams: config.Upstreams{
				Timeout: config.Duration(2 * time.Second),
				Groups:  map[string][]config.Upstream{"default": {mockUp}},
			},
			QueryLog: config.QueryLog{
				Type:          config.QueryLogTypeSqlite,
				Target:        config.Secret(dbPath),
				FlushInterval: config.Duration(1 * time.Second), // frequent flush => max writer contention
			},
			Blocking: config.Blocking{
				Denylists: map[string][]config.BytesSource{
					"ads":      config.NewBytesSources("blocklist:ads"),
					"tracking": config.NewBytesSources("blocklist:tracking"),
				},
				ClientGroupsBlock: map[string][]string{"default": {"ads", "tracking"}},
				BlockType:         "zeroIp",
				BlockTTL:          config.Duration(6 * time.Hour),
			},
			Ports: config.Ports{
				DNS:     config.ListenConfig{dnsAddr},
				HTTP:    config.ListenConfig{httpAddr},
				DOHPath: "/dns-query",
			},
		}
		// list updater ON so the seeder + updater goroutine + prewarm all run;
		// point the network sources at a dead port so checkAll fails instantly
		// instead of pulling megabytes (keeps the run CPU/DB-bound).
		cfg.Lists.Updater.Enable = true
		cfg.Lists.Updater.IntervalHours = 168
		cfg.Lists.Updater.TrancoURL = "http://127.0.0.1:1"
		cfg.Lists.Updater.BlocklistRepo = "invalid/invalid"

		d := &cfg.Privacy.Decoy
		d.Enable = true
		d.QueriesPerMinute = qpm
		d.PersonaCover = false   // use the fixed QPM directly, don't subtract real QPS
		d.ReactiveVolume = false // ditto: keep a steady high decoy rate
		d.DiurnalShaping = false
		d.ActiveHoursStart = 0
		d.ActiveHoursEnd = 24
		d.ReplayWeight = 15
		d.CorpusWeight = 5
		d.ListWeight = 1
		d.CompanionPct = 40
		d.CohortPct = 55
		d.ChatterPct = 15
		d.PrewarmEnable = true
		d.PrewarmIntervalHours = 12
		return cfg
	}

	ctxAll, cancelAll := context.WithCancel(context.Background())
	defer cancelAll()

	// --- server generation management (rebuild loop owns lifecycle) ----------
	type gen struct {
		srv    *Server
		cancel context.CancelFunc
	}
	var (
		curMu   sync.RWMutex
		cur     *gen
		rebuilt atomic.Int64
	)

	start := func() (*gen, error) {
		srvCtx, cancel := context.WithCancel(context.Background())
		srv, err := NewServer(srvCtx, buildCfg(), nil)
		if err != nil {
			cancel()
			return nil, err
		}
		errCh := make(chan error, 16)
		srv.Start(srvCtx, errCh)
		go func() {
			for e := range errCh {
				if e != nil {
					t.Logf("server errCh: %v", e)
				}
			}
		}()
		return &gen{srv: srv, cancel: cancel}, nil
	}

	stop := func(g *gen) {
		g.cancel() // shuts HTTP servers (ctx-driven)
		sctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		_ = g.srv.Stop(sctx)
		c()
	}

	setCur := func(g *gen) {
		curMu.Lock()
		cur = g
		curMu.Unlock()
	}
	getCur := func() *gen {
		curMu.RLock()
		defer curMu.RUnlock()
		return cur
	}

	g0, err := start()
	if err != nil {
		t.Fatalf("initial NewServer: %v", err)
	}
	setCur(g0)
	// wait for the first generation to bind + seed
	waitReady(t, dnsAddr, 60*time.Second)

	var wg sync.WaitGroup
	cnt := &stressCounters{}
	lat := newReservoir(50_000)

	qtypes := []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeHTTPS, dns.TypeTXT, dns.TypeMX}
	names := makeNames(200)

	// --- DNS flood -----------------------------------------------------------
	for i := 0; i < nClients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(id) + 1))
			udpC := &dns.Client{Net: "udp", Timeout: 3 * time.Second}
			tcpC := &dns.Client{Net: "tcp", Timeout: 3 * time.Second}
			for ctxAll.Err() == nil {
				m := new(dns.Msg)
				m.SetQuestion(names[rnd.Intn(len(names))], qtypes[rnd.Intn(len(qtypes))])
				m.RecursionDesired = true
				c := udpC
				if rnd.Intn(4) == 0 {
					c = tcpC // ~25% TCP
				}
				t0 := time.Now()
				resp, _, err := c.Exchange(m, dnsAddr)
				el := time.Since(t0)
				if err != nil {
					cnt.dialErr.Add(1)
					time.Sleep(25 * time.Millisecond) // don't spin during rebuild downtime
					continue
				}
				lat.add(float64(el.Microseconds()) / 1000.0)
				switch resp.Rcode {
				case dns.RcodeSuccess:
					cnt.ok.Add(1)
				case dns.RcodeServerFailure:
					cnt.servfail.Add(1)
				default:
					cnt.other.Add(1)
				}
			}
		}(i)
	}

	// --- HTTP API hammer -----------------------------------------------------
	apiPaths := []string{
		"/api/ui/stats/overview", "/api/ui/stats/buckets", "/api/ui/stats/top?col=question_name",
		"/api/ui/stats/latency", "/api/ui/noise/overview", "/api/ui/noise/buckets",
		"/api/ui/noise/top", "/api/ui/noise/sourcemix", "/api/ui/queries?limit=50",
		"/api/ui/system", "/api/ui/clients",
	}
	var apiOK, apiErr atomic.Int64
	for i := 0; i < nHTTP; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(id) + 100))
			hc := &http.Client{Timeout: 5 * time.Second}
			for ctxAll.Err() == nil {
				p := apiPaths[rnd.Intn(len(apiPaths))]
				resp, err := hc.Get("http://" + httpAddr + p)
				if err != nil {
					apiErr.Add(1)
					time.Sleep(25 * time.Millisecond) // don't spin during rebuild downtime
					continue
				}
				_, _ = readAllDiscard(resp)
				_ = resp.Body.Close()
				if resp.StatusCode >= 500 && resp.StatusCode != 503 {
					apiErr.Add(1)
				} else {
					apiOK.Add(1)
				}
			}
		}(i)
	}

	// --- SSE subscribers (held open, reconnect after rebuild kills them) -----
	for i := 0; i < nSSE; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctxAll.Err() == nil {
				req, _ := http.NewRequestWithContext(ctxAll, http.MethodGet, "http://"+httpAddr+"/api/ui/stream", nil)
				hc := &http.Client{Timeout: 0}
				resp, err := hc.Do(req)
				if err != nil {
					time.Sleep(200 * time.Millisecond)
					continue
				}
				// blocking reads; the request carries ctxAll, and a rebuild closes
				// the underlying conn — either way Read returns and we reconnect.
				buf := make([]byte, 4096)
				for {
					if _, err := resp.Body.Read(buf); err != nil {
						break
					}
				}
				_ = resp.Body.Close()
			}
		}()
	}

	// --- rebuild loop (supervisor-style apply under load) --------------------
	var downtimeNs atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(rebuildEvery)
		defer tk.Stop()
		for {
			select {
			case <-ctxAll.Done():
				return
			case <-tk.C:
				// supervisor discipline: stop the old generation first (releases the
				// DNS/HTTP ports), then bind the new one. This is a real apply's brief
				// downtime window; DNS workers see dial errors across it.
				old := getCur()
				t0 := time.Now()
				stop(old)
				tStop := time.Since(t0)
				ng, err := start()
				if err != nil {
					t.Logf("rebuild NewServer failed: %v", err)
					continue
				}
				tStart := time.Since(t0) - tStop
				setCur(ng)
				downtimeNs.Add(int64(time.Since(t0)))
				rebuilt.Add(1)
				t.Logf("rebuild #%d: stop=%s start(NewServer+Start)=%s", rebuilt.Load(), tStop.Round(time.Millisecond), tStart.Round(time.Millisecond))
			}
		}
	}()

	// --- runtime upstream swap loop ------------------------------------------
	var swaps, swapErr atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(swapEvery)
		defer tk.Stop()
		for {
			select {
			case <-ctxAll.Done():
				return
			case <-tk.C:
				g := getCur()
				if g == nil {
					continue
				}
				sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
				err := g.srv.SwapUpstreams(sctx, "default", []config.Upstream{mockUp})
				c()
				if err != nil {
					swapErr.Add(1)
				} else {
					swaps.Add(1)
				}
			}
		}
	}()

	// --- sampler: goroutines + heap + FDs over time --------------------------
	type sample struct {
		t         time.Duration
		goroutine int
		heapMB    float64
		fds       int
		rebuilds  int64
	}
	var samples []sample
	sampleStart := time.Now()
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(2 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctxAll.Done():
				return
			case <-tk.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				samples = append(samples, sample{
					t:         time.Since(sampleStart).Round(time.Second),
					goroutine: runtime.NumGoroutine(),
					heapMB:    float64(ms.HeapAlloc) / (1024 * 1024),
					fds:       fdCount(),
					rebuilds:  rebuilt.Load(),
				})
			}
		}
	}()

	t.Logf("stress running: %s, clients=%d http=%d sse=%d rebuild=%s swap=%s qpm=%.0f db=%s",
		duration, nClients, nHTTP, nSSE, rebuildEvery, swapEvery, qpm, dbPath)

	time.Sleep(duration)
	cancelAll()
	wg.Wait()

	// stop the final generation
	if g := getCur(); g != nil {
		stop(g)
	}
	// let goroutines from the last gen wind down before the final count
	time.Sleep(500 * time.Millisecond)
	runtime.GC()

	// --- report --------------------------------------------------------------
	p50, p95, p99 := lat.percentiles()
	totalDNS := cnt.ok.Load() + cnt.servfail.Load() + cnt.other.Load() + cnt.dialErr.Load()
	secs := duration.Seconds()

	fmt.Printf("\n================ STRESS REPORT ================\n")
	fmt.Printf("duration            : %s\n", duration)
	fmt.Printf("DNS total           : %d  (%.0f q/s)\n", totalDNS, float64(totalDNS)/secs)
	fmt.Printf("  success (NOERROR) : %d\n", cnt.ok.Load())
	fmt.Printf("  SERVFAIL          : %d\n", cnt.servfail.Load())
	fmt.Printf("  other rcode       : %d\n", cnt.other.Load())
	fmt.Printf("  dial/timeout err  : %d  (includes rebuild downtime windows)\n", cnt.dialErr.Load())
	fmt.Printf("DNS latency (ms)    : p50=%.2f p95=%.2f p99=%.2f\n", p50, p95, p99)
	fmt.Printf("HTTP API            : ok=%d err=%d (%.0f req/s)\n", apiOK.Load(), apiErr.Load(), float64(apiOK.Load())/secs)
	fmt.Printf("upstream swaps      : ok=%d err=%d\n", swaps.Load(), swapErr.Load())
	rb := rebuilt.Load()
	avgDown := time.Duration(0)
	if rb > 0 {
		avgDown = time.Duration(downtimeNs.Load() / rb)
	}
	fmt.Printf("rebuilds            : %d  (avg downtime/apply %s — blocklist reload)\n", rb, avgDown.Round(time.Millisecond))
	fmt.Printf("SQLITE locked events: %d\n", hook.locked.Load())
	fmt.Printf("SQLITE busy events  : %d\n", hook.busy.Load())
	fmt.Printf("\n  t(s)  goroutines  heapMB   fds  rebuilds\n")
	for _, s := range samples {
		fmt.Printf("  %4d  %10d  %6.1f  %4d  %8d\n",
			int(s.t.Seconds()), s.goroutine, s.heapMB, s.fds, s.rebuilds)
	}
	fmt.Printf("final goroutines    : %d\n", runtime.NumGoroutine())
	fmt.Printf("final fds           : %d\n", fdCount())
	fmt.Printf("===============================================\n\n")

	// --- assertions: the run must HOLD ---------------------------------------
	if hook.locked.Load() > 0 {
		t.Errorf("SQLite 'database is locked' events: %d (expected 0)", hook.locked.Load())
	}
	if hook.busy.Load() > 0 {
		t.Errorf("SQLite BUSY events: %d (expected 0)", hook.busy.Load())
	}
	if cnt.ok.Load() == 0 {
		t.Errorf("no successful DNS queries — server never served")
	}
	// leak guard: goroutines/fds must not grow monotonically with rebuild count.
	// Compare an early steady-state sample to the final one (allow slack).
	if len(samples) >= 4 {
		early := samples[2] // after warmup + a couple rebuilds
		lastG := runtime.NumGoroutine()
		lastFD := fdCount()
		if lastG > early.goroutine*3 {
			t.Errorf("goroutine growth suggests a leak: early=%d final=%d over %d rebuilds",
				early.goroutine, lastG, rebuilt.Load())
		}
		if early.fds > 0 && lastFD > early.fds*3 {
			t.Errorf("fd growth suggests a leak: early=%d final=%d over %d rebuilds",
				early.fds, lastFD, rebuilt.Load())
		}
	}
}

// startMockUpstream runs an in-process UDP+TCP DNS server that answers any
// query instantly, and returns it as a config.Upstream plus a stop func.
func startMockUpstream(t *testing.T) (config.Upstream, func()) {
	t.Helper()
	var (
		pc   net.PacketConn
		tl   net.Listener
		port int
	)
	for try := 0; try < 20; try++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("mock tcp listen: %v", err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		u, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			_ = l.Close() // udp port taken, try another
			continue
		}
		tl, pc, port = l, u, p
		break
	}
	if tl == nil {
		t.Fatalf("mock upstream: no free udp+tcp port pair")
	}

	handler := dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(req)
		for _, q := range req.Question {
			switch q.Qtype {
			case dns.TypeA:
				rr, _ := dns.NewRR(q.Name + " 60 IN A 93.184.216.34")
				resp.Answer = append(resp.Answer, rr)
			case dns.TypeAAAA:
				rr, _ := dns.NewRR(q.Name + " 60 IN AAAA 2606:2800:220:1:248:1893:25c8:1946")
				resp.Answer = append(resp.Answer, rr)
			}
		}
		_ = w.WriteMsg(resp)
	})

	udpSrv := &dns.Server{PacketConn: pc, Handler: handler}
	tcpSrv := &dns.Server{Listener: tl, Handler: handler}
	go func() { _ = udpSrv.ActivateAndServe() }()
	go func() { _ = tcpSrv.ActivateAndServe() }()

	up := config.Upstream{Net: config.NetProtocolTcpUdp, Host: "127.0.0.1", Port: uint16(port)}
	return up, func() {
		_ = udpSrv.Shutdown()
		_ = tcpSrv.Shutdown()
	}
}

// waitReady blocks until the DNS listener answers or the deadline passes.
func waitReady(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	c := &dns.Client{Net: "udp", Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m := new(dns.Msg)
		m.SetQuestion("ready.check.", dns.TypeA)
		if _, _, err := c.Exchange(m, addr); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("server not ready on %s after %s", addr, timeout)
}

func makeNames(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("host%d.example%d.com.", i, i%37))
	}
	// a few well-known shapes + likely-blocked candidates for variety
	out = append(out, "doubleclick.net.", "google.com.", "cloudflare.com.", "www.bild.de.")
	return out
}

func readAllDiscard(resp *http.Response) (int, error) {
	buf := make([]byte, 32*1024)
	total := 0
	for {
		n, err := resp.Body.Read(buf)
		total += n
		if err != nil {
			return total, err
		}
	}
}
