package querylog

import (
	"testing"
	"time"
)

// TestClassifyVendorTellClasses covers the +8 vendor-tell classes (each device's
// signature → its class) AND guards that the original 10 still classify correctly
// (no regression from the new, higher-priority cases). Pure classFeatures.classify()
// unit test — no DB, mirrors the real scorer path (toFeatures().classify()).
func TestClassifyVendorTellClasses(t *testing.T) {
	const n = 100 // ≥ classMinSamples; makes hits→share arithmetic exact-ish

	// broad+regular base so a device is a plausible browser/beacon UNLESS a vendor
	// tell fires. Domains > classIoTMaxDomains so it can't accidentally be iot, and
	// so mobile's breadth gate is satisfiable when PushHits is set.
	base := classFeatures{N: n, Domains: 20, Qtypes: 4, MeanGap: 60, MeanGap2: 3600}

	with := func(mut func(*classFeatures)) classFeatures {
		f := base
		mut(&f)
		return f
	}

	cases := []struct {
		name string
		f    classFeatures
		want string
	}{
		// --- +8 new classes: just over each threshold → that class ---
		{"nas", with(func(f *classFeatures) { f.NASHits = 5; f.ServerHits = 40 }), ClassNAS},
		{"router", with(func(f *classFeatures) { f.RouterHits = 5; f.ServerHits = 40 }), ClassRouterInfra},
		// media-server is a NARROW appliance (Domains ≤ classIoTMaxDomains); a broad
		// Plex-viewing PC is guarded separately below.
		{"media-server", with(func(f *classFeatures) { f.MediaHits = 8; f.ServerHits = 40; f.Domains = 6 }), ClassMediaServer},
		{"smart-tv", with(func(f *classFeatures) { f.SmartTVHits = 8; f.StreamHits = 90 }), ClassSmartTV},
		{"hub", with(func(f *classFeatures) { f.HubHits = 10 }), ClassSmartHomeHub},
		{"thermostat", with(func(f *classFeatures) { f.ThermostatHits = 10 }), ClassThermostat},
		{"lighting", with(func(f *classFeatures) { f.LightingHits = 12 }), ClassLighting},
		{"wearable", with(func(f *classFeatures) { f.WearableHits = 10 }), ClassWearable},

		// --- ordering guards: the load-bearing "before X" slots ---
		// synology hits serverNameLikes too (server share high) but nas must win.
		{"nas beats server", with(func(f *classFeatures) { f.NASHits = 6; f.ServerHits = 60 }), ClassNAS},
		// smart-tv also streams (stream share > classStreamShare) but tv-telemetry wins.
		{"smart-tv beats stream", with(func(f *classFeatures) { f.SmartTVHits = 9; f.StreamHits = 95 }), ClassSmartTV},

		// --- regression guard: the original 10 unchanged ---
		{"server", with(func(f *classFeatures) { f.ServerHits = 40 }), ClassServer},
		{"game-console", with(func(f *classFeatures) { f.GameHits = 20 }), ClassGameConsole},
		{"camera", with(func(f *classFeatures) { f.CameraHits = 20 }), ClassCamera},
		{"speaker", with(func(f *classFeatures) { f.SpeakerHits = 20 }), ClassSmartSpeaker},
		// printer keeps its low-diversity gate.
		{"printer", with(func(f *classFeatures) { f.PrinterHits = 15; f.Domains = 4 }), ClassPrinter},
		{"tv-streaming", with(func(f *classFeatures) { f.StreamHits = 40 }), ClassTVStreaming},
		{"mobile", with(func(f *classFeatures) { f.PushHits = 3 }), ClassMobile},
		// narrow, regular, low-qtype beacon → iot.
		{"iot", classFeatures{N: n, Domains: 3, Qtypes: 2, MeanGap: 60, MeanGap2: 3660}, ClassIoT},
		// broad, bursty, no vendor tell → workstation.
		{"workstation", classFeatures{N: n, Domains: 40, Qtypes: 6, MeanGap: 60, MeanGap2: 40000}, ClassWorkstation},
		// too few samples → unknown.
		{"unknown", classFeatures{N: classMinSamples - 1, Domains: 20}, ClassUnknown},
		// a workstation that pings a print-cloud once must NOT become printer
		// (share below classPrinterShare) — stays workstation.
		{"no false printer", with(func(f *classFeatures) { f.PrinterHits = 1; f.Domains = 40; f.MeanGap2 = 40000 }), ClassWorkstation},

		// --- overlap-theft guards (the boundary the old guard didn't exercise) ---
		// A broad PC used heavily for Plex viewing polls plex.tv over classMediaShare,
		// but its browsing breadth (Domains > classIoTMaxDomains) must keep it a
		// workstation, not steal it into media-server.
		{"plex-heavy workstation stays workstation",
			with(func(f *classFeatures) { f.MediaHits = 30; f.Domains = 40; f.MeanGap2 = 40000 }), ClassWorkstation},
		// A camera doing DDNS remote-access must NOT be stolen into router-infra:
		// dyndns is no longer a router signal, so RouterHits stays 0 and camera wins.
		{"camera stays camera (no dyndns theft)",
			with(func(f *classFeatures) { f.CameraHits = 20; f.RouterHits = 0 }), ClassCamera},
	}

	for _, tc := range cases {
		if got := tc.f.classify(); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestFoldMapsNewSignals proves a representative new-class vendor domain actually
// increments its accumulator counter (signal catalog → *Hits wiring), so the
// classify() thresholds above are reachable from real question_names.
func TestFoldMapsNewSignals(t *testing.T) {
	checks := []struct {
		qn  string
		get func(*clsAccum) int64
	}{
		{"api.quickconnect.to", func(a *clsAccum) int64 { return a.nas }},
		{"fw.asuscomm.com", func(a *clsAccum) int64 { return a.router }},
		{"plex.tv", func(a *clsAccum) int64 { return a.media }},
		{"cache.lgtvcommon.com", func(a *clsAccum) int64 { return a.smartTV }},
		{"api.smartthings.com", func(a *clsAccum) int64 { return a.hub }},
		{"api.ecobee.com", func(a *clsAccum) int64 { return a.thermostat }},
		{"euw1.tplinkcloud.com", func(a *clsAccum) int64 { return a.lighting }},
		{"api.fitbit.com", func(a *clsAccum) int64 { return a.wearable }},
	}

	for _, c := range checks {
		cls := map[string]*clsAccum{}
		foldClassSignal(cls, "k", &logEntry{QuestionName: c.qn, QuestionType: "A"}, time.Now())
		if got := c.get(cls["k"]); got != 1 {
			t.Errorf("%s: counter = %d, want 1", c.qn, got)
		}
	}

	// Negative guards: the tightened router tokens must NOT match these collisions
	// (the over-broad "ui.com"/"unifi"/"dyndns" tokens used to steal them into
	// router-infra). Each must leave router == 0.
	for _, qn := range []string{
		"unifiedlayer.com",                     // "unifi" collision (Bluehost/EIG hosting)
		"ennui.com",                            // "ui.com" collision
		"sui.com",                              // "ui.com" collision
		"gui.company.com",                      // "ui.com" collision
		"members.dyndns.org", "cam.dyndns.org", // "dyndns" — camera/NAS remote access
	} {
		cls := map[string]*clsAccum{}
		foldClassSignal(cls, "k", &logEntry{QuestionName: qn, QuestionType: "A"}, time.Now())
		if cls["k"].router != 0 {
			t.Errorf("%s: router counter = %d, want 0 (over-broad token regression)", qn, cls["k"].router)
		}
	}

	// And a real Ubiquiti domain still matches via ".ui.com".
	cls := map[string]*clsAccum{}
	foldClassSignal(cls, "k", &logEntry{QuestionName: "device.unifi.ui.com", QuestionType: "A"}, time.Now())
	if cls["k"].router != 1 {
		t.Errorf("device.unifi.ui.com: router counter = %d, want 1", cls["k"].router)
	}
}
