package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xERR0R/blocky/cache"
	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/decoy"
	"github.com/0xERR0R/blocky/lists"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/metrics"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/prewarm"
	"github.com/0xERR0R/blocky/querylog"
	"github.com/0xERR0R/blocky/redis"
	"github.com/0xERR0R/blocky/resolver"
	"github.com/0xERR0R/blocky/server/freebind"

	"github.com/0xERR0R/blocky/util"
	goredis "github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/hashicorp/go-multierror"

	"github.com/go-chi/chi/v5"
	"github.com/miekg/dns"
	"github.com/pires/go-proxyproto"
	"github.com/quic-go/quic-go"
	"github.com/sirupsen/logrus"
)

const (
	maxUDPBufferSize = 65535
	caExpiryYears    = 10
	certExpiryYears  = 5

	networkUDP    = "udp"
	networkTCP    = "tcp"
	networkTCPTLS = "tcp-tls"
)

// retireGrace is how long a retired resolver bundle's background loops are
// stopped before its io resources (redis bridge/conn, log buffers) are closed,
// so in-flight Resolve() calls that captured the old chain can finish. Mirrors
// resolver.replaceUpstreamsCloseDelay. Var, not const, so tests can shorten it.
//
//nolint:gochecknoglobals
var retireGrace = 10 * time.Second

// resolverBundle is everything a config apply rebuilds. It is published behind
// Server.live via an atomic.Pointer and is read-only after publication: never
// mutate a bundle that has been stored.
type resolverBundle struct {
	resolver     resolver.ChainedResolver
	cfg          *config.Config
	upstreamTree *resolver.UpstreamTreeResolver // nil = no live upstream swap (single group / recursive)
	ctx          context.Context                // this bundle's background context (child of serverCtx)
	cancel       context.CancelFunc             // cancels ctx; stops this bundle's background loops
	closers      []io.Closer                    // redis bridge/conn, closed after retireGrace on retire
	logFlushers  []interface{ Flush() error }   // query-log resolvers, flushed on retire
	dbClosers    []func() error                 // query-log DB conns, closed AFTER the flush on retire/shutdown
	decoyEngine  *decoy.Engine                  // background noise generator; nil unless privacy.decoy.enable
	listUpdater  *lists.Updater                 // background list refresher; nil unless lists.updater.enable
	prewarm      *prewarm.Worker                // corpus pre-warmer; nil unless privacy.decoy.enable
}

// Server controls the endpoints for DNS and HTTP.
//
// The listeners, HTTP router, DNS servers, query-log hub and decoy source are
// built once in NewServer and torn down only on real shutdown. Everything a
// config apply rebuilds lives behind the atomic live pointer, so an apply swaps
// the resolver chain without dropping :80/:53. See ApplyConfig.
type Server struct {
	// persistent — built once, torn down only on real shutdown
	dnsServers        []*dns.Server
	servers           map[net.Listener]*httpServer
	http3Server       *http3Server          // nil when disabled
	http3PacketConns  []net.PacketConn      // one per address in ports.https
	store             *configstore.Store    // nil = config endpoints respond 503
	qlHub             *querylog.Hub         // live query stream fan-out; survives applies; nil unless sqlite query log
	decoySource       *querylog.DecoySource // sqlite path is stable; survives applies; closed in Stop
	persistentClosers []io.Closer           // stats RO reader etc.; closed in Stop

	// swappable — the whole point: an apply rebuilds this and swaps it atomically
	live atomic.Pointer[resolverBundle]

	// retired bundles whose delayed flush+close (time.AfterFunc, retireGrace) is
	// still pending. Drained synchronously in Stop so a shutdown within retireGrace
	// doesn't drop their buffered query-log entries (AfterFunc timers die on exit).
	retiredMu sync.Mutex
	retired   map[*resolverBundle]struct{}
}

// retireBundle flushes a retired bundle's query-log buffers, closes its DB
// conns (after the flush lands — the flush needs them open) and closes its io.
// Called from the delayed AfterFunc on a normal retire and synchronously from
// Stop for any retiree still pending at shutdown.
func (s *Server) retireBundle(b *resolverBundle) {
	for _, fl := range b.logFlushers {
		_ = fl.Flush()
	}

	for _, c := range b.dbClosers {
		_ = c()
	}

	closeAll(b.closers)
}

// SwapUpstreams replaces one group's upstreams in the running resolver tree
// without rebuilding the server. Errors when no tree is present in the chain
// (e.g. single-group config, where the tree collapses to its only branch).
func (s *Server) SwapUpstreams(ctx context.Context, group string, upstreams []config.Upstream) error {
	tree := s.live.Load().upstreamTree
	if tree == nil {
		return errors.New("no upstream tree in the running resolver chain")
	}

	return tree.ReplaceUpstreams(ctx, group, upstreams)
}

func logger() *logrus.Entry {
	return log.PrefixedLog("server")
}

func tlsCipherSuites() []uint16 {
	tlsCipherSuites := []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
	}

	return tlsCipherSuites
}

type NewServerFunc func(address string) (*dns.Server, error)

func retrieveCertificate(cfg *config.Config) (cert tls.Certificate, err error) {
	if cfg.CertFile == "" && cfg.KeyFile == "" {
		cert, err = util.TLSGenerateSelfSignedCert([]string{"blocky.invalid", "*"})
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("unable to generate self-signed certificate: %w", err)
		}

		log.Log().Info("using self-signed certificate")
	} else {
		cert, err = tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("can't load certificate files: %w", err)
		}
	}

	return cert, nil
}

func newTLSConfig(cfg *config.Config) (*tls.Config, error) {
	var cert tls.Certificate

	cert, err := retrieveCertificate(cfg)
	if err != nil {
		return nil, fmt.Errorf("can't retrieve cert: %w", err)
	}

	// #nosec G402 // See TLSVersion.validate
	res := &tls.Config{
		MinVersion:   uint16(cfg.MinTLSServeVer), //nolint:gosec // TLS version constants fit safely in uint16
		CipherSuites: tlsCipherSuites(),
		Certificates: []tls.Certificate{cert},
	}

	return res, nil
}

