package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The system-usage sampler publishes into s.sysUsage; the /api/ui/system handler
// merges it when present and OMITS the fields when nil (the degrade contract the
// UI header relies on to hide itself on non-linux / before the first sample).

func TestSystemEndpointMergesSysUsage(t *testing.T) {
	s := &statsAPI{start: time.Now()}
	s.sysUsage.Store(&sysSnapshot{
		CPUPerCore: []float64{10, 20}, CPUTotal: 15,
		MemUsed: 100, MemTotal: 200, DiskUsed: 300, DiskTotal: 400,
		DiskReadBps: 1024, DiskWriteBps: 512,
	})

	rr := httptest.NewRecorder()
	s.system(rr, httptest.NewRequest(http.MethodGet, "/api/ui/system", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"cpuPerCore", "cpuTotal", "memUsed", "memTotal", "diskUsed", "diskTotal", "diskReadBps", "diskWriteBps"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}

	if m["cpuTotal"].(float64) != 15 {
		t.Errorf("cpuTotal = %v, want 15", m["cpuTotal"])
	}

	if m["memTotal"].(float64) != 200 {
		t.Errorf("memTotal = %v, want 200", m["memTotal"])
	}

	if cores, ok := m["cpuPerCore"].([]any); !ok || len(cores) != 2 {
		t.Errorf("cpuPerCore = %v, want 2 entries", m["cpuPerCore"])
	}
}

func TestSystemEndpointOmitsSysUsageWhenNil(t *testing.T) {
	s := &statsAPI{start: time.Now()} // sysUsage never stored -> nil

	rr := httptest.NewRecorder()
	s.system(rr, httptest.NewRequest(http.MethodGet, "/api/ui/system", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"cpuPerCore", "cpuTotal", "memTotal", "diskReadBps"} {
		if _, ok := m[k]; ok {
			t.Errorf("key %q must be absent when no sample has been published", k)
		}
	}
	// the baseline (always-present) fields still render
	if _, ok := m["version"]; !ok {
		t.Error("baseline field 'version' missing")
	}
}
