package decoy

import (
	"context"
	"math/rand"
	"net"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/metrics"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"
	"github.com/0xERR0R/blocky/util"
)

//nolint:gochecknoglobals
var decoyQueriesTotal = promauto.With(metrics.Reg).NewCounter(prometheus.CounterOpts{
	Name: "blocky_decoy_queries_total",
	Help: "Total number of synthetic decoy/noise queries emitted",
})

// decoyClientIP is the pseudo-client all decoy queries appear to come from
// (loopback), so they never look like a real LAN client in the log.
const decoyClientIP = "127.0.0.1"

// ResolveFunc runs a request through the server's resolver chain (same path
// real queries take, so decoys follow the active group strategy/recursion).
type ResolveFunc func(ctx context.Context, req *model.Request) (*model.Response, error)

// Engine emits background noise queries at a randomized rate, mixing replayed
// real queries with entries from the static list. Every emitted request is
// marked Bypass (skip cache) and Decoy (excluded from dashboards).
type Engine struct {
	cfg     config.DecoyConfig
	source  *querylog.DecoySource
	resolve ResolveFunc
	logger  *logrus.Entry
	rnd     *rand.Rand
	now     func() time.Time // injectable for tests
}

func NewEngine(cfg config.DecoyConfig, source *querylog.DecoySource, resolve ResolveFunc) *Engine {
	return &Engine{
		cfg:     cfg,
		source:  source,
		resolve: resolve,
		logger:  log.PrefixedLog("decoy"),
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // noise timing, not crypto
		now:     time.Now,
	}
}

// Seed loads the embedded list into the source if the table is empty.
func (e *Engine) Seed() error {
	r, err := openList()
	if err != nil {
		return err
	}
	defer r.Close()

	n, err := e.source.SeedIfEmpty(r)
	if err != nil {
		return err
	}

	if n > 0 {
		e.logger.Infof("seeded %d decoy domains", n)
	}

	return nil
}

// Run seeds the list, then emits decoy queries until ctx is cancelled. Inter-
// arrival times are exponentially distributed (Poisson process) around the
// configured mean rate — a clean fixed tick would itself be a fingerprint.
func (e *Engine) Run(ctx context.Context) {
	if err := e.Seed(); err != nil {
		e.logger.WithError(err).Error("can't seed decoy list; decoy engine not started")

		return
	}

	e.logger.Infof("decoy engine started (%.1f queries/min, active %02d:00-%02d:00)",
		e.cfg.QueriesPerMinute, e.cfg.ActiveHoursStart, e.cfg.ActiveHoursEnd)

	timer := time.NewTimer(e.nextInterval())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if e.withinActiveHours(e.now()) {
				e.emit(ctx)
			}

			timer.Reset(e.nextInterval())
		}
	}
}

// nextInterval draws an exponential inter-arrival time for the configured rate.
func (e *Engine) nextInterval() time.Duration {
	qpm := e.cfg.QueriesPerMinute
	if qpm <= 0 {
		qpm = 1
	}

	meanSeconds := 60.0 / qpm

	return time.Duration(e.rnd.ExpFloat64() * meanSeconds * float64(time.Second))
}

// withinActiveHours reports whether t's hour is in [start, end). end==24 means
// midnight (whole day when start==0).
func (e *Engine) withinActiveHours(t time.Time) bool {
	h := t.Hour()

	return h >= e.cfg.ActiveHoursStart && h < e.cfg.ActiveHoursEnd
}

// emit builds one synthetic request and resolves it through the chain.
func (e *Engine) emit(ctx context.Context) {
	name, qtype := e.nextQuery()
	if name == "" {
		return
	}

	req := &model.Request{
		ClientIP:  net.ParseIP(decoyClientIP),
		Req:       util.NewMsgWithQuestion(dns.Fqdn(name), dns.Type(qtype)),
		Protocol:  model.RequestProtocolUDP,
		RequestTS: e.now(),
		Bypass:    true,
		Decoy:     true,
	}

	if _, err := e.resolve(ctx, req); err != nil {
		e.logger.WithError(err).Debug("decoy query failed")
	}

	decoyQueriesTotal.Inc()
}

// nextQuery weighted-chooses a source and returns a domain + qtype. Replay is
// preferred by ReplayWeight:ListWeight, but an empty replay pool (cold start)
// falls through to the list, so early on it is effectively 100% list.
func (e *Engine) nextQuery() (name string, qtype uint16) {
	if e.chooseReplay() {
		if q, err := e.source.SampleRecentReal(1); err != nil {
			e.logger.WithError(err).Debug("replay sample failed")
		} else if len(q) > 0 {
			return q[0].Name, qtypeFromString(q[0].Qtype)
		}
	}

	domain, err := e.source.SampleList()
	if err != nil {
		e.logger.WithError(err).Debug("list sample failed")

		return "", 0
	}

	return domain, e.randListQtype()
}

// chooseReplay flips a weighted coin: replay with probability
// ReplayWeight/(ReplayWeight+ListWeight).
func (e *Engine) chooseReplay() bool {
	total := e.cfg.ReplayWeight + e.cfg.ListWeight
	if total == 0 {
		return false
	}

	return uint(e.rnd.Intn(int(total))) < e.cfg.ReplayWeight
}

// randListQtype picks A or AAAA for list-sourced queries.
func (e *Engine) randListQtype() uint16 {
	if e.rnd.Intn(2) == 0 {
		return dns.TypeAAAA
	}

	return dns.TypeA
}

func qtypeFromString(s string) uint16 {
	if t, ok := dns.StringToType[s]; ok {
		return t
	}

	return dns.TypeA
}
