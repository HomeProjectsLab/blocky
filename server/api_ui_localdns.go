package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/0xERR0R/blocky/configstore"

	"github.com/go-chi/chi/v5"
	"github.com/miekg/dns"
)

// registerLocalDNSUIEndpoints mounts the local-DNS-records editor under
// /api/ui/localdns. store may be nil (503, like the other config endpoints).
func registerLocalDNSUIEndpoints(router *chi.Mux, store *configstore.Store) {
	u := &uiAPI{store: store}

	router.Route("/api/ui/localdns", func(r chi.Router) {
		r.Get("/", u.getLocalDNS)
		r.Put("/", u.putLocalDNS)
	})
}

type localDNSRow struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// TTL is a pointer so an explicit 0 (a legitimate do-not-cache value) is
	// distinguishable from an omitted field: nil defaults to 3600, 0 is honored.
	TTL   *uint32 `json:"ttl"`
	Value string  `json:"value"`
}

func (u *uiAPI) getLocalDNS(rw http.ResponseWriter, _ *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	zone, err := u.store.GetLocalDNSZone()
	if err != nil {
		internalError(rw, err)

		return
	}

	rows := make([]localDNSRow, 0)
	zp := dns.NewZoneParser(strings.NewReader(zone), "", "")

	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		hdr := rr.Header()
		ttl := hdr.Ttl
		rows = append(rows, localDNSRow{
			Name:  hdr.Name,
			Type:  dns.TypeToString[hdr.Rrtype],
			TTL:   &ttl,
			Value: strings.TrimPrefix(rr.String(), hdr.String()),
		})
	}

	// A parse error here means the stored zone is bad; surface the rows we got
	// plus the raw text so the escape-hatch textarea can still fix it.
	writeJSON(rw, http.StatusOK, map[string]any{"records": rows, "zone": zone})
}

func (u *uiAPI) putLocalDNS(rw http.ResponseWriter, req *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	var body struct {
		Records []localDNSRow `json:"records"`
		Zone    *string       `json:"zone"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		badRequest(rw, err)

		return
	}

	var text string
	if body.Zone != nil {
		text = *body.Zone // escape hatch: raw text
	} else {
		text = assembleZone(body.Records)
	}

	if bad, err := validateZone(text); err != nil {
		msg := err.Error()
		if bad != "" {
			msg = fmt.Sprintf("%s (near: %s)", msg, bad)
		}

		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": msg})

		return
	}

	if err := u.store.SetLocalDNSZone(text); err != nil {
		badRequest(rw, err)

		return
	}

	u.store.RequestApply()

	writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
}

// assembleZone turns editor rows into a zone text, auto-fixing the things that
// silently break the loader: trailing dots on names and target hostnames, and
// quoting on TXT rdata. TTL defaults to 3600.
func assembleZone(rows []localDNSRow) string {
	var b strings.Builder

	for _, r := range rows {
		name := fqdn(strings.TrimSpace(r.Name))
		ttl := uint32(3600)
		if r.TTL != nil {
			ttl = *r.TTL // honor an explicit 0 (do-not-cache); only nil defaults
		}

		typ := strings.ToUpper(strings.TrimSpace(r.Type))
		value := strings.TrimSpace(r.Value)

		switch typ {
		case "CNAME", "NS", "PTR":
			value = fqdn(value)
		case "MX":
			value = fqdnToken(value, 1) // "<pref> <host>"
		case "SRV":
			value = fqdnToken(value, 3) // "<pri> <weight> <port> <host>"
		case "TXT":
			if !strings.HasPrefix(value, `"`) {
				value = `"` + value + `"`
			}
		}

		fmt.Fprintf(&b, "%s\t%d\tIN\t%s\t%s\n", name, ttl, typ, value)
	}

	return b.String()
}

// validateZone runs the same parser as the load path to completion. Returns the
// offending record text (best effort) and the error, or ("", nil) if valid.
func validateZone(text string) (string, error) {
	zp := dns.NewZoneParser(strings.NewReader(text), "", "")

	var last string

	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		last = rr.String()
	}

	if err := zp.Err(); err != nil {
		return last, err
	}

	return "", nil
}

// fqdn appends a trailing dot to a hostname unless it already has one or is an
// IP literal.
func fqdn(host string) string {
	if host == "" || strings.HasSuffix(host, ".") || net.ParseIP(host) != nil {
		return host
	}

	return host + "."
}

// fqdnToken fqdn-ifies the i-th whitespace token of value (the target hostname
// in MX/SRV rdata), leaving the rest as-is.
func fqdnToken(value string, i int) string {
	fields := strings.Fields(value)
	if i < len(fields) {
		fields[i] = fqdn(fields[i])
	}

	return strings.Join(fields, " ")
}
