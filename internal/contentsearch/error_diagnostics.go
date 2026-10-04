package contentsearch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"syscall"
)

// Diagnostics is a value snapshot of source-owned enums and an HTTP status.
// It never retains the request, remote body, credentials or transport cause.
type Diagnostics struct {
	Provider      string
	Code          string
	Status        int
	TransportKind string
}

func (e *Error) SafeDiagnostics() Diagnostics {
	if e == nil {
		return Diagnostics{}.Safe()
	}
	return (Diagnostics{Provider: e.Provider, Code: e.Code, Status: e.Status,
		TransportKind: e.TransportKind}).Safe()
}

// Safe validates again at the logging boundary, including manually constructed
// errors supplied by another internal client. Unknown input is never copied.
func (d Diagnostics) Safe() Diagnostics {
	safe := Diagnostics{Provider: "unknown", Code: "unknown"}
	switch d.Provider {
	case "exa", "tavily":
		safe.Provider = d.Provider
	}
	switch d.Code {
	case "invalid_endpoint", "invalid_request", "request_failed", "provider_rejected",
		"invalid_response", "response_too_large", "response_unreadable":
		safe.Code = d.Code
	}
	if d.Status >= 100 && d.Status <= 599 {
		safe.Status = d.Status
	}
	if safe.Code == "request_failed" || safe.Code == "response_unreadable" {
		safe.TransportKind = "unknown"
		switch d.TransportKind {
		case "dns", "tls", "timeout", "no_route", "connection_refused", "connection_reset",
			"request_canceled", "unknown":
			safe.TransportKind = d.TransportKind
		}
	}
	return safe
}

func safeTransportKind(err error) string {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var certificate *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	var recordHeader tls.RecordHeaderError
	var alert tls.AlertError
	if errors.As(err, &certificate) || errors.As(err, &unknownAuthority) || errors.As(err, &hostname) ||
		errors.As(err, &invalidCertificate) || errors.As(err, &recordHeader) || errors.As(err, &alert) {
		return "tls"
	}
	if errors.Is(err, context.Canceled) {
		return "request_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return "no_route"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "connection_reset"
	}
	// url.Error also satisfies net.Error, but its Timeout method may stop at
	// an intermediate fmt wrapper. Check each typed cause without its string.
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		var networkError net.Error
		if errors.As(cause, &networkError) && networkError.Timeout() {
			return "timeout"
		}
	}
	return "unknown"
}
