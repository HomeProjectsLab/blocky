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

// Hash returns a stable identifier over the software-identifying fields only.
// Per-query noise (MsgID, SrcPort, SNI, cookie presence, 0x20 casing) is
// deliberately excluded so the same client stack always hashes the same.
func (f *Fingerprint) Hash() string {
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
