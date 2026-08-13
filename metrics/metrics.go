package metrics

import (
	"github.com/0xERR0R/blocky/config"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//nolint:gochecknoglobals
var Reg = prometheus.NewRegistry()

// RegisterMetric registers prometheus collector.
//
// If a collector with the same descriptors is already registered (which
// happens when the server is rebuilt in-process, e.g. on config reload),
// the old collector is replaced by the new one so exported metrics always
// track the live instance. This matters for collectors like GaugeFunc that
// capture instance state by reference. Counters reset to zero on rebuild;
// Prometheus handles counter resets natively.
func RegisterMetric(c prometheus.Collector) {
	if err := Reg.Register(c); err != nil {
		Reg.Unregister(c)
		_ = Reg.Register(c)
	}
}

// Start starts prometheus endpoint
func Start(router *chi.Mux, cfg config.Metrics) {
	if cfg.Enable {
		_ = Reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		_ = Reg.Register(collectors.NewGoCollector())
		router.Handle(cfg.Path, promhttp.InstrumentMetricHandler(Reg,
			promhttp.HandlerFor(Reg, promhttp.HandlerOpts{})))
	}
}
