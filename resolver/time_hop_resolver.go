package resolver

import (
	"context"
	"errors"
	"math/rand"
	"slices"
	"sync/atomic"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
)

const timeHopResolverType = "time_hop"

// TimeHopResolver sticks to one random upstream for a random interval
// (uniform in [hopMin, hopMax]) and then hops to another one, so no single
// upstream sees the full query history for long. On a query failure it hops
// to a different random upstream immediately.
type TimeHopResolver struct {
	configurable[*config.UpstreamGroup]
	typed

	resolvers atomic.Pointer[[]*upstreamResolverStatus]
	current   atomic.Pointer[timeHopState]

	hopMin, hopMax time.Duration
}

type timeHopState struct {
	status   *upstreamResolverStatus
	deadline time.Time
}

// NewTimeHopResolver creates a new time-hop resolver instance
func NewTimeHopResolver(
	ctx context.Context, cfg config.UpstreamGroup, bootstrap *Bootstrap,
) (*TimeHopResolver, error) {
	settings := cfg.GroupSettings(cfg.Name)

	r := &TimeHopResolver{
		configurable: withConfig(&cfg),
		typed:        withType(timeHopResolverType),

		hopMin: settings.HopMin.ToDuration(),
		hopMax: settings.HopMax.ToDuration(),
	}

	// if init strategy is fast, use bootstrap until init finishes
	r.setResolvers(newUpstreamResolverStatuses([]Resolver{bootstrap}))

	return initGroupResolvers(ctx, r, cfg, bootstrap)
}

func (r *TimeHopResolver) setResolvers(resolvers []*upstreamResolverStatus) {
	r.resolvers.Store(&resolvers)
	r.current.Store(nil) // force a fresh pick from the new set
}

func (r *TimeHopResolver) loadResolvers() []*upstreamResolverStatus {
	return *r.resolvers.Load()
}

func (r *TimeHopResolver) Name() string {
	return r.String()
}

func (r *TimeHopResolver) String() string {
	return formatUpstreamResolvers(timeHopResolverType, r.cfg.Name, r.loadResolvers())
}

// hop picks a random healthy upstream (skipping exclude, if possible) and sticks to it
// for a random duration in [hopMin, hopMax]. Concurrent hops are harmless: last write wins.
func (r *TimeHopResolver) hop(
	resolvers []*upstreamResolverStatus, exclude *upstreamResolverStatus,
) *upstreamResolverStatus {
	var excluded []*upstreamResolverStatus
	if exclude != nil && len(resolvers) > 1 {
		excluded = []*upstreamResolverStatus{exclude}
	}

	status := weightedRandom(resolvers, excluded)

	stick := r.hopMin
	if r.hopMax > r.hopMin {
		stick += time.Duration(rand.Int63n(int64(r.hopMax - r.hopMin))) //nolint:gosec // pseudo-randomness is fine
	}

	r.current.Store(&timeHopState{status: status, deadline: time.Now().Add(stick)})

	return status
}

// Resolve delegates the request to the currently sticky upstream, hopping to a new
// random one when the stick interval expired or the current query fails.
func (r *TimeHopResolver) Resolve(ctx context.Context, request *model.Request) (*model.Response, error) {
	ctx, logger := r.log(ctx)

	resolvers := *r.resolvers.Load()

	status := r.currentStatus(resolvers)
	if status == nil {
		status = r.hop(resolvers, nil)
	}

	logger.WithField("resolver", status.resolver).Debug("delegating to resolver")

	resp, err := status.resolve(ctx, request) // records the error for the health model
	if err == nil {
		return resp, nil
	}

	// failure: hop to a different upstream immediately and retry once
	next := r.hop(resolvers, status)
	if next == status {
		return nil, err
	}

	logger.WithField("resolver", next.resolver).Debug("hopping to resolver after failure")

	resp, retryErr := next.resolve(ctx, request)
	if retryErr != nil {
		return nil, errors.Join(err, retryErr)
	}

	return resp, nil
}

// currentStatus returns the sticky upstream if it is still valid: deadline not
// passed and still part of the (possibly swapped) resolver set.
func (r *TimeHopResolver) currentStatus(resolvers []*upstreamResolverStatus) *upstreamResolverStatus {
	state := r.current.Load()
	if state == nil || time.Now().After(state.deadline) {
		return nil
	}

	if slices.Contains(resolvers, state.status) {
		return state.status
	}

	return nil
}
