package util

import (
	"fmt"
	"net"
	"net/http"
)

//nolint:gochecknoglobals
var baseTransport *http.Transport

//nolint:gochecknoinits
func init() {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic(fmt.Errorf(
			"unsupported Go version: http.DefaultTransport is not of type *http.Transport: it is a %T",
			http.DefaultTransport,
		))
	}

	baseTransport = base
}

// DefaultHTTPTransport returns a new Transport with the same defaults as net/http.
func DefaultHTTPTransport() *http.Transport {
	return &http.Transport{
		DialContext:            baseTransport.DialContext,
		DialTLSContext:         baseTransport.DialTLSContext,
		DisableCompression:     baseTransport.DisableCompression,
		DisableKeepAlives:      baseTransport.DisableKeepAlives,
		ExpectContinueTimeout:  baseTransport.ExpectContinueTimeout,
		ForceAttemptHTTP2:      baseTransport.ForceAttemptHTTP2,
		GetProxyConnectHeader:  baseTransport.GetProxyConnectHeader,
		IdleConnTimeout:        baseTransport.IdleConnTimeout,
		MaxConnsPerHost:        baseTransport.MaxConnsPerHost,
		MaxIdleConns:           baseTransport.MaxIdleConns,
		MaxIdleConnsPerHost:    baseTransport.MaxConnsPerHost,
		MaxResponseHeaderBytes: baseTransport.MaxResponseHeaderBytes,
		OnProxyConnectResponse: baseTransport.OnProxyConnectResponse,
		Proxy:                  baseTransport.Proxy,
		ProxyConnectHeader:     baseTransport.ProxyConnectHeader,
		ReadBufferSize:         baseTransport.ReadBufferSize,
		ResponseHeaderTimeout:  baseTransport.ResponseHeaderTimeout,
		TLSClientConfig:        baseTransport.TLSClientConfig,
		TLSHandshakeTimeout:    baseTransport.TLSHandshakeTimeout,
		TLSNextProto:           baseTransport.TLSNextProto,
		WriteBufferSize:        baseTransport.WriteBufferSize,
	}
}

// HTTPClientIP extracts the client IP address from an HTTP request's
// RemoteAddr. Forwarding headers (RFC 7239 Forwarded / X-Forwarded-For) are
// deliberately NOT consulted: they are client-controlled, so trusting them
// lets a DoH client rotate its rate-limit bucket at will and impersonate other
// devices for per-client blocking/statistics.
// ponytail: no trusted-proxy support — honor forwarding headers only from a
// configured proxy allowlist if blocky is ever deployed behind a reverse proxy.
func HTTPClientIP(r *http.Request) net.IP {
	// RemoteAddr is normally "host:port"
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr might not have a port in some cases
		return net.ParseIP(r.RemoteAddr)
	}

	return net.ParseIP(ip)
}
