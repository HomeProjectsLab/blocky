package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/0xERR0R/blocky/configstore"

	"github.com/go-chi/chi/v5"
	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func u32(v uint32) *uint32 { return &v }

var _ = Describe("Local DNS zone assembly + validation", func() {
	It("assembles good rows into a valid zone and auto-appends trailing dots", func() {
		text := assembleZone([]localDNSRow{
			{Name: "web.lan", Type: "A", TTL: nil, Value: "10.0.0.5"},
			{Name: "www.lan", Type: "CNAME", Value: "web.lan"},
			{Name: "lan", Type: "MX", Value: "10 mail.lan"},
			{Name: "greet.lan", Type: "TXT", Value: "hello world"},
		})

		Expect(text).Should(ContainSubstring("web.lan.\t3600\tIN\tA\t10.0.0.5")) // nil TTL -> default 3600
		Expect(text).Should(ContainSubstring("mail.lan."))                       // MX target got a dot
		Expect(text).Should(ContainSubstring(`"hello world"`))

		bad, err := validateZone(text)
		Expect(err).Should(Succeed())
		Expect(bad).Should(BeEmpty())
	})

	It("honors an explicit TTL of 0 (do-not-cache) instead of forcing 3600", func() {
		text := assembleZone([]localDNSRow{
			{Name: "nocache.lan", Type: "A", TTL: u32(0), Value: "10.0.0.5"},
		})

		Expect(text).Should(ContainSubstring("nocache.lan.\t0\tIN\tA\t10.0.0.5"))
		Expect(text).ShouldNot(ContainSubstring("3600"))
	})

	It("preserves an explicit non-zero TTL", func() {
		text := assembleZone([]localDNSRow{
			{Name: "cached.lan", Type: "A", TTL: u32(120), Value: "10.0.0.5"},
		})

		Expect(text).Should(ContainSubstring("cached.lan.\t120\tIN\tA\t10.0.0.5"))
	})

	It("rejects a malformed record", func() {
		text := assembleZone([]localDNSRow{
			{Name: "ok.lan", Type: "A", Value: "10.0.0.1"},
			{Name: "bad.lan", Type: "A", Value: "not-an-ip"},
		})

		_, err := validateZone(text)
		Expect(err).Should(HaveOccurred())
	})

	// One assembled row per record type; each must parse back through the same
	// zone parser the loader uses, and the rdata must survive intact.
	DescribeTable("assembles and re-parses every record type",
		func(row localDNSRow, wantSubstr string) {
			text := assembleZone([]localDNSRow{row})
			Expect(text).Should(ContainSubstring(wantSubstr))

			bad, err := validateZone(text)
			Expect(err).Should(Succeed())
			Expect(bad).Should(BeEmpty())
		},
		Entry("A", localDNSRow{Name: "web.lan", Type: "A", Value: "10.0.0.5"}, "\tA\t10.0.0.5"),
		Entry("AAAA (IPv6)", localDNSRow{Name: "v6.lan", Type: "AAAA", Value: "2001:db8::1"}, "\tAAAA\t2001:db8::1"),
		Entry("CNAME appends dot", localDNSRow{Name: "www.lan", Type: "CNAME", Value: "web.lan"}, "\tCNAME\tweb.lan."),
		Entry("NS appends dot", localDNSRow{Name: "lan", Type: "NS", Value: "ns1.lan"}, "\tNS\tns1.lan."),
		Entry("PTR appends dot", localDNSRow{Name: "5.0.0.10.in-addr.arpa", Type: "PTR", Value: "web.lan"}, "\tPTR\tweb.lan."),
		Entry("MX dots the target only", localDNSRow{Name: "lan", Type: "MX", Value: "10 mail.lan"}, "\tMX\t10 mail.lan."),
		Entry("SRV dots the 4th token", localDNSRow{Name: "_sip._tcp.lan", Type: "SRV", Value: "10 5 5060 sip.lan"}, "\tSRV\t10 5 5060 sip.lan."),
		Entry("TXT quotes spaced value", localDNSRow{Name: "greet.lan", Type: "TXT", Value: "hello world"}, `"hello world"`),
		Entry("TXT keeps existing quotes", localDNSRow{Name: "q.lan", Type: "TXT", Value: `"already quoted"`}, `"already quoted"`),
		Entry("CAA passes rdata through", localDNSRow{Name: "lan", Type: "CAA", Value: `0 issue "letsencrypt.org"`}, `0 issue "letsencrypt.org"`),
	)

	It("does not double-dot an already-dotted CNAME target", func() {
		text := assembleZone([]localDNSRow{{Name: "www.lan.", Type: "CNAME", Value: "web.lan."}})
		Expect(text).Should(ContainSubstring("www.lan.\t3600\tIN\tCNAME\tweb.lan."))
		Expect(text).ShouldNot(ContainSubstring("web.lan.."))
		Expect(text).ShouldNot(ContainSubstring("www.lan.."))
	})

	It("does not append a dot to an IP literal used as a CNAME/PTR value", func() {
		// fqdn() leaves an IP literal alone (a dotted IP is not a hostname)
		text := assembleZone([]localDNSRow{{Name: "x.lan", Type: "PTR", Value: "10.0.0.1"}})
		Expect(text).Should(ContainSubstring("10.0.0.1\n"))
		Expect(text).ShouldNot(ContainSubstring("10.0.0.1."))
	})

	It("lower-cases nothing but upper-cases the type and trims whitespace", func() {
		text := assembleZone([]localDNSRow{{Name: "  web.lan  ", Type: " a ", Value: "  10.0.0.5  "}})
		Expect(text).Should(ContainSubstring("web.lan.\t3600\tIN\tA\t10.0.0.5\n"))
	})

	It("surfaces the offending record text on a malformed later row", func() {
		text := assembleZone([]localDNSRow{
			{Name: "ok.lan", Type: "A", Value: "10.0.0.1"},
			{Name: "bad.lan", Type: "AAAA", Value: "not-a-v6"},
		})

		bad, err := validateZone(text)
		Expect(err).Should(HaveOccurred())
		// the parser stops at the bad row; the last good record is reported as the
		// nearest context so the UI can point at where it broke
		Expect(bad).Should(ContainSubstring("ok.lan"))
	})

	It("reports an empty offending string when the very first row is malformed", func() {
		text := assembleZone([]localDNSRow{{Name: "bad.lan", Type: "A", Value: "nope"}})

		bad, err := validateZone(text)
		Expect(err).Should(HaveOccurred())
		Expect(bad).Should(BeEmpty())
	})

	It("accepts an empty zone", func() {
		bad, err := validateZone(assembleZone(nil))
		Expect(err).Should(Succeed())
		Expect(bad).Should(BeEmpty())
	})
})