// NewServer creates a new server instance with the passed config. It builds the
// persistent parts (listeners, DNS servers, HTTP router, query-log hub, decoy
// source) once and the first resolver bundle behind the atomic live pointer. A
// later config apply rebuilds only the bundle (see ApplyConfig); the listeners
// are never in the apply path. ctx is the server-lifetime context, cancelled
// only on real shutdown.
//
//nolint:funlen
func NewServer(ctx context.Context, cfg *config.Config, store *configstore.Store) (server *Server, err error) {
	var tlsCfg *tls.Config

	if len(cfg.Ports.HTTPS) > 0 || len(cfg.Ports.TLS) > 0 {
		tlsCfg, err = newTLSConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create TLS configuration: %w", err)
		}
	}

	if cfg.Ports.FreeBind && !freebind.Supported {
		logger().Warn("ports.freeBind: true is only supported on Linux; " +
			"ignoring on this platform (binding normally)")
	}

	dnsServers, err := createServers(ctx, cfg, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("server creation failed: %w", err)
	}

	httpListeners, httpsListeners, http3PacketConns, err := createHTTPListeners(ctx, cfg, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP/HTTPS listeners: %w", err)
	}

	// registered once for the process, not per apply, to avoid duplicate-registration surprises
	metrics.RegisterEventListeners()
	metrics.RegisterMetric(lists.ListUpdateTotal)

	server = &Server{
		dnsServers:       dnsServers,
		servers:          make(map[net.Listener]*httpServer),
		http3PacketConns: http3PacketConns,
		store:            store,
	}

	// The shared list/decoy source keys on the sqlite path, which a normal apply
	// never moves, so it is persistent (a query-log/decoy change takes the full
	// restart fallback, see ListenersCompatible). It must exist BEFORE the
	// resolver chain is built: the blocking resolver's "blocklist:<category>"
	// sources stream from it while their list caches load during construction.
	server.decoySource, err = openDecoySource(cfg)
	if err != nil {
		return nil, err
	}

	// live query stream: only the sqlite query log target feeds the UI, so the
	// hub (and the /api/ui/stream endpoint) exists only in that mode
	if cfg.QueryLog.Type == config.QueryLogTypeSqlite {
		server.qlHub = querylog.NewHub()
	}

	// build the first resolver bundle on a per-apply child context
	resolverCtx, cancel := context.WithCancel(ctx)

	bundle, err := server.buildResolverBundle(resolverCtx, cancel, cfg)
	if err != nil {
		cancel()

		return nil, err
	}

	server.live.Store(bundle)

	// First-boot only: hand freed pages back to the OS so the startup memstats
	// logged by printConfiguration reflect steady state. FreeOSMemory does a
	// synchronous madvise on top of the STW GC, so it must NOT run on the per-apply
	// hot-swap path (printConfiguration's own runtime.GC is the cheap part kept there).
	debug.FreeOSMemory()

	server.printConfiguration(bundle)

	server.registerDNSHandlers(ctx)

	openAPIImpl := server.createOpenAPIInterfaceImpl()

	var blStats blocklistStatser

	var classifier clientClassifier

	if server.decoySource != nil {
		blStats = server.decoySource
		classifier = server.decoySource
	}

	httpRouter, statsCloser := createHTTPRouter(cfg, openAPIImpl, store, server, server.qlHub, blStats, classifier)
	server.persistentClosers = append(server.persistentClosers, statsCloser) // stats RO reader, closed in Stop
	server.registerDoHEndpoints(httpRouter, cfg)

	if len(http3PacketConns) > 0 {
		server.http3Server = newHTTP3Server(httpRouter, newH3TLSConfig(tlsCfg))
	}

	if len(cfg.Ports.HTTP) != 0 {
		srv := newHTTPServer("http", httpRouter, cfg)

		for _, l := range httpListeners {
			server.servers[l] = srv
		}
	}

	if len(cfg.Ports.HTTPS) != 0 {
		var httpsHandler http.Handler = httpRouter
		if server.http3Server != nil {
			httpsHandler = newAltSvcMiddleware(server.http3Server)(httpRouter)
		}

		srv := newHTTPServer("https", httpsHandler, cfg)

		for _, l := range httpsListeners {
			server.servers[l] = srv
		}
	}

	return server, nil
}

// buildResolverBundle builds a complete, ready-to-publish resolver bundle off to
// the side: no listener or socket is touched. All the fallible per-apply work
// (bootstrap, redis, blocklist seed, resolver chain, decoy engine) happens here,
// so any error returns before the caller swaps — leaving the old bundle serving.
// On the error path anything already opened (redis conn/bridge) is closed to
// avoid a leak-on-failed-apply. ctx/cancel own the bundle's background loops.
func (s *Server) buildResolverBundle(
	ctx context.Context, cancel context.CancelFunc, cfg *config.Config,
) (*resolverBundle, error) {
	var deferredClosers []io.Closer

	fail := func(err error) (*resolverBundle, error) {
		closeAll(deferredClosers)

		return nil, err
	}

	bootstrap, err := resolver.NewBootstrap(ctx, cfg)
	if err != nil {
		return fail(fmt.Errorf("failed to create bootstrap resolver: %w", err))
	}

	var redisConn *goredis.Client
	if cfg.Redis.IsEnabled() {
		redisConn, err = redis.New(ctx, &cfg.Redis)
		if err != nil {
			if cfg.Redis.Required {
				return fail(fmt.Errorf("failed to create required Redis client: %w", err))
			}

			logger().WithError(err).Warn("Redis is enabled but optional and could not be initialized, continuing without Redis")
		}
	}

	if redisConn != nil {
		deferredClosers = append(deferredClosers, redisConn)
	}

	redisResult, err := createRedisCacheDecorator(ctx, redisConn, cfg.Redis.Required)
	if err != nil {
		return fail(err)
	}

	if redisResult.bridge != nil {
		deferredClosers = append(deferredClosers, redisResult.bridge)
	}

	// Seed the enabled blocklist categories BEFORE the resolver chain is built:
	// the blocking resolver imports its blocklist:<category> sources during
	// construction, so the rows must already exist (the background updater seeds
	// asynchronously in Start, which is too late — blocking would load 0 domains).
	// SeedBlocklistFloor makes this a fast no-op on every launch after the first.
	if s.decoySource != nil && cfg.Lists.Updater.Enable && blockingUsesBlocklistSources(cfg) {
		seeder := lists.NewUpdater(cfg.Lists.Updater, s.decoySource, false)
		seeder.SetEnabledCategories(enabledBlocklistCategories(s.store))

		if err := seeder.SeedBlocklistFloor(); err != nil {
			return fail(fmt.Errorf("can't seed blocklist floor: %w", err))
		}
	}

	queryResolver, err := createQueryResolver(ctx, cfg, bootstrap, redisResult.decorator)
	if err != nil {
		return fail(err)
	}

	bundle := &resolverBundle{
		resolver: queryResolver,
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
	}

	// retain the upstream tree for live upstream swaps (it is the chain's
	// non-chained tail, so GetFromChainWithType can't reach it), collect the
	// query-log flushers, and re-attach the persistent hub so open /api/ui/stream
	// SSE connections stay on the same hub across the swap
	resolver.ForEach(queryResolver, func(res resolver.Resolver) {
		if tree, ok := res.(*resolver.UpstreamTreeResolver); ok {
			bundle.upstreamTree = tree
		}

		if fl, ok := res.(interface{ Flush() error }); ok {
			bundle.logFlushers = append(bundle.logFlushers, fl)
		}

		if qlr, ok := res.(*resolver.QueryLoggingResolver); ok {
			if s.qlHub != nil {
				qlr.SetHub(s.qlHub)
			}

			// close the query-log DB conn when this bundle retires, else every
			// apply leaks an sqlite (or remote DB) connection
			if c := qlr.WriterDBCloser(); c != nil {
				bundle.dbClosers = append(bundle.dbClosers, c)
			}
		}
	})

	decoyEngine, listUpdater, prewarmWorker := s.buildDecoyEngine(cfg)
	bundle.decoyEngine = decoyEngine
	bundle.listUpdater = listUpdater
	bundle.prewarm = prewarmWorker

	if redisResult.bridge != nil {
		bundle.closers = append(bundle.closers, redisResult.bridge)
	}

	if redisConn != nil {
		bundle.closers = append(bundle.closers, redisConn)
	}

	return bundle, nil
}

