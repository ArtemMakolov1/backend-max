package maxclient

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// MAX requires Russian Trusted Root CA for platform-api2.max.ru. Keep its
// trust local to this client's transport, leaving other providers unchanged.
// Source: https://gu-st.ru/content/lending/russian_trusted_root_ca_pem.crt
// Certificate SHA-256: d26d2d0231b7c39f92cc738512ba54103519e4405d68b5bd703e9788ca8ecf31
//
//go:embed certs/russian_trusted_root_ca.pem
var maxRootCertificate []byte

// NewHTTPClient keeps system roots and adds MAX's official root plus an
// optional operator-provided PEM bundle. Certificate and hostname validation
// remain enabled. The root pool and transport are private to this client.
func NewHTTPClient(caCertFile string, timeout time.Duration) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport has an unexpected type")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system CA pool: %w", err)
	}
	if !roots.AppendCertsFromPEM(maxRootCertificate) {
		return nil, errors.New("bundled MAX root is not a valid PEM certificate")
	}
	if caCertFile = strings.TrimSpace(caCertFile); caCertFile != "" {
		// #nosec G703 -- this is an operator-owned local environment path, never HTTP input; the contents are subsequently parsed as PEM certificates.
		pemBytes, err := os.ReadFile(caCertFile)
		if err != nil {
			return nil, fmt.Errorf("read MAX_CA_CERT_FILE: %w", err)
		}
		if !roots.AppendCertsFromPEM(pemBytes) {
			return nil, errors.New("MAX_CA_CERT_FILE does not contain a valid PEM certificate")
		}
	}
	clone := transport.Clone()
	clone.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return &http.Client{Transport: clone, Timeout: timeout}, nil
}
