package querylog

import (
	"encoding/json"
	"time"

	"github.com/0xERR0R/blocky/model"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Hub", func() {
	var hub *Hub

	newEntry := func() *LogEntry {
		return &LogEntry{
			Start:        time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC),
			ClientIP:     "192.168.1.10",
			ClientNames:  []string{"laptop"},
			QuestionName: "www.example.com.",
			QuestionType: "A",
			ResponseType: "RESOLVED",
			ResponseCode: "NOERROR",
			DurationMs:   12,
			Fingerprint:  model.Fingerprint{Transport: model.TransportDoH},
		}
	}

	BeforeEach(func() {
		hub = NewHub()
	})

	It("delivers published entries as contract-shaped JSON", func() {
		events, unsubscribe := hub.Subscribe()
		defer unsubscribe()

		hub.Publish(newEntry())

		var data []byte
		Eventually(events).Should(Receive(&data))

		var item map[string]any
		Expect(json.Unmarshal(data, &item)).Should(Succeed())
		Expect(item).Should(HaveKeyWithValue("ts", "2026-08-13T10:00:00Z"))
		Expect(item).Should(HaveKeyWithValue("client", "192.168.1.10"))
		Expect(item).Should(HaveKeyWithValue("clientNames", ConsistOf("laptop")))
		Expect(item).Should(HaveKeyWithValue("question", "www.example.com"))
		Expect(item).Should(HaveKeyWithValue("qtype", "A"))
		Expect(item).Should(HaveKeyWithValue("rtype", "RESOLVED"))
		Expect(item).Should(HaveKeyWithValue("rcode", "NOERROR"))
		Expect(item).Should(HaveKeyWithValue("durationMs", BeNumerically("==", 12)))
		Expect(item).Should(HaveKeyWithValue("transport", "doh"))
		Expect(item).Should(HaveKey("fpHash"))
		Expect(item).Should(HaveKeyWithValue("decoy", BeFalse()))
	})

	It("drops events for a slow subscriber without blocking Publish", func() {
		_, unsubscribe := hub.Subscribe()
		defer unsubscribe()

		done := make(chan struct{})

		go func() {
			defer GinkgoRecover()
			defer close(done)

			// overfill the (256) buffer; must never block even though nobody reads
			for range hubSubBuffer * 2 {
				hub.Publish(newEntry())
			}
		}()

		Eventually(done, "5s").Should(BeClosed())
	})

	It("stops delivery after unsubscribe", func() {
		events, unsubscribe := hub.Subscribe()
		unsubscribe()
		unsubscribe() // idempotent

		hub.Publish(newEntry())

		Consistently(events, "100ms").ShouldNot(Receive())
	})

	It("is a no-op without subscribers and on a nil hub", func() {
		hub.Publish(newEntry())

		var nilHub *Hub
		nilHub.Publish(newEntry()) // must not panic
	})
})
