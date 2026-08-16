package model

import (
	"crypto/sha1" //nolint:gosec // fingerprint identifier, not a security boundary
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Transport is the wire transport a DNS query arrived over.
type Transport uint8

const (
	TransportDo53UDP Transport = iota
	TransportDo53TCP
	TransportDoT
	TransportDoH
	TransportDoH3
)

func (t Transport) String() string {
	switch t {
	case TransportDo53UDP:
		return "do53-udp"
	case TransportDo53TCP:
		return "do53-tcp"
	case TransportDoT:
		return "dot"
	case TransportDoH:
		return "doh"
	case TransportDoH3:
		return "doh3"
	default:
		return fmt.Sprintf("transport(%d)", uint8(t))
	}
}

// Fingerprint captures client-software-identifying properties of a query,
// snapshotted before the resolver chain mutates the message.
type Fingerprint struct {
	Transport Transport
	SrcPort   uint16

	TLSVersion uint16
	TLSCipher  uint16
	SNI        string
	ALPN       string
	UserAgent  string

	MsgID  uint16
	QClass uint16
	RD     bool
	CD     bool
	AD     bool

	HadEDNS0     bool
	EDNSVersion  uint8
	EDNSUDPSize  uint16
	DO           bool
	EDNSOptCodes []uint16 // wire order — discriminating signal

	HasCookie bool
	Mixed0x20 bool // qname contains uppercase
}

// hasEntropy reports whether the fingerprint carries any device-discriminating
// signal beyond a bare default-stub query. A plain Do53 query with no EDNS, no
// TLS and no User-Agent is content-free: every default resolver on a LAN emits
// the identical shape, so hashing it would collapse physically distinct devices
// — and, worse, their person mappings (Phase 5) — into ONE device_key. Below
// this floor we return no hash so callers fall back to the per-name legacy key
// (see deviceKeyExpr / SampleClient: fp_hash == "" → key on client_name).
//
// ponytail: naive entropy floor. Two devices that DO share an identical wire
// stack (same model, same EDNS/TLS shape) still merge into one key above the
// floor — the accepted fp-cohort ceiling. Tighten only if same-model person
// bleed shows up in practice (would need a per-device secret, not wire fields).
func (f *Fingerprint) hasEntropy() bool {
	return f.HadEDNS0 || f.TLSVersion != 0 || f.UserAgent != "" ||
		(f.Transport != TransportDo53UDP && f.Transport != TransportDo53TCP)
}

// Hash returns a stable identifier over the software-identifying fields only,
// or "" when the fingerprint is below the entropy floor (see hasEntropy) — a
// content-free bare query is keyed by name, never merged by fingerprint.
// Per-query noise (MsgID, SrcPort, SNI, cookie presence, 0x20 casing) is
// deliberately excluded so the same client stack always hashes the same.
func (f *Fingerprint) Hash() string {
	if !f.hasEntropy() {
		return ""
	}

	opts := make([]string, len(f.EDNSOptCodes))
	for i, c := range f.EDNSOptCodes {
		opts[i] = strconv.FormatUint(uint64(c), 10)
	}

	input := fmt.Sprintf("%d|%d|%d|%s|%s|%t|%d|%d|%t|%s|%d|%t|%t",
		f.Transport, f.TLSVersion, f.TLSCipher, f.ALPN, f.UserAgent,
		f.HadEDNS0, f.EDNSVersion, f.EDNSUDPSize, f.DO,
		strings.Join(opts, ","), f.QClass, f.RD, f.CD)

	sum := sha1.Sum([]byte(input)) //nolint:gosec // see import comment

	return hex.EncodeToString(sum[:10])
}