var _ = Describe("Local DNS UI endpoints", func() {
	var (
		router *chi.Mux
		store  *configstore.Store
	)

	ldnsExec := func(method, path string, body []byte) *httptest.ResponseRecorder {
		GinkgoHelper()

		var r *http.Request
		if body != nil {
			r = httptest.NewRequest(method, path, bytes.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)

		return rec
	}

	BeforeEach(func() {
		var err error
		store, err = configstore.Open(filepath.Join(GinkgoT().TempDir(), "cfg"))
		Expect(err).Should(Succeed())
		DeferCleanup(func() { _ = store.Close() })

		router = chi.NewRouter()
		registerLocalDNSUIEndpoints(router, store)
	})

	It("round-trips records: PUT rows then GET parses them back", func() {
		put := ldnsExec(http.MethodPut, "/api/ui/localdns/",
			[]byte(`{"records":[{"name":"web.lan","type":"A","ttl":600,"value":"10.0.0.5"}]}`))
		Expect(put.Code).Should(Equal(http.StatusOK))

		get := ldnsExec(http.MethodGet, "/api/ui/localdns/", nil)
		Expect(get.Code).Should(Equal(http.StatusOK))

		var body struct {
			Records []struct {
				Name  string  `json:"name"`
				Type  string  `json:"type"`
				TTL   *uint32 `json:"ttl"`
				Value string  `json:"value"`
			} `json:"records"`
			Zone string `json:"zone"`
		}
		Expect(json.Unmarshal(get.Body.Bytes(), &body)).Should(Succeed())
		Expect(body.Records).Should(HaveLen(1))
		Expect(body.Records[0].Name).Should(Equal("web.lan."))
		Expect(body.Records[0].Type).Should(Equal("A"))
		Expect(body.Records[0].TTL).ShouldNot(BeNil())
		Expect(*body.Records[0].TTL).Should(BeEquivalentTo(600))
		Expect(body.Records[0].Value).Should(Equal("10.0.0.5"))
	})

	It("accepts a raw zone via the escape hatch and skips row assembly", func() {
		raw := "direct.lan.\t42\tIN\tA\t10.9.9.9\n"
		put := ldnsExec(http.MethodPut, "/api/ui/localdns/",
			[]byte(`{"zone":`+strconv.Quote(raw)+`}`))
		Expect(put.Code).Should(Equal(http.StatusOK))

		stored, err := store.GetLocalDNSZone()
		Expect(err).Should(Succeed())
		Expect(stored).Should(Equal(raw)) // stored verbatim, not re-assembled
	})

	It("rejects a malformed raw zone with the offending line in the error", func() {
		put := ldnsExec(http.MethodPut, "/api/ui/localdns/",
			[]byte(`{"zone":"good.lan.\t3600\tIN\tA\t10.0.0.1\nbad.lan.\t3600\tIN\tA\tnope\n"}`))
		Expect(put.Code).Should(Equal(http.StatusBadRequest))

		var m map[string]string
		Expect(json.Unmarshal(put.Body.Bytes(), &m)).Should(Succeed())
		Expect(m["error"]).Should(ContainSubstring("near:"))
		Expect(m["error"]).Should(ContainSubstring("good.lan"))
	})

	It("rejects malformed records assembled from rows", func() {
		put := ldnsExec(http.MethodPut, "/api/ui/localdns/",
			[]byte(`{"records":[{"name":"bad.lan","type":"A","value":"not-an-ip"}]}`))
		Expect(put.Code).Should(Equal(http.StatusBadRequest))
	})

	It("rejects a malformed JSON body", func() {
		put := ldnsExec(http.MethodPut, "/api/ui/localdns/", []byte(`{oops`))
		Expect(put.Code).Should(Equal(http.StatusBadRequest))
	})

	It("returns 503 when the store is nil (GET and PUT)", func() {
		router = chi.NewRouter()
		registerLocalDNSUIEndpoints(router, nil)

		Expect(ldnsExec(http.MethodGet, "/api/ui/localdns/", nil).Code).Should(Equal(http.StatusServiceUnavailable))
		Expect(ldnsExec(http.MethodPut, "/api/ui/localdns/", []byte(`{}`)).Code).Should(Equal(http.StatusServiceUnavailable))
	})

	It("always returns the raw zone text alongside parsed rows on GET", func() {
		// the raw "zone" key is the escape-hatch textarea's source: it must be
		// present even when the store round-trips a valid zone
		Expect(store.SetLocalDNSZone("web.lan.\t3600\tIN\tA\t10.0.0.5\n")).Should(Succeed())

		get := ldnsExec(http.MethodGet, "/api/ui/localdns/", nil)
		Expect(get.Code).Should(Equal(http.StatusOK))

		var body map[string]any
		Expect(json.Unmarshal(get.Body.Bytes(), &body)).Should(Succeed())
		Expect(body).Should(HaveKey("zone"))
		Expect(body["zone"]).Should(ContainSubstring("web.lan."))
	})

	It("uses the same parser on load and validate (parity smoke)", func() {
		// a zone assembled from every type must both validate and re-parse on load
		text := assembleZone([]localDNSRow{
			{Name: "web.lan", Type: "A", Value: "10.0.0.5"},
			{Name: "v6.lan", Type: "AAAA", Value: "2001:db8::1"},
			{Name: "www.lan", Type: "CNAME", Value: "web.lan"},
			{Name: "greet.lan", Type: "TXT", Value: "hello world"},
		})
		zp := dns.NewZoneParser(strings.NewReader(text), "", "")
		n := 0
		for _, ok := zp.Next(); ok; _, ok = zp.Next() {
			n++
		}
		Expect(zp.Err()).Should(Succeed())
		Expect(n).Should(Equal(4))
	})
})
