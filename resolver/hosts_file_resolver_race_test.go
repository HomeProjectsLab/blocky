package resolver

import (
	"context"
	"sync"
	"testing"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/mock"
)

// TestHostsFileResolverConcurrentRefresh asserts (under -race) that the periodic
// refresh swapping r.hosts does not race with concurrent Resolve reads.
func TestHostsFileResolverConcurrentRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := config.HostsFile{
		Sources:  []config.BytesSource{config.TextBytesSource("192.0.2.1 race.example.com")},
		HostsTTL: config.Duration(60),
		Loading: config.SourceLoading{
			RefreshPeriod:      -1,
			MaxErrorsPerSource: 5,
		},
	}

	sut, err := NewHostsFileResolver(ctx, cfg, systemResolverBootstrap)
	if err != nil {
		t.Fatalf("NewHostsFileResolver: %v", err)
	}

	next := &mockResolver{}
	next.On("Resolve", mock.Anything).Return(&model.Response{Res: new(dns.Msg)}, nil)
	sut.Next(next)

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		for i := 0; i < 100; i++ {
			if err := sut.loadSources(ctx); err != nil {
				t.Errorf("loadSources: %v", err)

				return
			}
		}
	}()

	go func() {
		defer wg.Done()

		for i := 0; i < 100; i++ {
			if _, err := sut.Resolve(ctx, newRequest("race.example.com.", dns.Type(dns.TypeA))); err != nil {
				t.Errorf("Resolve: %v", err)

				return
			}
		}
	}()

	wg.Wait()
}