// ApplyConfig rebuilds the resolver chain (and its background deps) from cfg and
// swaps it in atomically behind the live listeners — :80/:53 never drop. The
// ordering avoids half-built reads (live only ever holds a fully built bundle),
// use-after-close (old io closes after retireGrace so in-flight Resolve() calls
// finish) and leaks (old.cancel + delayed closeAll reap every old goroutine and
// conn). On any build error the old bundle stays live and the error is returned.
func (s *Server) ApplyConfig(serverCtx context.Context, cfg *config.Config) error {
	resolverCtx, cancel := context.WithCancel(serverCtx)

	newBundle, err := s.buildResolverBundle(resolverCtx, cancel, cfg)
	if err != nil {
		cancel()

		return err
	}

	old := s.live.Swap(newBundle)

	s.startBundleBackground(newBundle)

	s.printConfiguration(newBundle)

	if old != nil {
		// stop the old background loops immediately (they read s.resolve, which
		// is already the new chain); close its io after a grace delay so in-flight
		// Resolve() calls that captured the old chain can finish.
		old.cancel()

		s.retiredMu.Lock()
		if s.retired == nil {
			s.retired = make(map[*resolverBundle]struct{})
		}

		s.retired[old] = struct{}{}
		s.retiredMu.Unlock()

		time.AfterFunc(retireGrace, func() {
			// Claim the retiree under the lock: whoever removes it owns the
			// flush+close. If Stop already drained it, this is a no-op.
			s.retiredMu.Lock()
			_, pending := s.retired[old]
			delete(s.retired, old)
			s.retiredMu.Unlock()

			if pending {
				s.retireBundle(old)
			}
		})
	}

	return nil
}

// startBundleBackground starts a bundle's background goroutines on its own
// context, so a later swap can stop exactly these via the bundle's cancel.
func (s *Server) startBundleBackground(b *resolverBundle) {
	if b.decoyEngine != nil {
		go b.decoyEngine.Run(b.ctx)
	}

	if b.listUpdater != nil {
		go b.listUpdater.Run(b.ctx)
	}

	if b.prewarm != nil {
		go b.prewarm.Run(b.ctx)
	}
}

// ListenersCompatible reports whether the running server can hot-swap to newCfg
// without rebinding sockets or rebuilding the router. It returns false when a
// change touches anything the persistent listeners/router/decoy source depend
// on — the caller then falls back to a full restart (brief downtime, correct).
//
// ponytail: hot-swap covers resolver/blocking/lists/caching/upstreams/dnssec
// config only. Ports (incl. proxy-protocol, freeBind, DoH path), TLS, HTTP/3 and
// the query-log/decoy target take the full-restart fallback. Upgrade path:
// rebind individual listeners live if that class of change ever becomes frequent.
func ListenersCompatible(a, b *config.Config) bool {
	return reflect.DeepEqual(a.Ports, b.Ports) &&
		a.CertFile == b.CertFile &&
		a.KeyFile == b.KeyFile &&
		a.MinTLSServeVer == b.MinTLSServeVer &&
		reflect.DeepEqual(a.HTTP3, b.HTTP3) &&
		// /metrics is registered into the router once at NewServer from
		// cfg.Prometheus (enable/path); the hot-swap rebuilds only the resolver
		// bundle, not the router, so a prometheus change must force a full restart.
		reflect.DeepEqual(a.Prometheus, b.Prometheus) &&
		// the query-log hub, decoy source and stats reader are frozen into the
		// router once and key on the sqlite target; a change to any of these
		// makes them stale, so force a restart
		reflect.DeepEqual(a.QueryLog, b.QueryLog) &&
		a.Privacy.Decoy.Enable == b.Privacy.Decoy.Enable &&
		a.Lists.Updater.Enable == b.Lists.Updater.Enable
}

func createServers(ctx context.Context, cfg *config.Config, tlsCfg *tls.Config) ([]*dns.Server, error) {
	var dnsServers []*dns.Server

	var err *multierror.Error

	freeBind := cfg.Ports.FreeBind

	addServers := func(newServer NewServerFunc, addresses config.ListenConfig) error {
		for _, address := range addresses {
			server, err := newServer(address)
			if err != nil {
				return err
			}

			dnsServers = append(dnsServers, server)
		}

		return nil
	}

	err = multierror.Append(err,
		addServers(func(address string) (*dns.Server, error) {
			return createUDPServer(ctx, address, listenerOptions{freeBind: freeBind})
		}, cfg.Ports.DNS),
		addServers(func(address string) (*dns.Server, error) {
			return createTCPServer(ctx, address, listenerOptions{
				freeBind:      freeBind,
				proxyProtocol: cfg.Ports.ProxyProtocol.Has(config.ProxyProtocolTypeDns),
			})
		}, cfg.Ports.DNS),
		addServers(func(address string) (*dns.Server, error) {
			return createTLSServer(ctx, address, tlsCfg, listenerOptions{
				freeBind:      freeBind,
				proxyProtocol: cfg.Ports.ProxyProtocol.Has(config.ProxyProtocolTypeTls),
			})
		}, cfg.Ports.TLS))

	if multiErr := err.ErrorOrNil(); multiErr != nil {
		return nil, fmt.Errorf("failed to create DNS servers: %w", multiErr)
	}

	return dnsServers, nil
}

