package querylog

import "testing"

// Plain self-check for BuildPersonaRollup's folding logic (per-person sums,
// class histogram, shared-facet blanking, TZ hour rotation). Kept framework-free.
func TestBuildPersonaRollup(t *testing.T) {
	var utc [24]int
	utc[0] = 5 // 00:00 UTC

	clients := []ClientRow{
		{Name: "phone", Queries: 100, Blocked: 10, OS: "iOS", Vendor: []string{"Apple"}},
		{Name: "laptop", Queries: 200, Blocked: 20, OS: "macOS", Vendor: []string{"Apple"}},
		{Name: "nat-box", Queries: 50, Blocked: 5, OS: "Linux", NatAggregate: true},
		{Name: "orphan", Queries: 7},
	}
	classes := []ClientClassInfo{
		{Client: "phone", Effective: "iot"},
		{Client: "laptop", Effective: "workstation"},
	}
	persons := map[string]string{"phone": "Alex", "laptop": "Alex"}
	names := map[string]string{"phone": "Alex's iPhone"}
	profiles := map[string]ClientProfileInfo{"phone": {HourHistUTC: utc}}

	pr := BuildPersonaRollup(clients, classes, persons, names, profiles, nil, "Europe/Zurich")

	if !pr.Enabled {
		t.Fatal("Enabled should be true")
	}
	// Alex = phone+laptop: 300 queries, two classes.
	if len(pr.People) != 1 || pr.People[0].Person != "Alex" {
		t.Fatalf("want one person Alex, got %+v", pr.People)
	}
	if pr.People[0].Queries != 300 || pr.People[0].Blocked != 30 {
		t.Fatalf("person sums wrong: %+v", pr.People[0])
	}
	if pr.People[0].Classes["iot"] != 1 || pr.People[0].Classes["workstation"] != 1 {
		t.Fatalf("person class mix wrong: %+v", pr.People[0].Classes)
	}
	// nat-box + orphan are unassigned.
	if len(pr.Unassigned) != 2 {
		t.Fatalf("want 2 unassigned, got %v", pr.Unassigned)
	}
	// SharedSplit: nat-box is shared, the other three single.
	if pr.SharedSplit.Shared != 1 || pr.SharedSplit.Single != 3 {
		t.Fatalf("shared split wrong: %+v", pr.SharedSplit)
	}
	// NAT facets blanked: Linux OS must NOT appear in the OS histogram.
	for _, o := range pr.OS {
		if o.Name == "Linux" {
			t.Fatal("shared/NAT OS facet leaked into histogram")
		}
	}
	// display-name overlay wins.
	var phone *PersonaClient
	for i := range pr.Clients {
		if pr.Clients[i].Name == "phone" {
			phone = &pr.Clients[i]
		}
	}
	if phone == nil || phone.DisplayName != "Alex's iPhone" {
		t.Fatalf("display name overlay failed: %+v", phone)
	}
	// TZ rotation: Zurich is UTC+1/+2, so the 00:00-UTC count lands at hour 1 or 2, not 0.
	if phone.HourLocal[0] != 0 {
		t.Fatalf("presence not localized off UTC hour 0: %v", phone.HourLocal)
	}
	if phone.HourLocal[1]+phone.HourLocal[2] != 5 {
		t.Fatalf("localized presence lost the count: %v", phone.HourLocal)
	}
	// unknown default class present for the unclassified assigned/unassigned rows.
	var sawUnknown bool
	for _, c := range pr.Classes {
		if c.Name == ClassUnknown {
			sawUnknown = true
		}
	}
	if !sawUnknown {
		t.Fatal("expected an 'unknown' class bucket for unclassified rows")
	}
}
