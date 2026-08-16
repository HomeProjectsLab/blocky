//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package querylog

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// realRow mirrors the columns of log_entries that the replay sampler reads, so
// the test can seed the replay pool without depending on the writer internals.
type realRow struct {
	// mirror the composite index too: the client-scoped samplers pin it with
	// INDEXED BY (a hard requirement, not a hint), so the seed table must carry it.
	RequestTS     time.Time `gorm:"column:request_ts;index:idx_client_name_request_ts,priority:2"`
	ClientIP      string    `gorm:"column:client_ip"`
	ClientName    string    `gorm:"column:client_name;index:idx_client_name_request_ts,priority:1"`
	QuestionName  string    `gorm:"column:question_name"`
	QuestionType  string    `gorm:"column:question_type"`
	ResponseType  string    `gorm:"column:response_type"`
	EffectiveTLDP string    `gorm:"column:effective_tldp"`
	FpHash        string    `gorm:"column:fp_hash;index"`
	Decoy         bool      `gorm:"column:decoy"`
	EDNSUDPSize   uint16    `gorm:"column:edns_udp_size"`
	EDNSOptCodes  string    `gorm:"column:edns_opt_codes"`
	FpDetail      string    `gorm:"column:fp_detail"`
}

func (realRow) TableName() string { return "log_entries" }

