package resolver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/util"
	"github.com/sirupsen/logrus"
)

const (
	upstreamTreeResolverType = "upstream_tree"

	// replaceUpstreamsCloseDelay is the grace period before the old upstream clients
	// are closed after a runtime swap, so in-flight queries can finish.
	replaceUpstreamsCloseDelay = 10 * time.Second
)

// upstreamGroupResolver is implemented by all per-group strategy resolvers and
// allows swapping their upstream set at runtime.
type upstreamGroupResolver interface {
	Resolver

	setResolvers(resolvers []*upstreamResolverStatus)
	loadResolvers() []*upstreamResolverStatus
}

var (
	_ upstreamGroupResolver = (*ParallelBestResolver)(nil)
	_ upstreamGroupResolver = (*StrictResolver)(nil)
	_ upstreamGroupResolver = (*RoundRobinResolver)(nil)
	_ upstreamGroupResolver = (*TimeHopResolver)(nil)
	_ upstreamGroupResolver = (*DomainShardResolver)(nil)
)

type UpstreamTreeResolver struct {
	configurable[*config.Upstreams]
	typed

	branches  map[string]Resolver
	bootstrap *Bootstrap
}

func NewUpstreamTreeResolver(ctx context.Context, cfg config.Upstreams, bootstrap *Bootstrap) (Resolver, error) {
	if len(cfg.Groups[upstreamDefaultCfgName]) == 0 {
		return nil, fmt.Errorf("no external DNS resolvers configured as default upstream resolvers. "+
			"Please configure at least one under '%s' configuration name", upstreamDefaultCfgName)
	}

	branches, err := createUpstreamBranches(ctx, cfg, bootstrap)
	if err != nil {
		return nil, err
	}

	// return resolver that forwards request to specific resolver branch depending on the client.
	// The tree is kept even for a single group so it stays the stable target for runtime
	// upstream swaps (ReplaceUpstreams).
	r := UpstreamTreeResolver{
		configurable: withConfig(&cfg),
		typed:        withType(upstreamTreeResolverType),

		branches:  branches,
		bootstrap: bootstrap,
	}

	return &r, nil
}

// ReplaceUpstreams swaps the upstream set of one group at runtime, without a server
// rebuild: the new upstreams are built and validated first (per the configured init
// strategy), then atomically replace the old set; the old upstream clients are closed
// after a grace period so in-flight queries can finish.
func (r *UpstreamTreeResolver) ReplaceUpstreams(
	ctx context.Context, group string, upstreams []config.Upstream,
) error {
	branch, ok := r.branches[group]
	if !ok {
		return fmt.Errorf("unknown upstream group '%s'", group)
	}

	gr, ok := branch.(upstreamGroupResolver)
	if !ok {
		return fmt.Errorf("group '%s' resolver (%s) does not support upstream replacement", group, branch.Type())
	}

	groupConfig := config.NewUpstreamGroup(group, *r.cfg, upstreams)
	groupConfig.Strategy = r.cfg.EffectiveStrategy(group)

	newResolvers, err := createGroupResolvers(ctx, groupConfig, r.bootstrap)
	if err != nil {
		return fmt.Errorf("failed to create new upstreams for group '%s': %w", group, err)
	}

	old := gr.loadResolvers()
	gr.setResolvers(newResolvers)

	time.AfterFunc(replaceUpstreamsCloseDelay, func() {
		for _, status := range old {
			if closer, ok := status.resolver.(io.Closer); ok {
				_ = closer.Close()
			}
		}
	})

	return nil
}

func createUpstreamBranches(
	ctx context.Context, cfg config.Upstreams, bootstrap *Bootstrap,
) (map[string]Resolver, error) {
	branches := make(map[string]Resolver, len(cfg.Groups))
	errs := make([]error, 0, len(cfg.Groups))

	for group, upstreams := range cfg.Groups {
		var (
			upstream Resolver
			err      error
		)

		strategy := cfg.EffectiveStrategy(group)

		groupConfig := config.NewUpstreamGroup(group, cfg, upstreams)
		// per-group override: the group resolver only sees its own strategy
		groupConfig.Strategy = strategy

		switch strategy {
		case config.UpstreamStrategyParallelBest,
			config.UpstreamStrategyRandom,
			config.UpstreamStrategyWeightedRandom:
			upstream, err = NewParallelBestResolver(ctx, groupConfig, bootstrap)
		case config.UpstreamStrategyStrict:
			upstream, err = NewStrictResolver(ctx, groupConfig, bootstrap)
		case config.UpstreamStrategyRoundRobin,
			config.UpstreamStrategyWeightedRoundRobin:
			upstream, err = NewRoundRobinResolver(ctx, groupConfig, bootstrap)
		case config.UpstreamStrategyTimeHop:
			upstream, err = NewTimeHopResolver(ctx, groupConfig, bootstrap)
		case config.UpstreamStrategyDomainShard:
			upstream, err = NewDomainShardResolver(ctx, groupConfig, bootstrap)
		case config.UpstreamStrategyRecursive:
			err = errors.New("recursive strategy lands in Phase 4")
		}

		if err != nil {
			errs = append(errs, fmt.Errorf("group %s: %w", group, err))

			continue
		}

		branches[group] = upstream
	}

	if len(errs) != 0 {
		return nil, errors.Join(errs...)
	}

	return branches, nil
}

func (r *UpstreamTreeResolver) Name() string {
	return r.String()
}

func (r *UpstreamTreeResolver) String() string {
	result := make([]string, 0, len(r.branches))

	for group, res := range r.branches {
		result = append(result, fmt.Sprintf("%s (%s)", group, res.Type()))
	}

	return fmt.Sprintf("%s upstreams %q", upstreamTreeResolverType, strings.Join(result, ", "))
}

func (r *UpstreamTreeResolver) Resolve(ctx context.Context, request *model.Request) (*model.Response, error) {
	ctx, logger := r.log(ctx)

	group := r.upstreamGroupByClient(logger, request)

	// delegate request to group resolver
	logger.WithField("resolver", fmt.Sprintf("%s (%s)", group, r.branches[group].Type())).Debug("delegating to resolver")

	resp, err := r.branches[group].Resolve(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("upstream resolution failed for group '%s': %w", group, err)
	}

	return resp, nil
}

func (r *UpstreamTreeResolver) upstreamGroupByClient(logger *logrus.Entry, request *model.Request) string {
	groups := make([]string, 0, len(r.branches))
	clientIP := request.ClientIP.String()

	// try IP
	if _, exists := r.branches[clientIP]; exists {
		return clientIP
	}

	// try client names
	for _, name := range request.ClientNames {
		for group := range r.branches {
			if util.ClientNameMatchesGroupName(group, name) {
				groups = append(groups, group)
			}
		}
	}

	// try CIDR (only if no client name matched)
	if len(groups) == 0 {
		for cidr := range r.branches {
			if util.CidrContainsIP(cidr, request.ClientIP) {
				groups = append(groups, cidr)
			}
		}
	}

	if len(groups) > 0 {
		if len(groups) > 1 {
			logger.WithFields(logrus.Fields{
				"clientNames": request.ClientNames,
				"clientIP":    clientIP,
				"groups":      groups,
			}).Warn("client matches multiple groups")
		}

		return groups[0]
	}

	return upstreamDefaultCfgName
}