func createHTTPListeners(
	ctx context.Context, cfg *config.Config, tlsCfg *tls.Config,
) (httpListeners, httpsListeners []net.Listener, http3PacketConns []net.PacketConn, err error) {
	httpListeners, err = newTCPListeners(ctx, "http", cfg.Ports.HTTP,
		cfg.Ports.ProxyProtocol.Has(config.ProxyProtocolTypeHttp))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create HTTP listeners: %w", err)
	}

	httpsListeners, err = newTLSListeners(ctx, "https", cfg.Ports.HTTPS, tlsCfg,
		cfg.Ports.ProxyProtocol.Has(config.ProxyProtocolTypeHttps))
	if err != nil {
		closeAll(httpListeners)

		return nil, nil, nil, fmt.Errorf("failed to create HTTPS listeners: %w", err)
	}

	if cfg.HTTP3.IsEnabled() {
		switch {
		case len(cfg.Ports.HTTPS) == 0:
			logger().Warn("http3.enable is true but ports.https is empty; HTTP/3 disabled")
		case cfg.Ports.ProxyProtocol.Has(config.ProxyProtocolTypeHttps):
			logger().Warn("http3.enable is true but ports.proxyProtocol includes 'https'; " +
				"HTTP/3 cannot carry PROXY protocol headers and is disabled to keep the client IP consistent")
		default:
			http3PacketConns, err = newUDPPacketConns(ctx, cfg.Ports.HTTPS)
			if err != nil {
				closeAll(httpListeners)
				closeAll(httpsListeners)

				return nil, nil, nil, fmt.Errorf("failed to create HTTP/3 UDP listeners: %w", err)
			}
		}
	}

	return httpListeners, httpsListeners, http3PacketConns, nil
}

func closeAll[T io.Closer](closers []T) {
	for _, c := range closers {
		_ = c.Close()
	}
}

func newTCPListeners(
	ctx context.Context, proto string, addresses config.ListenConfig, proxyProtocol bool,
) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(addresses))
	lc := &net.ListenConfig{}

	for _, address := range addresses {
		listener, err := lc.Listen(ctx, networkTCP, address)
		if err != nil {
			return nil, fmt.Errorf("start %s listener on %s failed: %w", proto, address, err)
		}

		listener = newProxyProtocolListener(listener, proxyProtocol)

		listeners = append(listeners, listener)
	}

	return listeners, nil
}

func newTLSListeners(
	ctx context.Context, proto string, addresses config.ListenConfig, tlsCfg *tls.Config, proxyProtocol bool,
) ([]net.Listener, error) {
	listeners, err := newTCPListeners(ctx, proto, addresses, proxyProtocol)
	if err != nil {
		return nil, fmt.Errorf("failed to create TCP listeners for TLS: %w", err)
	}

	for i, inner := range listeners {
		listeners[i] = tls.NewListener(inner, tlsCfg)
	}

	return listeners, nil
}

func newProxyProtocolListener(listener net.Listener, enabled bool) net.Listener {
	if !enabled {
		return listener
	}

	return &proxyproto.Listener{
		Listener: listener,
		ConnPolicy: func(proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			return proxyproto.REQUIRE, nil
		},
	}
}

// listenerOptions bundles the socket-level options applied when a DNS listener is pre-created
// before miekg/dns starts serving (freebind socket option, PROXY protocol wrapping).
type listenerOptions struct {
	freeBind      bool
	proxyProtocol bool
}

func createDNSServer(ctx context.Context, network, address string, tlsCfg *tls.Config, opts listenerOptions,
) (*dns.Server, error) {
	srv := &dns.Server{
		Addr:    address,
		Net:     network,
		Handler: dns.NewServeMux(),
		NotifyStartedFunc: func() {
			logger().Infof("%s server is up and running on address %s", strings.ToUpper(network), address)
		},
	}

	if network == networkUDP {
		srv.UDPSize = maxUDPBufferSize
	}

	if tlsCfg != nil {
		srv.TLSConfig = tlsCfg
	}

	// When freeBind is enabled (and supported), pre-create the listener with the IP_FREEBIND socket
	// option and hand it to the server, which is then started via ActivateAndServe (see Server.Start).
	if (opts.freeBind && freebind.Supported) || (opts.proxyProtocol && network != networkUDP) {
		if err := attachListener(ctx, srv, network, address, tlsCfg, listenerOptions{
			freeBind:      opts.freeBind && freebind.Supported,
			proxyProtocol: opts.proxyProtocol,
		}); err != nil {
			return nil, err
		}
	}

	return srv, nil
}

// attachListener creates a listener/packet connection for DNS servers that need custom socket handling
// before miekg/dns starts serving, such as freebind or PROXY protocol wrapping.
func attachListener(ctx context.Context, srv *dns.Server, network, address string,
	tlsCfg *tls.Config, opts listenerOptions,
) error {
	lc := net.ListenConfig{}
	if opts.freeBind {
		lc.Control = freebind.Control
	}

	switch network {
	case networkUDP:
		pc, err := lc.ListenPacket(ctx, networkUDP, address)
		if err != nil {
			return fmt.Errorf("freebind udp listener on %s failed: %w", address, err)
		}

		srv.PacketConn = pc
	case networkTCP:
		l, err := lc.Listen(ctx, networkTCP, address)
		if err != nil {
			return fmt.Errorf("tcp listener on %s failed: %w", address, err)
		}

		l = newProxyProtocolListener(l, opts.proxyProtocol)
		srv.Listener = l
	case networkTCPTLS:
		l, err := lc.Listen(ctx, networkTCP, address)
		if err != nil {
			return fmt.Errorf("tcp-tls listener on %s failed: %w", address, err)
		}

		l = newProxyProtocolListener(l, opts.proxyProtocol)
		srv.Listener = tls.NewListener(l, tlsCfg)
	default:
		return fmt.Errorf("unsupported DNS listener network %q", network)
	}

	return nil
}

func createTLSServer(ctx context.Context, address string, tlsCfg *tls.Config, opts listenerOptions,
) (*dns.Server, error) {
	return createDNSServer(ctx, networkTCPTLS, address, tlsCfg, opts)
}

func createTCPServer(ctx context.Context, address string, opts listenerOptions) (*dns.Server, error) {
	return createDNSServer(ctx, networkTCP, address, nil, opts)
}