var _ = Describe("DecoySource", func() {
	var (
		path   string
		source *DecoySource
	)

	BeforeEach(func() {
		path = filepath.Join(GinkgoT().TempDir(), "decoy.db")

		// create log_entries so the replay sampler has a table to read
		raw, e := gorm.Open(sqlite.Open(path), &gorm.Config{})
		Expect(e).Should(Succeed())
		Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())

		now := time.Now()
		Expect(raw.Create(&[]realRow{
			{RequestTS: now, QuestionName: "real1.com", QuestionType: "A", Decoy: false},
			{RequestTS: now, QuestionName: "real2.com", QuestionType: "AAAA", Decoy: false},
			{RequestTS: now, QuestionName: "adecoy.com", QuestionType: "A", Decoy: true},
			{RequestTS: now.Add(-30 * 24 * time.Hour), QuestionName: "old.com", QuestionType: "A", Decoy: false},
		}).Error).Should(Succeed())

		sqlDB, _ := raw.DB()
		Expect(sqlDB.Close()).Should(Succeed())

		source, err = NewDecoySource(path)
		Expect(err).Should(Succeed())
		DeferCleanup(func() { _ = source.Close() })
	})

	Describe("SeedIfEmpty + SampleList", func() {
		It("seeds a tiny list and samples domains from it", func() {
			n, e := source.SeedIfEmpty(strings.NewReader("a.com\nb.com\nc.com\n"))
			Expect(e).Should(Succeed())
			Expect(n).Should(Equal(3))

			for range 20 {
				d, e := source.SampleList()
				Expect(e).Should(Succeed())
				Expect(d).Should(BeElementOf("a.com", "b.com", "c.com"))
			}
		})

		It("skips blank lines and does not reseed a populated table", func() {
			n, e := source.SeedIfEmpty(strings.NewReader("a.com\n\n\nb.com\n"))
			Expect(e).Should(Succeed())
			Expect(n).Should(Equal(2))

			n2, e := source.SeedIfEmpty(strings.NewReader("x.com\ny.com\n"))
			Expect(e).Should(Succeed())
			Expect(n2).Should(Equal(0)) // already seeded
		})

		It("returns empty string when the list is not seeded", func() {
			d, e := source.SampleList()
			Expect(e).Should(Succeed())
			Expect(d).Should(BeEmpty())
		})
	})

	Describe("SampleRealFingerprint", func() {
		It("returns the full wire shape parsed from columns + fp_detail", func() {
			// Fresh DB with a single distinctive real row so the random sample is
			// deterministic (only one non-decoy row in the window).
			p := filepath.Join(GinkgoT().TempDir(), "fp.db")
			raw, e := gorm.Open(sqlite.Open(p), &gorm.Config{})
			Expect(e).Should(Succeed())
			Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())
			Expect(raw.Create(&realRow{
				RequestTS: time.Now(), QuestionName: "fp.example", QuestionType: "AAAA",
				EDNSUDPSize: 4096, EDNSOptCodes: "10,3",
				FpDetail: `{"qclass":1,"do":true,"hadEdns0":true,"hasCookie":true,"mixed0x20":true}`,
			}).Error).Should(Succeed())
			sqlDB, _ := raw.DB()
			Expect(sqlDB.Close()).Should(Succeed())

			src, e := NewDecoySource(p)
			Expect(e).Should(Succeed())
			DeferCleanup(func() { _ = src.Close() })

			fp, e := src.SampleRealFingerprint()
			Expect(e).Should(Succeed())
			Expect(fp.Qtype).Should(Equal("AAAA"))
			Expect(fp.EDNSUDPSize).Should(Equal(uint16(4096)))
			Expect(fp.EDNSOptCodes).Should(Equal([]uint16{10, 3})) // wire order preserved
			Expect(fp.HadEDNS0).Should(BeTrue())
			Expect(fp.DO).Should(BeTrue())
			Expect(fp.HasCookie).Should(BeTrue())
			Expect(fp.Mixed0x20).Should(BeTrue())
			Expect(fp.QClass).Should(Equal(uint16(1)))
		})

		It("cold start (no real rows) yields the zero value, no error", func() {
			p := filepath.Join(GinkgoT().TempDir(), "empty.db")
			raw, e := gorm.Open(sqlite.Open(p), &gorm.Config{})
			Expect(e).Should(Succeed())
			Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())
			sqlDB, _ := raw.DB()
			Expect(sqlDB.Close()).Should(Succeed())

			src, e := NewDecoySource(p)
			Expect(e).Should(Succeed())
			DeferCleanup(func() { _ = src.Close() })

			fp, e := src.SampleRealFingerprint()
			Expect(e).Should(Succeed())
			Expect(fp.HadEDNS0).Should(BeFalse())
			Expect(fp.EDNSOptCodes).Should(BeEmpty())
		})
	})

	Describe("SampleRecentReal", func() {
		It("only ever samples recent non-decoy queries", func() {
			// Independent random-rowid draws (one per requested item), so with only
			// two eligible rows a limit of 10 yields repeats — but never the decoy
			// row nor the 30-day-old row (both excluded by the per-draw predicate).
			rows, e := source.SampleRecentReal(10)
			Expect(e).Should(Succeed())
			Expect(rows).ShouldNot(BeEmpty())

			for _, r := range rows {
				Expect(r.Name).Should(BeElementOf("real1.com", "real2.com"))
			}
		})
	})

	Describe("noise corpus (T3)", func() {
		It("upserts real non-blocked queries and samples them back", func() {
			now := time.Now()
			Expect(upsertNoiseCorpus(source.db, []*logEntry{
				{QuestionName: "corpus1.com", RequestTS: now},
				{QuestionName: "corpus1.com", RequestTS: now}, // second hit bumps count
				{QuestionName: "corpus2.com", RequestTS: now},
				{QuestionName: "blocked.com", RequestTS: now, ResponseType: "BLOCKED"}, // skipped
				{QuestionName: "adecoy.com", RequestTS: now, Decoy: true},              // skipped
				{QuestionName: "", RequestTS: now},                                     // skipped
			})).Should(Succeed())

			var hits int64
			Expect(source.db.Raw("SELECT hits FROM noise_corpus WHERE domain = ?", "corpus1.com").
				Scan(&hits).Error).Should(Succeed())
			Expect(hits).Should(Equal(int64(2)))

			var n int64
			Expect(source.db.Raw("SELECT COUNT(*) FROM noise_corpus").Scan(&n).Error).Should(Succeed())
			Expect(n).Should(Equal(int64(2))) // only the two real, non-blocked domains

			for range 20 {
				d, e := source.SampleCorpus()
				Expect(e).Should(Succeed())
				Expect(d).Should(BeElementOf("corpus1.com", "corpus2.com"))
			}
		})

		It("returns empty string on an empty corpus", func() {
			d, e := source.SampleCorpus()
			Expect(e).Should(Succeed())
			Expect(d).Should(BeEmpty())
		})

		It("LRU-prunes down to the cap by last_seen", func() {
			orig := noiseCorpusCap
			noiseCorpusCap = 5
			DeferCleanup(func() { noiseCorpusCap = orig })

			base := time.Now().Add(-time.Hour)

			entries := make([]*logEntry, 0, 8)
			for i := range 8 {
				entries = append(entries, &logEntry{
					QuestionName: "c" + strconv.Itoa(i) + ".com",
					RequestTS:    base.Add(time.Duration(i) * time.Minute), // c0 oldest, c7 newest
				})
			}
			Expect(upsertNoiseCorpus(source.db, entries)).Should(Succeed())

			Expect(pruneNoiseCorpus(source.db)).Should(Succeed())

			var n int64
			Expect(source.db.Raw("SELECT COUNT(*) FROM noise_corpus").Scan(&n).Error).Should(Succeed())
			Expect(n).Should(Equal(int64(5)))

			// the 3 oldest (c0,c1,c2) are gone; the 5 newest survive
			var survivors []string
			Expect(source.db.Raw("SELECT domain FROM noise_corpus ORDER BY domain").Scan(&survivors).Error).Should(Succeed())
			Expect(survivors).Should(ConsistOf("c3.com", "c4.com", "c5.com", "c6.com", "c7.com"))
		})
	})

	Describe("IsBlockedDomain + block exclusion (T6)", func() {
		BeforeEach(func() {
			_, e := source.SeedBlocklistIfEmpty("ads", strings.NewReader("bad.com\n"))
			Expect(e).Should(Succeed())
		})

		It("is true for a seeded blocklist domain, false otherwise", func() {
			b, e := source.IsBlockedDomain("bad.com")
			Expect(e).Should(Succeed())
			Expect(b).Should(BeTrue())

			b, e = source.IsBlockedDomain("good.com")
			Expect(e).Should(Succeed())
			Expect(b).Should(BeFalse())
		})

		It("SampleList never returns a blocked domain", func() {
			_, e := source.SeedIfEmpty(strings.NewReader("bad.com\ngood1.com\ngood2.com\ngood3.com\n"))
			Expect(e).Should(Succeed())

			for range 100 {
				d, e := source.SampleList()
				Expect(e).Should(Succeed())
				Expect(d).ShouldNot(Equal("bad.com"))
			}
		})
	})

	Describe("SampleList rank-weighting (T7)", func() {
		It("skews draws toward low rowids (popular ranks)", func() {
			p := filepath.Join(GinkgoT().TempDir(), "rank.db")
			raw, e := gorm.Open(sqlite.Open(p), &gorm.Config{})
			Expect(e).Should(Succeed())
			Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())
			sqlDB, _ := raw.DB()
			Expect(sqlDB.Close()).Should(Succeed())

			src, e := NewDecoySource(p)
			Expect(e).Should(Succeed())
			DeferCleanup(func() { _ = src.Close() })

			const total = 1000

			var b strings.Builder
			for i := 1; i <= total; i++ {
				b.WriteString("r" + strconv.Itoa(i) + ".com\n") // rowid i == rank i
			}
			_, e = src.SeedIfEmpty(strings.NewReader(b.String()))
			Expect(e).Should(Succeed())

			topDecile := 0
			const draws = 2000
			for range draws {
				d, e := src.SampleList()
				Expect(e).Should(Succeed())
				rank, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(d, "r"), ".com"))
				if rank <= total/10 {
					topDecile++
				}
			}

			// Zipf s≈1 puts ~ln(100)/ln(1000) ≈ 0.67 of draws in the top decile;
			// uniform would give 0.10. Loose lower bound well clear of uniform.
			Expect(float64(topDecile) / float64(draws)).Should(BeNumerically(">", 0.4))
		})
	})

	Describe("SampleFingerprintForName (T13)", func() {
		It("returns the name's own eTLD+1 fingerprint when present, else falls back", func() {
			p := filepath.Join(GinkgoT().TempDir(), "fpname.db")
			raw, e := gorm.Open(sqlite.Open(p), &gorm.Config{})
			Expect(e).Should(Succeed())
			Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())
			Expect(raw.Create(&[]realRow{
				{
					RequestTS: time.Now(), QuestionName: "api.example.com", QuestionType: "AAAA",
					EffectiveTLDP: "example.com", EDNSUDPSize: 1232, EDNSOptCodes: "10,8",
					FpDetail: `{"hadEdns0":true}`,
				},
				{
					RequestTS: time.Now(), QuestionName: "other.net", QuestionType: "A",
					EffectiveTLDP: "other.net", EDNSUDPSize: 512,
				},
			}).Error).Should(Succeed())
			sqlDB, _ := raw.DB()
			Expect(sqlDB.Close()).Should(Succeed())

			src, e := NewDecoySource(p)
			Expect(e).Should(Succeed())
			DeferCleanup(func() { _ = src.Close() })

			// a subdomain of a known eTLD+1 gets that eTLD+1's stable shape
			fp, e := src.SampleFingerprintForName("cdn.example.com")
			Expect(e).Should(Succeed())
			Expect(fp.EDNSUDPSize).Should(Equal(uint16(1232)))
			Expect(fp.EDNSOptCodes).Should(Equal([]uint16{10, 8}))
			Expect(fp.HadEDNS0).Should(BeTrue())

			// an unseen eTLD+1 falls back to a random real fp (one of the two rows)
			fp, e = src.SampleFingerprintForName("nothing.here.org")
			Expect(e).Should(Succeed())
			Expect(fp.EDNSUDPSize).Should(BeElementOf(uint16(1232), uint16(512)))
		})
	})

	// seedLog rebuilds a fresh DB with the given real rows and returns an open
	// DecoySource over it. Used by the cohort/session/revisit specs, which need
	// crafted client_name / response_type / effective_tldp timelines.
	seedLog := func(name string, rows []realRow) *DecoySource {
		p := filepath.Join(GinkgoT().TempDir(), name)
		raw, e := gorm.Open(sqlite.Open(p), &gorm.Config{})
		Expect(e).Should(Succeed())
		Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())
		if len(rows) > 0 {
			Expect(raw.Create(&rows).Error).Should(Succeed())
		}
		sqlDB, _ := raw.DB()
		Expect(sqlDB.Close()).Should(Succeed())

		src, e := NewDecoySource(p)
		Expect(e).Should(Succeed())
		DeferCleanup(func() { _ = src.Close() })

		// The raw seed above bypasses the write path, so fold the same rows into the
		// durable heuristics accumulator the way doDBWrite would — this is what the
		// repointed (accumulator-based) RefreshClientClasses now scores from.
		if len(rows) > 0 {
			batch := make([]*logEntry, len(rows))
			for i, r := range rows {
				batch[i] = &logEntry{
					RequestTS: r.RequestTS, ClientName: r.ClientName, QuestionName: r.QuestionName,
					QuestionType: r.QuestionType, ResponseType: r.ResponseType,
					EffectiveTLDP: r.EffectiveTLDP, FpHash: r.FpHash, Decoy: r.Decoy,
				}
			}
			Expect(upsertHeuristics(src.db, batch)).Should(Succeed())
		}

		return src
	}

	Describe("SampleCohort (7G)", func() {
		It("groups a same-client burst in time order, includes a blocked member, excludes other clients", func() {
			now := time.Now()
			src := seedLog("cohort.db", []realRow{
				{RequestTS: now, ClientName: "h1", QuestionName: "page.com", QuestionType: "A", EffectiveTLDP: "page.com"},
				{RequestTS: now.Add(300 * time.Millisecond), ClientName: "h1", QuestionName: "cdn.page.com", QuestionType: "AAAA"},
				{RequestTS: now.Add(500 * time.Millisecond), ClientName: "h1", QuestionName: "tracker.com", QuestionType: "A", ResponseType: "BLOCKED"},
				// different client inside the same window — must be excluded
				{RequestTS: now.Add(200 * time.Millisecond), ClientName: "h2", QuestionName: "other.com", QuestionType: "A"},
			})

			// SampleCohort seeds on a RANDOM real row, so it may return either
			// h1's burst or h2's singleton. Sample repeatedly: capture h1's cohort
			// to assert its structure, and assert on EVERY draw that clients are
			// never mixed (exclusion holds regardless of which seed was picked).
			var burst []CohortMember

			for range 60 {
				mem, e := src.SampleCohort()
				Expect(e).Should(Succeed())

				switch {
				case len(mem) == 1:
					Expect(mem[0].Domain).Should(Equal("other.com")) // h2's singleton, never mixed
				case len(mem) == 3:
					burst = mem // h1's three rows, no h2 row mixed in
				default:
					Fail("cohort mixed clients or wrong size: had length " + strconv.Itoa(len(mem)))
				}
			}

			Expect(burst).ShouldNot(BeNil(), "expected to sample h1's 3-member cohort within 60 draws")
			Expect(burst[0].Domain).Should(Equal("page.com")) // primary first
			Expect(burst[0].DelayMs).Should(Equal(0))
			Expect(burst[0].Qtype).Should(Equal(uint16(1))) // A
			Expect(burst[1].Domain).Should(Equal("cdn.page.com"))
			Expect(burst[1].Qtype).Should(Equal(uint16(28))) // AAAA
			Expect(burst[1].DelayMs).Should(BeNumerically("~", 300, 5))
			Expect(burst[2].Domain).Should(Equal("tracker.com"))
			Expect(burst[2].Blocked).Should(BeTrue()) // blocked member kept
			Expect(burst[2].DelayMs).Should(BeNumerically("~", 500, 5))
		})

		It("returns nil on a cold start (no real rows)", func() {
			src := seedLog("cohort_empty.db", nil)
			mem, e := src.SampleCohort()
			Expect(e).Should(Succeed())
			Expect(mem).Should(BeNil())
		})
	})

	Describe("NextInSession / SessionSeed (#1)", func() {
		It("returns the frequent successor and \"\" for an unknown primary", func() {
			now := time.Now().Add(-time.Hour)
			// h1: a->b twice (two sessions), a->c once. b should dominate.
			src := seedLog("session.db", []realRow{
				{RequestTS: now, ClientName: "h1", QuestionName: "a.com", EffectiveTLDP: "a.com"},
				{RequestTS: now.Add(1 * time.Minute), ClientName: "h1", QuestionName: "b.com", EffectiveTLDP: "b.com"},
				{RequestTS: now.Add(2 * time.Hour), ClientName: "h1", QuestionName: "a.com", EffectiveTLDP: "a.com"},
				{RequestTS: now.Add(2*time.Hour + time.Minute), ClientName: "h1", QuestionName: "b.com", EffectiveTLDP: "b.com"},
				{RequestTS: now.Add(4 * time.Hour), ClientName: "h1", QuestionName: "a.com", EffectiveTLDP: "a.com"},
				{RequestTS: now.Add(4*time.Hour + time.Minute), ClientName: "h1", QuestionName: "c.com", EffectiveTLDP: "c.com"},
			})

			// transitions are materialized on the class-refresh timer, not per emit
			Expect(src.RefreshClientClasses()).Should(Succeed())

			for range 30 {
				nxt, e := src.NextInSession("a.com")
				Expect(e).Should(Succeed())
				Expect(nxt).Should(BeElementOf("b.com", "c.com"))
			}

			nxt, e := src.NextInSession("zzz.com")
			Expect(e).Should(Succeed())
			Expect(nxt).Should(BeEmpty())
		})

		It("does not learn a transition across an idle longer than the session gap", func() {
			now := time.Now().Add(-time.Hour)
			// a -> b but 45min apart: beyond sessionGap, so no transition.
			src := seedLog("session_gap.db", []realRow{
				{RequestTS: now, ClientName: "h1", QuestionName: "a.com", EffectiveTLDP: "a.com"},
				{RequestTS: now.Add(45 * time.Minute), ClientName: "h1", QuestionName: "b.com", EffectiveTLDP: "b.com"},
			})

			Expect(src.RefreshClientClasses()).Should(Succeed())

			nxt, e := src.NextInSession("a.com")
			Expect(e).Should(Succeed())
			Expect(nxt).Should(BeEmpty())
		})

		It("SessionSeed returns a session-starting primary", func() {
			now := time.Now().Add(-time.Hour)
			src := seedLog("seed.db", []realRow{
				{RequestTS: now, ClientName: "h1", QuestionName: "start.com", EffectiveTLDP: "start.com"},
				{RequestTS: now.Add(time.Minute), ClientName: "h1", QuestionName: "mid.com", EffectiveTLDP: "mid.com"},
			})

			Expect(src.RefreshClientClasses()).Should(Succeed())

			seed, e := src.SessionSeed()
			Expect(e).Should(Succeed())
			Expect(seed).Should(Equal("start.com")) // only the first row starts a session
		})
	})

	Describe("RevisitInterval (#5)", func() {
		It("returns ~the median gap for a domain with several visits", func() {
			now := time.Now().Add(-6 * time.Hour)
			// visits at 0, +1h, +2h, +4h -> deltas 1h,1h,2h -> median 1h
			src := seedLog("revisit.db", []realRow{
				{RequestTS: now, ClientName: "h1", QuestionName: "news.com", EffectiveTLDP: "news.com"},
				{RequestTS: now.Add(1 * time.Hour), ClientName: "h1", QuestionName: "news.com", EffectiveTLDP: "news.com"},
				{RequestTS: now.Add(2 * time.Hour), ClientName: "h1", QuestionName: "news.com", EffectiveTLDP: "news.com"},
				{RequestTS: now.Add(4 * time.Hour), ClientName: "h1", QuestionName: "news.com", EffectiveTLDP: "news.com"},
			})

			d, ok := src.RevisitInterval("www.news.com")
			Expect(ok).Should(BeTrue())
			Expect(d).Should(BeNumerically("~", time.Hour, 2*time.Minute))
		})

		It("collapses an intra-page-load burst and reports ok=false for a single visit", func() {
			now := time.Now().Add(-time.Hour)
			// three queries within a second == one visit -> no revisit interval
			src := seedLog("revisit_single.db", []realRow{
				{RequestTS: now, ClientName: "h1", QuestionName: "solo.com", EffectiveTLDP: "solo.com"},
				{RequestTS: now.Add(200 * time.Millisecond), ClientName: "h1", QuestionName: "solo.com", EffectiveTLDP: "solo.com"},
				{RequestTS: now.Add(500 * time.Millisecond), ClientName: "h1", QuestionName: "solo.com", EffectiveTLDP: "solo.com"},
			})

			_, ok := src.RevisitInterval("solo.com")
			Expect(ok).Should(BeFalse())
		})
	})

	Describe("AddToCorpus (TASK P)", func() {
		It("inserts a domain and refreshes hits on re-add", func() {
			src := seedLog("addcorpus.db", nil)

			Expect(src.AddToCorpus("warm.com")).Should(Succeed())
			Expect(src.AddToCorpus("warm.com")).Should(Succeed())

			var hits int64
			Expect(src.db.Raw("SELECT hits FROM noise_corpus WHERE domain = ?", "warm.com").
				Scan(&hits).Error).Should(Succeed())
			Expect(hits).Should(Equal(int64(2)))

			d, e := src.SampleCorpus()
			Expect(e).Should(Succeed())
			Expect(d).Should(Equal("warm.com"))
		})
	})

	Describe("client classification (TASK M)", func() {
		// buildClientTimeline emits n rows for one client: an IoT beacon (periodic,
		// cycling through `domains`) or a browsing burst (many domains). Returns the
		// crafted rows for seedLog.
		beacon := func(client, ip string, domains []string, n int, start time.Time, period time.Duration) []realRow {
			rows := make([]realRow, 0, n)
			for i := range n {
				d := domains[i%len(domains)]
				rows = append(rows, realRow{
					RequestTS: start.Add(time.Duration(i) * period), ClientIP: ip, ClientName: client,
					QuestionName: d, QuestionType: "A", EffectiveTLDP: d,
				})
			}

			return rows
		}

		It("classifies a periodic 2-domain client as iot, a browser as workstation, a registry-heavy client as server; override wins", func() {
			now := time.Now()
			var rows []realRow

			// iot: 2 domains, periodic 5-min beacon, only A queries.
			rows = append(rows, beacon("iotdev", "10.0.0.5",
				[]string{"telemetry.tuya.com", "time.nist.gov"}, 40, now.Add(-8*time.Hour), 5*time.Minute)...)

			// workstation: many distinct browsing domains, mixed qtypes, bursty.
			qtypes := []string{"A", "AAAA", "HTTPS"}
			for i := range 60 {
				d := fmt.Sprintf("site%d.com", i)
				// cluster three per burst then jump ahead → high inter-arrival CoV
				rows = append(rows, realRow{
					RequestTS: now.Add(-6*time.Hour + time.Duration(i)*7*time.Minute + time.Duration(i%3)*time.Second),
					ClientIP:  "10.0.0.20", ClientName: "laptop",
					QuestionName: d, QuestionType: qtypes[i%3], EffectiveTLDP: d,
				})
			}

			// server: registry/update-heavy.
			srv := []string{"docker.io", "ghcr.io", "deb.debian.org", "registry.npmjs.org", "github.com"}
			for i := range 50 {
				d := srv[i%len(srv)]
				rows = append(rows, realRow{
					RequestTS: now.Add(-4*time.Hour + time.Duration(i)*3*time.Minute),
					ClientIP:  "10.0.0.30", ClientName: "buildbox",
					QuestionName: d, QuestionType: "A", EffectiveTLDP: effectiveTLDP(d),
				})
			}

			src := seedLog("classify.db", rows)
			Expect(src.RefreshClientClasses()).Should(Succeed())

			c, e := src.ClientClass("iotdev")
			Expect(e).Should(Succeed())
			Expect(c).Should(Equal(ClassIoT))

			c, e = src.ClientClass("laptop")
			Expect(e).Should(Succeed())
			Expect(c).Should(Equal(ClassWorkstation))

			c, e = src.ClientClass("buildbox")
			Expect(e).Should(Succeed())
			Expect(c).Should(Equal(ClassServer))

			// override wins over auto, and "auto"/"" clears it.
			Expect(src.SetClientClassOverride("iotdev", ClassServer)).Should(Succeed())
			c, _ = src.ClientClass("iotdev")
			Expect(c).Should(Equal(ClassServer))

			list, e := src.ListClientClasses()
			Expect(e).Should(Succeed())
			var iot ClientClassInfo
			for _, r := range list {
				if r.Client == "iotdev" {
					iot = r
				}
			}
			Expect(iot.Class).Should(Equal(ClassIoT)) // auto unchanged
			Expect(iot.Override).Should(Equal(ClassServer))
			Expect(iot.Effective).Should(Equal(ClassServer))

			Expect(src.SetClientClassOverride("iotdev", "auto")).Should(Succeed())
			c, _ = src.ClientClass("iotdev")
			Expect(c).Should(Equal(ClassIoT))
		})

		It("classifies a client with too few queries as unknown", func() {
			now := time.Now()
			src := seedLog("classify_sparse.db",
				beacon("sparse", "10.0.0.9", []string{"a.com"}, 5, now.Add(-time.Hour), time.Minute))
			Expect(src.RefreshClientClasses()).Should(Succeed())

			c, e := src.ClientClass("sparse")
			Expect(e).Should(Succeed())
			Expect(c).Should(Equal(ClassUnknown))
		})

		It("returns unknown for an unclassified client and stores an override set before refresh", func() {
			src := seedLog("classify_pre.db", nil)

			c, e := src.ClientClass("ghost")
			Expect(e).Should(Succeed())
			Expect(c).Should(Equal(ClassUnknown))

			Expect(src.SetClientClassOverride("ghost", ClassIoT)).Should(Succeed())
			c, _ = src.ClientClass("ghost")
			Expect(c).Should(Equal(ClassIoT))
		})

		It("rejects an invalid override class", func() {
			src := seedLog("classify_bad.db", nil)
			Expect(src.SetClientClassOverride("x", "toaster")).Should(MatchError(ContainSubstring("invalid device class")))
		})

		It("stores, reads, sanitizes and clears a manual display name", func() {
			src := seedLog("names.db", nil)

			n, e := src.ClientName("laptop")
			Expect(e).Should(Succeed())
			Expect(n).Should(BeEmpty()) // none set yet

			// control chars stripped, trimmed, capped at 63 runes
			Expect(src.SetClientName("laptop", "  Alex's iPhone\n\x00  ")).Should(Succeed())
			n, _ = src.ClientName("laptop")
			Expect(n).Should(Equal("Alex's iPhone"))

			Expect(src.SetClientName("laptop", strings.Repeat("x", 100))).Should(Succeed())
			n, _ = src.ClientName("laptop")
			Expect([]rune(n)).Should(HaveLen(63))

			names, e := src.ClientNames()
			Expect(e).Should(Succeed())
			Expect(names).Should(HaveKey("laptop"))

			// blank clears; then it drops out of the bulk map
			Expect(src.SetClientName("laptop", "   ")).Should(Succeed())
			n, _ = src.ClientName("laptop")
			Expect(n).Should(BeEmpty())
			names, _ = src.ClientNames()
			Expect(names).ShouldNot(HaveKey("laptop"))

			Expect(src.SetClientName("", "x")).Should(MatchError(ContainSubstring("must not be empty")))
		})

		It("samples a client of a given effective class for attribution", func() {
			now := time.Now()
			rows := beacon("iotdev", "10.0.0.5",
				[]string{"telemetry.tuya.com", "time.nist.gov"}, 40, now.Add(-8*time.Hour), 5*time.Minute)
			src := seedLog("classify_sample.db", rows)
			Expect(src.RefreshClientClasses()).Should(Succeed())

			p, e := src.SampleClientOfClass(ClassIoT)
			Expect(e).Should(Succeed())
			Expect(p.IP).Should(Equal("10.0.0.5"))

			// no server present → empty persona (caller falls back)
			p, e = src.SampleClientOfClass(ClassServer)
			Expect(e).Should(Succeed())
			Expect(p.IP).Should(BeEmpty())
		})

		// fpBeacon: an IoT-shaped periodic beacon carrying an explicit fingerprint
		// hash, at a given IP/name. Same shape as beacon() but sets fp_hash + lets the
		// caller vary the (ip, name) pair to simulate a DHCP lease change / roam.
		fpBeacon := func(ip, name, fp string, domains []string, n int, start time.Time) []realRow {
			rows := make([]realRow, 0, n)
			for i := range n {
				rows = append(rows, realRow{
					RequestTS: start.Add(time.Duration(i) * 5 * time.Minute),
					ClientIP:  ip, ClientName: name, FpHash: fp,
					QuestionName: domains[i%len(domains)], QuestionType: "A",
					EffectiveTLDP: domains[i%len(domains)],
				})
			}

			return rows
		}

		It("keeps class/person/persona associated to the SAME fingerprint across an IP change (goal 3)", func() {
			now := time.Now()
			const fp = "aabbccddeeff00112233" // 20-hex, shape of a real fp_hash
			iotDomains := []string{"telemetry.tuya.com", "time.nist.gov"}

			// Same device, same fingerprint, two different IPs/names (the DHCP/roam
			// case: client_name falls back to the IP string, so both change together).
			// Contiguous 5-min beacons so the cohort stays cleanly IoT-shaped.
			var rows []realRow
			rows = append(rows, fpBeacon("10.0.0.5", "10.0.0.5", fp, iotDomains, 40, now.Add(-8*time.Hour))...)
			rows = append(rows, fpBeacon("10.0.0.99", "10.0.0.99", fp, iotDomains, 40, now.Add(-8*time.Hour+205*time.Minute))...)

			src := seedLog("ipindep.db", rows)
			Expect(src.RefreshClientClasses()).Should(Succeed())

			// (a) the stable device_key is the fingerprint, identical under both IPs.
			keyA := src.deviceKey("10.0.0.5")
			keyB := src.deviceKey("10.0.0.99")
			Expect(keyA).Should(Equal(fp))
			Expect(keyB).Should(Equal(keyA), "device_key must not depend on client_ip")

			// (b) class resolves the same under the old and the new IP.
			cA, e := src.ClientClass("10.0.0.5")
			Expect(e).Should(Succeed())
			cB, e := src.ClientClass("10.0.0.99")
			Expect(e).Should(Succeed())
			Expect(cA).Should(Equal(ClassIoT))
			Expect(cB).Should(Equal(cA), "class must survive an IP change")

			// (c) a person mapped under the OLD IP is found under the NEW IP.
			Expect(src.SetClientPerson("10.0.0.5", "Alex")).Should(Succeed())
			pB, e := src.ClientPerson("10.0.0.99")
			Expect(e).Should(Succeed())
			Expect(pB).Should(Equal("Alex"), "person mapping must follow the fingerprint, not the IP")

			// (d) the sampled persona keys on the stable fingerprint regardless of
			// which IP the sampled row happened to carry.
			persona, e := src.SampleClientOfClass(ClassIoT)
			Expect(e).Should(Succeed())
			Expect(persona.Key).Should(Equal(fp))
			Expect(persona.IP).Should(BeElementOf("10.0.0.5", "10.0.0.99"))
		})

		It("classifies a console-backend cohort as game-console and a camera-backend cohort as camera", func() {
			now := time.Now()

			// game-console: dominated by xbox live telemetry.
			game := fpBeacon("10.0.0.40", "10.0.0.40", "1111111111aaaaaaaaaa",
				[]string{"title.xboxlive.com", "notify.xboxlive.com"}, 30, now.Add(-6*time.Hour))
			// camera: dominated by Hikvision cloud.
			cam := fpBeacon("10.0.0.41", "10.0.0.41", "2222222222bbbbbbbbbb",
				[]string{"dev.hik-connect.com", "push.hik-connect.com"}, 30, now.Add(-6*time.Hour))

			src := seedLog("newclasses.db", append(game, cam...))
			Expect(src.RefreshClientClasses()).Should(Succeed())

			c, e := src.ClientClass("10.0.0.40")
			Expect(e).Should(Succeed())
			Expect(c).Should(Equal(ClassGameConsole))

			c, e = src.ClientClass("10.0.0.41")
			Expect(e).Should(Succeed())
			Expect(c).Should(Equal(ClassCamera))
		})

		It("migrates a legacy name-keyed class override onto the fp device_key (no silent revert on upgrade)", func() {
			now := time.Now()
			const fp = "3333333333cccccccccc"
			rows := fpBeacon("10.0.0.42", "10.0.0.42", fp,
				[]string{"telemetry.tuya.com", "time.nist.gov"}, 40, now.Add(-8*time.Hour))
			src := seedLog("classmig.db", rows)

			// simulate a pre-upgrade override stored under the OLD client_name key
			// (auto class IoT for this cohort; user manually pinned it to server).
			Expect(src.db.Exec(
				`INSERT INTO client_class (client, class, override, updated_at) VALUES (?, ?, ?, ?)`,
				"10.0.0.42", "", ClassServer, now).Error).Should(Succeed())

			Expect(src.RefreshClientClasses()).Should(Succeed())

			// override must now resolve under the stable fp key AND via the name.
			c, e := src.ClientClassByKey(fp)
			Expect(e).Should(Succeed())
			Expect(c).Should(Equal(ClassServer), "override must follow the fp key, not silently revert to auto")

			c, e = src.ClientClass("10.0.0.42")
			Expect(e).Should(Succeed())
			Expect(c).Should(Equal(ClassServer))

			// the stale name-keyed row is gone (can't shadow-revert later).
			var n int64
			Expect(src.db.Raw(`SELECT COUNT(*) FROM client_class WHERE client = ?`, "10.0.0.42").
				Scan(&n).Error).Should(Succeed())
			Expect(n).Should(BeZero())
		})
	})
})
