package resolver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"golang.org/x/net/publicsuffix"
)

const domainShardResolverType = "domain_shard"

// DomainShardResolver pins each registrable domain (eTLD+1) to one upstream via a
// stable hash: all subdomains of one site always hit the same provider, so no other
// upstream ever learns about that site, while different sites spread across providers.
type DomainShardResolver struct {
	configurable[*config.UpstreamGroup]
	typed

	resolvers atomic.Pointer[[]*upstreamResolverStatus]
}

// NewDomainShardResolver creates a new domain-shard resolver instance
func NewDomainShardResolver(
	ctx context.Context, cfg config.UpstreamGroup, bootstrap *Bootstrap,
) (*DomainShardResolver, error) {
	r := &DomainShardResolver{
		configurable: withConfig(&cfg),
		typed:        withType(domainShardResolverType),
	}

	// if init strategy is fast, use bootstrap until init finishes
	r.setResolvers(newUpstreamResolverStatuses([]Resolver{bootstrap}))

	return initGroupResolvers(ctx, r, cfg, bootstrap)
}

func (r *DomainShardResolver) setResolvers(resolvers []*upstreamResolverStatus) {
	r.resolvers.Store(&resolvers)
}

func (r *DomainShardResolver) loadResolvers() []*upstreamResolverStatus {
	return *r.resolvers.Load()
}

func (r *DomainShardResolver) Name() string {
	return r.String()
}

func (r *DomainShardResolver) String() string {
	return formatUpstreamResolvers(domainShardResolverType, r.cfg.Name, r.loadResolvers())
}

// shardKey returns the registrable domain (eTLD+1) of the query name, falling back
// to the full name when no eTLD+1 can be derived (e.g. a bare TLD or invalid name).
func shardKey(qName string) string {
	domain := strings.ToLower(strings.TrimSuffix(qName, "."))

	if etld, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		return etld
	}

	return domain
}

// shardSalt returns the current rotation salt: the number of whole rotation
// windows since the epoch. It is constant within a window and increments across
// windows, so the mapping is deterministic per-window yet drifts over time.
//
// Derived purely from the clock, so there is no shared mutable salt to swap and
// the "atomic salt" is inherently lock-free — no rotation goroutine needed.
// ponytail: clock-derived salt instead of a background atomic swap; add a timer
// only if rotation must fire on some signal other than wall-clock windows.
func (r *DomainShardResolver) shardSalt() uint64 {
	hours := r.cfg.DomainShard.RotateHours
	if hours == 0 { // rotation disabled -> stable mapping
		return 0
	}

	return uint64(time.Now().Unix()) / (uint64(hours) * 3600)
}

// shardIndex maps a shard key to an upstream index, mixing the rotation salt into
// the hash so the same key lands on a different upstream once the salt rotates.
func shardIndex(salt uint64, key string, n int) uint64 {
	if n <= 0 {
		// No upstreams to shard across. Callers must handle the empty case, but
		// never let a modulo-by-zero panic escape into the DNS hot path.
		return 0
	}

	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], salt)

	h := fnv.New64a()
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(key))

	return h.Sum64() % uint64(n) //nolint:gosec // n > 0 checked above
}

// Resolve delegates the request to the upstream the query's eTLD+1 hashes to,
// walking the ring to the next upstream on failure.
func (r *DomainShardResolver) Resolve(ctx context.Context, request *model.Request) (*model.Response, error) {
	ctx, logger := r.log(ctx)

	resolvers := *r.resolvers.Load()
	if len(resolvers) == 0 {
		return nil, errors.New("no upstreams available in the domain_shard group")
	}

	start := shardIndex(r.shardSalt(), shardKey(request.Req.Question[0].Name), len(resolvers))

	errs := make([]error, 0, len(resolvers))

	for i := range resolvers {
		res := resolvers[(start+uint64(i))%uint64(len(resolvers))]

		logger.WithField("resolver", res.resolver).Debug("delegating to resolver")

		resp, err := res.resolve(ctx, request) // records the error for the health model
		if err == nil {
			return resp, nil
		}

		errs = append(errs, err)
	}

	return nil, fmt.Errorf("resolution failed: %w", errors.Join(errs...))
}