func createUDPServer(ctx context.Context, address string, opts listenerOptions) (*dns.Server, error) {
	return createDNSServer(ctx, networkUDP, address, nil, opts)
}

type redisBridgeResult struct {
	decorator resolver.CacheDecorator
	bridge    *redis.EventBusBridge
}

func createRedisCacheDecorator(
	ctx context.Context, redisConn *goredis.Client, required bool,
) (*redisBridgeResult, error) {
	if redisConn == nil {
		return &redisBridgeResult{}, nil
	}

	bridge, err := redis.NewEventBusBridge(ctx, redisConn)
	if err != nil {
		if required {
			return nil, fmt.Errorf("failed to create required Redis event bridge: %w", err)
		}

		logger().Warn("failed to create Redis event bridge: ", err)
	}

	decorator := func(inner cache.ExpiringCache[[]byte]) (cache.ExpiringCache[[]byte], error) {
		return cache.NewRedisExpiringByteCache(ctx, inner, redisConn, cache.RedisOptions[[]byte]{
			Prefix:  "blocky:cache:",
			Channel: "blocky_cache_sync",
		})
	}

	return &redisBridgeResult{decorator: decorator, bridge: bridge}, nil
}

func createQueryResolver(
	ctx context.Context,
	cfg *config.Config,
	bootstrap *resolver.Bootstrap,
	cacheDecorator resolver.CacheDecorator,
) (resolver.ChainedResolver, error) {
	// Derive the encrypted-upstream padding flag from privacy config so it rides along
	// the Upstreams config already threaded to every upstream client (RFC 7830).
	cfg.Upstreams.EDNSPadding = cfg.Privacy.EDNSPadding.Enable
	cfg.Upstreams.QueryCaseRandomization = cfg.Privacy.QueryCaseRandomization
	// Shadow completion is coherent only with the noise engine running (it gives
	// the egressed tracker queries their decoy cover); without it, shadowing would
	// leak blocked-domain queries uncovered. So default-on, but gated on decoy.
	cfg.Blocking.ShadowBlockedQueries = cfg.Privacy.ShadowBlockedQueries && cfg.Privacy.Decoy.Enable

	upstreamTree, utErr := resolver.NewUpstreamTreeResolver(ctx, cfg.Upstreams, bootstrap)
	blocking, blErr := resolver.NewBlockingResolver(ctx, cfg.Blocking, bootstrap)
	queryLogging, qlErr := resolver.NewQueryLoggingResolver(ctx, cfg.QueryLog)
	condUpstream, cuErr := resolver.NewConditionalUpstreamResolver(ctx, cfg.Conditional, cfg.Upstreams, bootstrap)
	customDNS := resolver.NewCustomDNSResolver(cfg.CustomDNS)
	hostsFile, hfErr := resolver.NewHostsFileResolver(ctx, cfg.HostsFile, bootstrap)
	// client name resolution consults local reverse sources (custom DNS, hosts file) before the rDNS upstream
	clientNames, cnErr := resolver.NewClientNamesResolver(
		ctx, cfg.ClientLookup, cfg.Upstreams, bootstrap, customDNS, hostsFile)
	decorator := cacheDecorator
	if !cfg.Caching.IsEnabled() {
		decorator = nil
	}

	// cfg.DNSSEC gates the DO bit on prefetch reloads (they bypass the DNSSEC
	// resolver above the cache).
	cachingResolver, crErr := resolver.NewCachingResolver(ctx, cfg.Caching, cfg.DNSSEC, cfg.Privacy.TTLJitter, decorator)
	// Pass upstreamTree to DNSSEC resolver so it can query for DNSKEY/DS records
	dnssecResolver, dsErr := resolver.NewDNSSECResolver(ctx, cfg.DNSSEC, upstreamTree)

	multiErr := multierror.Append(
		multierror.Prefix(utErr, "upstream tree resolver: "),
		multierror.Prefix(blErr, "blocking resolver: "),
		multierror.Prefix(qlErr, "query logging resolver: "),
		multierror.Prefix(cnErr, "client names resolver: "),
		multierror.Prefix(cuErr, "conditional upstream resolver: "),
		multierror.Prefix(hfErr, "hosts file resolver: "),
		multierror.Prefix(crErr, "caching resolver: "),
		multierror.Prefix(dsErr, "dnssec resolver: "),
	).ErrorOrNil()
	if multiErr != nil {
		// queryLogging is built eagerly (opens its DB) even when a sibling errors;
		// close its DB so a repeatedly-failing apply doesn't leak a conn each time.
		// Its goroutines are reaped by the caller's ctx cancel on the error path.
		if queryLogging != nil {
			if c := queryLogging.WriterDBCloser(); c != nil {
				_ = c()
			}
		}

		return nil, fmt.Errorf("failed to create query resolver components: %w", multiErr)
	}

	r := resolver.Chain(
		resolver.NewStatsResolver(ctx, cfg.Statistics),
		// stays above the ECS and client-name lookups: its bucket key must remain the
		// connection's source IP. Keyed on the ECS address instead (ecs.useAsClient), the
		// key would be attacker-controlled, letting a client both evade its own bucket and
		// fill the bounded bucket store, which drops queries of every new client once full.
		// Dropped queries therefore carry no client name and are attributed to the client
		// IP in the statistics.
		resolver.NewRateLimitingResolver(ctx, cfg.RateLimit),
		// adopts the ECS subnet as the internal client IP (ecs.useAsClient) before the
		// client-name lookup, blocking and the cache consume the client identity, so the
		// ECS client is used for those features and is preserved across cache hits
		resolver.NewECSClientResolver(cfg.ECS),
		// above filtering and fqdnOnly, which answer a query on their own: a lookup below
		// them would leave request.ClientNames empty for every query they short-circuit,
		// and the statistics would attribute those to the raw client IP while the same
		// client's other queries are attributed to its name
		clientNames,
		resolver.NewFilteringResolver(cfg.Filtering),
		resolver.NewFQDNOnlyResolver(cfg.FQDNOnly),
		resolver.NewEDEResolver(cfg.EDE),
		queryLogging,
		resolver.NewMetricsResolver(cfg.Prometheus),
		customDNS,
		hostsFile,
		// above blocking and the cache: it inspects only RESOLVED/CACHED answers
		// (conditional/custom DNS/hosts file/SUDN/blocked answers are recognized
		// by response type and pass through; the cache stores only
		// upstream-derived answers, so CACHED implies upstream origin), cached
		// answers — incl. entries synced via redis — are re-inspected on every
		// hit, and blocking's internal FQDN client-identifier lookups enter the
		// chain below it
		resolver.NewRebindingProtectionResolver(cfg.RebindingProtection),
		blocking,
		dnssecResolver, // DNSSEC validation BEFORE caching - validates all responses before they are cached
		cachingResolver,
		resolver.NewDNS64Resolver(cfg.DNS64), // DNS64 synthesis AFTER caching
		resolver.NewECSResolver(cfg.ECS),
		condUpstream,
		resolver.NewSpecialUseDomainNamesResolver(cfg.SUDN),
		upstreamTree,
	)

	return r, nil
}

