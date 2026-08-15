package querylog

import (
	"context"
	"time"

	"github.com/0xERR0R/blocky/config"

	"github.com/glebarez/sqlite"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ceWindow spans the whole test data range; every inserted row lands inside it.
var (
	ceFrom = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	ceBase = time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	ceTo   = time.Date(2026, 8, 13, 23, 0, 0, 0, time.UTC)
)

var _ = Describe("client enrichment", func() {
	var reader *Reader

	// ceInsert writes a raw log_entries row directly, bypassing the fingerprint
	// hashing so fp_hash / client_ip / decoy can be set precisely per case.
	ceInsert := func(name, ip, question, fpHash string, decoy bool, offset time.Duration) {
		GinkgoHelper()
		Expect(reader.db.Create(&logEntry{
			RequestTS:    ceBase.Add(offset),
			ClientIP:     ip,
			ClientName:   name,
			QuestionName: question,
			FpHash:       fpHash,
			Decoy:        decoy,
		}).Error).Should(Succeed())
	}

	BeforeEach(func() {
		ctx, cancel := context.WithCancel(context.Background())
		DeferCleanup(cancel)

		writer, e := newDatabaseWriter(ctx, sqlite.Open("file::memory:"), 7, time.Minute, config.QueryLogTypeSqlite)
		Expect(e).Should(Succeed())

		db, e := writer.db.DB()
		Expect(e).Should(Succeed())
		db.SetMaxOpenConns(1)
		DeferCleanup(db.Close)

		reader = &Reader{db: writer.db}
	})

	Context("enrichClients over the whole client set", func() {
		It("returns an empty map on an empty DB", func() {
			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out).Should(BeEmpty())
		})

		It("enriches a single client with one IP and few fingerprints", func() {
			ceInsert("laptop", "10.0.0.5", "www.example.com.", "fp-a", false, 0)
			ceInsert("laptop", "10.0.0.5", "www.example.com.", "fp-b", false, time.Minute)

			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out).Should(HaveKey("laptop"))
			Expect(out["laptop"].IPs).Should(ConsistOf("10.0.0.5"))
			Expect(out["laptop"].FpCount).Should(Equal(2))
			Expect(out["laptop"].NatAggregate).Should(BeFalse())
			Expect(out["laptop"].DeviceGuess).Should(BeEmpty())
		})

		It("collects multiple distinct IPs for one client_name", func() {
			ceInsert("roamer", "10.0.0.1", "www.example.com.", "fp-1", false, 0)
			ceInsert("roamer", "10.0.0.2", "www.example.com.", "fp-1", false, time.Minute)
			ceInsert("roamer", "10.0.0.2", "www.example.com.", "fp-1", false, 2*time.Minute) // dup IP

			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["roamer"].IPs).Should(ConsistOf("10.0.0.1", "10.0.0.2"))
		})
	})

	Context("NAT-aggregate fingerprint threshold (8)", func() {
		insertN := func(name string, n int) {
			for i := range n {
				ceInsert(name, "10.0.0.9", "www.example.com.",
					"fp-"+time.Duration(i).String(), false, time.Duration(i)*time.Minute)
			}
		}

		It("does not flag just-under the threshold (7 distinct fps)", func() {
			insertN("router", 7)
			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["router"].FpCount).Should(Equal(7))
			Expect(out["router"].NatAggregate).Should(BeFalse())
		})

		It("flags exactly-at the threshold (8 distinct fps)", func() {
			insertN("router", 8)
			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["router"].FpCount).Should(Equal(8))
			Expect(out["router"].NatAggregate).Should(BeTrue())
		})

		It("flags over the threshold (9 distinct fps)", func() {
			insertN("router", 9)
			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["router"].NatAggregate).Should(BeTrue())
		})

		It("counts DISTINCT fps only, so many repeats of one fp stay under threshold", func() {
			for i := range 20 {
				ceInsert("chatty", "10.0.0.9", "www.example.com.", "same-fp", false, time.Duration(i)*time.Minute)
			}
			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["chatty"].FpCount).Should(Equal(1))
			Expect(out["chatty"].NatAggregate).Should(BeFalse())
		})
	})

	Context("NULLIF on fp_hash", func() {
		It("does not count empty-string fp_hash toward the distinct fingerprint count", func() {
			ceInsert("blank", "10.0.0.3", "www.example.com.", "", false, 0)
			ceInsert("blank", "10.0.0.3", "www.example.com.", "", false, time.Minute)
			ceInsert("blank", "10.0.0.3", "www.example.com.", "real-fp", false, 2*time.Minute)

			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["blank"].FpCount).Should(Equal(1)) // only "real-fp" counts
		})

		It("reports zero fingerprints when every fp_hash is empty", func() {
			ceInsert("blank", "10.0.0.3", "www.example.com.", "", false, 0)
			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["blank"].FpCount).Should(Equal(0))
			Expect(out["blank"].NatAggregate).Should(BeFalse())
		})
	})

	Context("decoy exclusion", func() {
		It("excludes decoy rows from IPs, fp count, and device guess", func() {
			// A single real fp; the rest are decoys that would otherwise trip the
			// NAT threshold, add a stray IP, and produce a device guess.
			ceInsert("real", "10.0.0.7", "www.example.com.", "real-fp", false, 0)
			for i := range 10 {
				ceInsert("real", "10.9.9.9", "mtalk.google.com.", "decoy-"+time.Duration(i).String(), true,
					time.Duration(i+1)*time.Minute)
			}

			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["real"].FpCount).Should(Equal(1))
			Expect(out["real"].NatAggregate).Should(BeFalse())
			Expect(out["real"].IPs).Should(ConsistOf("10.0.0.7"))
			Expect(out["real"].DeviceGuess).Should(BeEmpty())
		})

		It("drops a client whose rows are all decoys", func() {
			ceInsert("ghost", "10.0.0.8", "www.example.com.", "fp-x", true, 0)
			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out).ShouldNot(HaveKey("ghost"))
		})
	})

	Context("device guessing", func() {
		It("labels a matching question suffix", func() {
			ceInsert("iphone", "10.0.0.4", "1-courier.push.apple.com.", "fp-a", false, 0)
			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["iphone"].DeviceGuess).Should(Equal("Apple device"))
		})

		It("leaves the guess empty when nothing matches", func() {
			ceInsert("mystery", "10.0.0.4", "www.plain-domain.example.", "fp-a", false, 0)
			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["mystery"].DeviceGuess).Should(BeEmpty())
		})

		It("resolves multi-device matches by MAX(label) lexicographically", func() {
			// Xbox and Roku both match; MAX picks "Xbox" ('X' > 'R').
			ceInsert("console", "10.0.0.4", "foo.xboxlive.com.", "fp-a", false, 0)
			ceInsert("console", "10.0.0.4", "foo.roku.com.", "fp-b", false, time.Minute)
			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["console"].DeviceGuess).Should(Equal("Xbox"))
		})
	})

	Context("window bounds", func() {
		It("excludes rows outside [from,to]", func() {
			ceInsert("laptop", "10.0.0.5", "www.example.com.", "fp-a", false, 0)
			// a row a day later, outside ceTo
			Expect(reader.db.Create(&logEntry{
				RequestTS: ceBase.Add(48 * time.Hour), ClientIP: "10.0.0.6",
				ClientName: "laptop", QuestionName: "www.example.com.", FpHash: "fp-late",
			}).Error).Should(Succeed())

			out, err := reader.enrichClients(ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(out["laptop"].FpCount).Should(Equal(1))
			Expect(out["laptop"].IPs).Should(ConsistOf("10.0.0.5"))
		})
	})

	Context("enrichClient for a single client_name", func() {
		It("returns a zero-value enrich for an unknown client", func() {
			e, err := reader.enrichClient("nobody", ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(e.IPs).Should(BeEmpty())
			Expect(e.FpCount).Should(Equal(0))
			Expect(e.NatAggregate).Should(BeFalse())
			Expect(e.DeviceGuess).Should(BeEmpty())
		})

		It("flags NAT aggregate at exactly the threshold and excludes decoys", func() {
			for i := range 8 {
				ceInsert("router", "10.0.0.9", "www.example.com.",
					"fp-"+time.Duration(i).String(), false, time.Duration(i)*time.Minute)
			}
			// a decoy fp that must not push the count higher
			ceInsert("router", "10.0.0.9", "www.example.com.", "decoy-fp", true, 9*time.Minute)

			e, err := reader.enrichClient("router", ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(e.FpCount).Should(Equal(8))
			Expect(e.NatAggregate).Should(BeTrue())
		})

		It("scopes to the named client only", func() {
			ceInsert("laptop", "10.0.0.5", "1-courier.push.apple.com.", "fp-a", false, 0)
			ceInsert("phone", "10.0.0.6", "foo.roku.com.", "fp-b", false, time.Minute)

			e, err := reader.enrichClient("laptop", ceFrom, ceTo)
			Expect(err).Should(Succeed())
			Expect(e.IPs).Should(ConsistOf("10.0.0.5"))
			Expect(e.DeviceGuess).Should(Equal("Apple device"))
		})
	})
})
