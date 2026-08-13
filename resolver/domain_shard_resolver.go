package resolver

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync/atomic"

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

// Resolve delegates the request to the upstream the query's eTLD+1 hashes to,
// walking the ring to the next upstream on failure.
func (r *DomainShardResolver) Resolve(ctx context.Context, request *model.Request) (*model.Response, error) {
	ctx, logger := r.log(ctx)

	resolvers := *r.resolvers.Load()

	h := fnv.New64a()
	_, _ = h.Write([]byte(shardKey(request.Req.Question[0].Name)))
	start := h.Sum64() % uint64(len(resolvers))

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