func (s *Server) registerDNSHandlers(ctx context.Context) {
	for _, server := range s.dnsServers {
		//nolint:forcetypeassert // handler is always *dns.ServeMux; set during server construction
		handler := server.Handler.(*dns.ServeMux)
		handler.HandleFunc(".", func(w dns.ResponseWriter, m *dns.Msg) {
			s.OnRequest(ctx, w, m)
		})
		handler.HandleFunc("healthcheck.blocky", func(w dns.ResponseWriter, m *dns.Msg) {
			s.OnHealthCheck(ctx, w, m)
		})
	}
}

func (s *Server) printConfiguration(b *resolverBundle) {
	logger().Info("current configuration:")

	if b.cfg.Redis.IsEnabled() {
		logger().Info("Redis:")
		log.WithIndent(logger(), "  ", b.cfg.Redis.LogConfig)
	}

	resolver.ForEach(b.resolver, func(res resolver.Resolver) {
		resolver.LogResolverConfig(res, logger())
	})

	logger().Info("listeners:")
	log.WithIndent(logger(), "  ", b.cfg.Ports.LogConfig)

	if len(s.http3PacketConns) > 0 {
		logger().Info("HTTP/3:")
		log.WithIndent(logger(), "  ", b.cfg.HTTP3.LogConfig)
	}

	logger().Info("runtime information:")

	// A plain GC here (per boot AND per hot-swap apply) is what actually reclaims a
	// retired bundle's sqlite writer FDs: closeDB shuts the *sql.DB but gorm's cached
	// prepared statements keep the underlying file descriptor alive until their
	// finalizers run. This is cheap (no madvise) — the expensive debug.FreeOSMemory()
	// that the finding flagged is first-boot-only, in NewServer.
	runtime.GC()

	logger().Infof("  numCPU =       %d", runtime.NumCPU())
	logger().Infof("  numGoroutine = %d", runtime.NumGoroutine())

	// gather memory stats
	var m runtime.MemStats

	runtime.ReadMemStats(&m)

	logger().Infof("  memory:")
	logger().Infof("    heap =     %10v MB", toMB(m.HeapAlloc))
	logger().Infof("    sys =      %10v MB", toMB(m.Sys))
	logger().Infof("    numGC =    %10v", m.NumGC)
}

func toMB(b uint64) uint64 {
	const bytesInKB = 1024

	return b / bytesInKB / bytesInKB
}

// buildDecoyEngine wires the decoy noise generator and the unified list updater
// for one resolver bundle. Both live on one sqlite query-log connection (the
// decoy list, the blocklist tables and the version meta all persist there), so
// the persistent DecoySource is shared. With any non-sqlite query-log target
// both stay disabled. The returned workers are started on the bundle's context
// by startBundleBackground so a later swap stops the old ones.
func (s *Server) buildDecoyEngine(cfg *config.Config) (*decoy.Engine, *lists.Updater, *prewarm.Worker) {
	needDecoy := cfg.Privacy.Decoy.Enable
	needUpdater := cfg.Lists.Updater.Enable

	if !needDecoy && !needUpdater {
		return nil, nil, nil
	}

	if s.decoySource == nil {
		logger().Warn("privacy.decoy / lists.updater require queryLog.type: sqlite; both disabled")

		return nil, nil, nil
	}

	var (
		decoyEngine   *decoy.Engine
		listUpdater   *lists.Updater
		prewarmWorker *prewarm.Worker
	)

	if needDecoy {
		// s.resolve reads the live bundle per call, so the noise generator always
		// uses the current chain even across a swap.
		decoyEngine = decoy.NewEngine(cfg.Privacy.Decoy, s.decoySource, s.resolve)
		// Live real-query tap (reactive volume + browse-triggered companions).
		// Nil in non-sqlite mode, but the decoy engine only runs in sqlite mode.
		decoyEngine.SetHub(s.qlHub)
		// Corpus pre-warmer: pull trending/mid-band domains in before first visit.
		prewarmWorker = prewarm.New(cfg.Privacy.Decoy, s.decoySource)
	}

	if needUpdater {
		listUpdater = lists.NewUpdater(cfg.Lists.Updater, s.decoySource, needDecoy)
		// Seed only the categories the user has enabled, so a fresh box carries a
		// few small lists instead of all ~5.4M embedded domains (~540MB).
		listUpdater.SetEnabledCategories(enabledBlocklistCategories(s.store))
	}

	return decoyEngine, listUpdater, prewarmWorker
}

// enabledBlocklistCategories returns the provider the list updater uses to
// decide which blocklist categories to seed and keep. With no config store it
// falls back to the updater's small default set.
func enabledBlocklistCategories(store *configstore.Store) func() ([]string, error) {
	if store == nil {
		return nil
	}

	return func() ([]string, error) {
		cats, err := store.ListBlockingCategories()
		if err != nil {
			return nil, err
		}

		enabled := make([]string, 0, len(cats))
		for _, c := range cats {
			if c.Enabled {
				enabled = append(enabled, c.Name)
			}
		}

		return enabled, nil
	}
}

// openDecoySource opens the shared sqlite list/decoy store when anything needs
// it (decoy engine, list updater, or blocking sources reading blocklist
// categories) and registers it as the lists package's blocklist provider.
// Returns nil (all consumers stay disabled) with a non-sqlite query log.
func openDecoySource(cfg *config.Config) (*querylog.DecoySource, error) {
	if cfg.QueryLog.Type != config.QueryLogTypeSqlite {
		return nil, nil
	}

	if !cfg.Privacy.Decoy.Enable && !cfg.Lists.Updater.Enable && !blockingUsesBlocklistSources(cfg) {
		return nil, nil
	}

	source, err := querylog.NewDecoySource(cfg.QueryLog.Target.Reveal())
	if err != nil {
		return nil, fmt.Errorf("can't open list/decoy source: %w", err)
	}

	lists.SetBlocklistProvider(source)

	return source, nil
}

