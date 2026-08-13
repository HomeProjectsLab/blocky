package resolver

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
)

const (
	roundRobinResolverType         = "round_robin"
	weightedRoundRobinResolverType = "weighted_round_robin"
)

// RoundRobinResolver cycles through the group's upstreams, one query per upstream in
// turn, spreading the query history evenly across all providers. With weights > 1
// (weighted_round_robin) an upstream gets proportionally more turns.
type RoundRobinResolver struct {
	configurable[*config.UpstreamGroup]
	typed

	state atomic.Pointer[roundRobinState]
	next  atomic.Uint64
}

// roundRobinState pairs the resolver set with its derived schedule so both swap atomically.
type roundRobinState struct {
	resolvers []*upstreamResolverStatus
	// ponytail: weight-expanded schedule; smooth WRR if weights ever exceed ~100.
	schedule []*upstreamResolverStatus
}

// NewRoundRobinResolver creates a new round-robin resolver instance
func NewRoundRobinResolver(
	ctx context.Context, cfg config.UpstreamGroup, bootstrap *Bootstrap,
) (*RoundRobinResolver, error) {
	typeName := roundRobinResolverType
	if cfg.Strategy == config.UpstreamStrategyWeightedRoundRobin {
		typeName = weightedRoundRobinResolverType
	}

	r := &RoundRobinResolver{
		configurable: withConfig(&cfg),
		typed:        withType(typeName),
	}

	// if init strategy is fast, use bootstrap until init finishes
	r.setResolvers(newUpstreamResolverStatuses([]Resolver{bootstrap}))

	return initGroupResolvers(ctx, r, cfg, bootstrap)
}

func (r *RoundRobinResolver) setResolvers(resolvers []*upstreamResolverStatus) {
	schedule := make([]*upstreamResolverStatus, 0, len(resolvers))

	for _, res := range resolvers {
		for range res.staticWeight() {
			schedule = append(schedule, res)
		}
	}

	r.state.Store(&roundRobinState{resolvers: resolvers, schedule: schedule})
}

func (r *RoundRobinResolver) loadResolvers() []*upstreamResolverStatus {
	return r.state.Load().resolvers
}

func (r *RoundRobinResolver) Name() string {
	return r.String()
}

func (r *RoundRobinResolver) String() string {
	return formatUpstreamResolvers(r.Type(), r.cfg.Name, r.loadResolvers())
}

// Resolve delegates the request to the next upstream in the schedule. Upstreams that
// errored within the recent error window are skipped (unless all are unhealthy); on
// failure the next candidate in ring order is tried, so the decaying health model
// keeps working.
func (r *RoundRobinResolver) Resolve(ctx context.Context, request *model.Request) (*model.Response, error) {
	ctx, logger := r.log(ctx)

	state := r.state.Load()
	schedule := state.schedule
	start := r.next.Add(1) - 1

	// candidates in ring order, deduplicated (weights repeat entries back to back)
	candidates := make([]*upstreamResolverStatus, 0, len(state.resolvers))
	healthy := make([]*upstreamResolverStatus, 0, len(state.resolvers))

outer:
	for i := range schedule {
		res := schedule[(start+uint64(i))%uint64(len(schedule))]

		for _, seen := range candidates {
			if seen == res {
				continue outer
			}
		}

		candidates = append(candidates, res)

		if res.isHealthy() {
			healthy = append(healthy, res)
		}
	}

	if len(healthy) > 0 {
		candidates = healthy
	}

	errs := make([]error, 0, len(candidates))

	for _, res := range candidates {
		logger.WithField("resolver", res.resolver).Debug("delegating to resolver")

		resp, err := res.resolve(ctx, request)
		if err == nil {
			return resp, nil
		}

		errs = append(errs, err)
	}

	return nil, fmt.Errorf("resolution failed: %w", errors.Join(errs...))
}