// blockingUsesBlocklistSources reports whether any denylist references a
// database-backed "blocklist:<category>" source.
func blockingUsesBlocklistSources(cfg *config.Config) bool {
	for _, sources := range cfg.Blocking.Denylists {
		for _, src := range sources {
			if src.Type == config.BytesSourceTypeFile && strings.HasPrefix(src.From, lists.BlocklistSourcePrefix) {
				return true
			}
		}
	}

	return false
}

func (s *Server) Start(ctx context.Context, errCh chan<- error) {
	logger().Info("Starting server")

	// the first bundle's background loops run on the bundle's own context, so
	// the first apply's old.cancel stops exactly these
	s.startBundleBackground(s.live.Load())

	for _, srv := range s.dnsServers {
		go func() {
			// When a listener/packet connection was pre-created (freeBind), serve it via
			// ActivateAndServe; otherwise let miekg/dns create the socket via ListenAndServe.
			serve := srv.ListenAndServe
			if srv.Listener != nil || srv.PacketConn != nil {
				serve = srv.ActivateAndServe
			}

			if err := serve(); err != nil {
				errCh <- fmt.Errorf("start %s listener failed: %w", srv.Net, err)
			}
		}()
	}

	for listener, srv := range s.servers {
		go func() {
			logger().Infof("%s server is up and running on addr/port %s", srv, listener.Addr())

			err := srv.Serve(ctx, listener)
			if err != nil {
				errCh <- fmt.Errorf("%s on %s: %w", srv, listener.Addr(), err)
			}
		}()
	}

	if s.http3Server != nil {
		for _, pc := range s.http3PacketConns {
			go func() {
				logger().Infof("%s server is up and running on addr/port %s",
					s.http3Server, pc.LocalAddr())

				err := s.http3Server.inner.Serve(pc)
				if err != nil &&
					!errors.Is(err, quic.ErrServerClosed) &&
					!errors.Is(err, http.ErrServerClosed) &&
					!errors.Is(err, net.ErrClosed) {
					errCh <- fmt.Errorf("%s on %s: %w", s.http3Server, pc.LocalAddr(), err)
				}
			}()
		}
	}

	registerPrintConfigurationTrigger(ctx, s)
}

// Stop stops the server
func (s *Server) Stop(ctx context.Context) error {
	logger().Info("Stopping server")

	// Shut down HTTP/3 in order: server first (drains in-flight
	// requests and unblocks the Serve goroutines), then UDP packet
	// conns. Closing the packet conns first would cause Serve to
	// return a non-sentinel error that would land in errCh as a
	// spurious "server start failed".
	if s.http3Server != nil {
		if err := s.http3Server.Close(); err != nil {
			logger().Warn("failed to close http3 server: ", err)
		}
	}

	for _, pc := range s.http3PacketConns {
		if err := pc.Close(); err != nil {
			logger().Warn("failed to close http3 packet conn: ", err)
		}
	}

	// per-apply io resources of the live bundle (redis bridge/conn)
	if b := s.live.Load(); b != nil {
		for _, c := range b.closers {
			if err := c.Close(); err != nil {
				logger().Warn("failed to close resource: ", err)
			}
		}
	}

	// persistent resources (stats RO reader, etc.)
	for _, c := range s.persistentClosers {
		if err := c.Close(); err != nil {
			logger().Warn("failed to close resource: ", err)
		}
	}

	if s.decoySource != nil {
		if err := s.decoySource.Close(); err != nil {
			logger().Warn("failed to close decoy source: ", err)
		}
	}

	for _, server := range s.dnsServers {
		if err := server.ShutdownContext(ctx); err != nil {
			return fmt.Errorf("stop %s listener failed: %w", server.Net, err)
		}
	}

	// listeners are down — flush the live bundle's query log buffers synchronously
	// so process exit can't race the writers' async goroutines
	if b := s.live.Load(); b != nil {
		for _, fl := range b.logFlushers {
			if err := fl.Flush(); err != nil {
				logger().Warn("failed to flush query log on shutdown: ", err)
			}
		}

		// close DB conns AFTER the flush lands
		for _, c := range b.dbClosers {
			if err := c(); err != nil {
				logger().Warn("failed to close query log database: ", err)
			}
		}
	}

	// Drain any bundles retired within the last retireGrace: their delayed
	// AfterFunc flush hasn't fired yet and process-exit would drop those timers,
	// losing the bundle's buffered query-log entries. Claim them under the lock so
	// a concurrently-firing AfterFunc doesn't double-close.
	s.retiredMu.Lock()
	pending := s.retired
	s.retired = nil
	s.retiredMu.Unlock()

	for b := range pending {
		s.retireBundle(b)
	}

	return nil
}

func extractClientIDFromHost(hostName string) string {
	const clientIDPrefix = "id-"
	if strings.HasPrefix(hostName, clientIDPrefix) && strings.Contains(hostName, ".") {
		return hostName[len(clientIDPrefix):strings.Index(hostName, ".")]
	}

	return ""
}

func newRequest(
	ctx context.Context,
	clientIP net.IP, clientID string,
	protocol model.RequestProtocol, request *dns.Msg,
) (context.Context, *model.Request) {
	ctx, logger := log.CtxWithFields(ctx, logrus.Fields{
		"req_id":    uuid.New().String(),
		"question":  util.QuestionToString(request.Question),
		"client_ip": clientIP,
	})

	logger.WithFields(logrus.Fields{
		"client_request_id": request.Id,
		"client_id":         clientID,
		"protocol":          protocol,
	}).Trace("new incoming request")

	req := model.Request{
		ClientIP:        clientIP,
		RequestClientID: clientID,
		Protocol:        protocol,
		Req:             request,
		RequestTS:       time.Now(),
	}

	return ctx, &req
}

// fingerprintFromMsg snapshots the message-level fingerprint attributes before
// the resolver chain mutates the message in place (see clientQuery).
func fingerprintFromMsg(msg *dns.Msg) model.Fingerprint {
	fp := model.Fingerprint{
		MsgID: msg.Id,
		RD:    msg.RecursionDesired,
		CD:    msg.CheckingDisabled,
		AD:    msg.AuthenticatedData,
	}

	if len(msg.Question) > 0 {
		q := msg.Question[0]
		fp.QClass = q.Qclass
		fp.Mixed0x20 = strings.ContainsFunc(q.Name, func(r rune) bool { return r >= 'A' && r <= 'Z' })
	}

	if opt := msg.IsEdns0(); opt != nil {
		fp.HadEDNS0 = true
		fp.EDNSVersion = opt.Version()
		fp.EDNSUDPSize = opt.UDPSize()
		fp.DO = opt.Do()

		for _, o := range opt.Option {
			code := o.Option()
			fp.EDNSOptCodes = append(fp.EDNSOptCodes, code)

			if code == dns.EDNS0COOKIE {
				fp.HasCookie = true
			}
		}
	}

	return fp
}

func newRequestFromDNS(ctx context.Context, rw dns.ResponseWriter, msg *dns.Msg) (context.Context, *model.Request) {
	var (
		clientIP net.IP
		protocol model.RequestProtocol
	)

	fp := fingerprintFromMsg(msg)

	if rw != nil {
		clientIP, protocol = resolveClientIPAndProtocol(rw.RemoteAddr())

		switch a := rw.RemoteAddr().(type) {
		case *net.UDPAddr:
			fp.SrcPort = uint16(a.Port) //nolint:gosec // G115: ports are 0-65535
			fp.Transport = model.TransportDo53UDP
		case *net.TCPAddr:
			fp.SrcPort = uint16(a.Port) //nolint:gosec // G115: ports are 0-65535
			fp.Transport = model.TransportDo53TCP
		}
	}

	var clientID string
	if con, ok := rw.(dns.ConnectionStater); ok && con.ConnectionState() != nil {
		cs := con.ConnectionState()
		clientID = extractClientIDFromHost(cs.ServerName)

		fp.Transport = model.TransportDoT
		fp.TLSVersion = cs.Version
		fp.TLSCipher = cs.CipherSuite
		fp.SNI = cs.ServerName
		fp.ALPN = cs.NegotiatedProtocol
	}

	ctx, request := newRequest(ctx, clientIP, clientID, protocol, msg)
	request.Fingerprint = fp

	return ctx, request
}

func newRequestFromHTTP(ctx context.Context, req *http.Request, msg *dns.Msg) (context.Context, *model.Request) {
	protocol := model.RequestProtocolTCP
	clientIP := util.HTTPClientIP(req)

	clientID := chi.URLParam(req, "clientID")
	if clientID == "" {
		clientID = extractClientIDFromHost(req.Host)
	}

	fp := fingerprintFromMsg(msg)
	fp.UserAgent = req.Header.Get("User-Agent")

	if req.ProtoMajor == 3 {
		fp.Transport = model.TransportDoH3
	} else {
		fp.Transport = model.TransportDoH
	}

	if req.TLS != nil {
		fp.TLSVersion = req.TLS.Version
		fp.TLSCipher = req.TLS.CipherSuite
		fp.SNI = req.TLS.ServerName
		fp.ALPN = req.TLS.NegotiatedProtocol
	}

	if _, port, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		if p, err := strconv.ParseUint(port, 10, 16); err == nil {
			fp.SrcPort = uint16(p)
		}
	}

	ctx, request := newRequest(ctx, clientIP, clientID, protocol, msg)
	request.Fingerprint = fp

	return ctx, request
}

// OnRequest will be executed if a new DNS request is received
func (s *Server) OnRequest(ctx context.Context, w dns.ResponseWriter, msg *dns.Msg) {
	ctx, request := newRequestFromDNS(ctx, w, msg)

	s.handleReq(ctx, request, w)
}

type msgWriter interface {
	WriteMsg(msg *dns.Msg) error
}

func (s *Server) handleReq(ctx context.Context, request *model.Request, w msgWriter) {
	response, err := s.resolve(ctx, request)
	switch {
	case errors.Is(err, resolver.ErrRateLimited):
		return
	case err != nil:
		log.FromCtx(ctx).Error("error on processing request:", err)
		m := new(dns.Msg)
		m.SetRcode(request.Req, dns.RcodeServerFailure)
		err := w.WriteMsg(m)
		util.LogOnError(ctx, "can't write message: ", err)
	default:
		err := w.WriteMsg(response.Res)
		util.LogOnError(ctx, "can't write message: ", err)
	}
}

func (s *Server) resolve(ctx context.Context, request *model.Request) (response *model.Response, rerr error) {
	defer func() {
		if val := recover(); val != nil {
			rerr = fmt.Errorf("panic occurred: %v", val)
		}
	}()

	// load the live bundle once so this request uses one coherent chain+config
	// even if an apply swaps mid-flight (hot DNS path: atomic pointer, no lock)
	bundle := s.live.Load()

	contextUpstreamTimeoutMultiplier := 100
	timeoutDuration := time.Duration(contextUpstreamTimeoutMultiplier) * bundle.cfg.Upstreams.Timeout.ToDuration()

	ctx, cancel := context.WithTimeout(ctx, timeoutDuration)

	defer cancel()

	// The resolver chain mutates request.Req in place, so capture what the client itself asked for
	// up front: the response is normalized against that, not the mutated request.
	query := newClientQuery(request)

	switch {
	case len(request.Req.Question) == 0:
		m := new(dns.Msg)
		m.SetRcode(request.Req, dns.RcodeFormatError)

		log.FromCtx(ctx).Error("query has no questions")

		response = &model.Response{Res: m, RType: model.ResponseTypeCUSTOMDNS, Reason: "CUSTOM DNS"}
	default:
		var err error

		response, err = bundle.resolver.Resolve(ctx, request)
		if err != nil {
			var upstreamErr *resolver.UpstreamServerError

			if errors.As(err, &upstreamErr) {
				response = &model.Response{Res: upstreamErr.Msg, RType: model.ResponseTypeRESOLVED, Reason: upstreamErr.Error()}
			} else {
				return nil, fmt.Errorf("query resolution failed: %w", err)
			}
		}
	}

	query.normalizeResponse(response.Res)

	return response, nil
}

// OnHealthCheck Handler for docker health check. Just returns OK code without delegating to resolver chain
func (s *Server) OnHealthCheck(ctx context.Context, w dns.ResponseWriter, request *dns.Msg) {
	resp := new(dns.Msg)
	resp.SetReply(request)
	resp.Rcode = dns.RcodeSuccess

	err := w.WriteMsg(resp)
	util.LogOnError(ctx, "can't write message: ", err)
}

func resolveClientIPAndProtocol(addr net.Addr) (ip net.IP, protocol model.RequestProtocol) {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return a.IP, model.RequestProtocolUDP
	case *net.TCPAddr:
		return a.IP, model.RequestProtocolTCP
	}

	return nil, model.RequestProtocolUDP
}
