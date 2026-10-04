package maxclient

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBundledMAXRootIsPinnedOfficialCertificate(t *testing.T) {
	block, rest := pem.Decode(maxRootCertificate)
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatal("expected exactly one PEM certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(certificate.Raw)
	if got := hex.EncodeToString(fingerprint[:]); got != "d26d2d0231b7c39f92cc738512ba54103519e4405d68b5bd703e9788ca8ecf31" {
		t.Fatalf("official certificate fingerprint = %s", got)
	}
	if !certificate.IsCA || certificate.Subject.CommonName != "Russian Trusted Root CA" {
		t.Fatalf("unexpected CA: %v", certificate.Subject)
	}
	if err := certificate.CheckSignatureFrom(certificate); err != nil {
		t.Fatalf("root self signature: %v", err)
	}
	client, err := NewHTTPClient("", 75*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	if client.Timeout != 75*time.Second || transport == http.DefaultTransport ||
		transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("MAX transport must be private and verify TLS")
	}
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: transport.TLSClientConfig.RootCAs}); err != nil {
		t.Fatalf("bundled official CA is not trusted: %v", err)
	}
	defaultTransport := http.DefaultTransport.(*http.Transport)
	if defaultTransport.TLSClientConfig != nil && defaultTransport.TLSClientConfig.RootCAs == transport.TLSClientConfig.RootCAs {
		t.Fatal("MAX must not replace the global trust pool")
	}
}

func TestMAXHTTPClientVerifiesCertificatesAndOptionalOperatorBundle(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := NewHTTPClient("", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := client.Get(server.URL); err == nil {
		_ = response.Body.Close()
		t.Fatal("untrusted self signed server must be rejected")
	} else {
		var unknownAuthority x509.UnknownAuthorityError
		if !errors.As(err, &unknownAuthority) {
			t.Fatalf("expected certificate validation error: %v", err)
		}
	}
	certificatePath := filepath.Join(t.TempDir(), "operator-root.pem")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	trusted, err := NewHTTPClient(certificatePath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := trusted.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("trusted server status = %d", response.StatusCode)
	}
	wrongHost, err := NewHTTPClient(certificatePath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	wrongHost.Transport.(*http.Transport).TLSClientConfig.ServerName = "wrong.example.invalid"
	if response, err := wrongHost.Get(server.URL); err == nil {
		_ = response.Body.Close()
		t.Fatal("trusted certificate with mismatching hostname must be rejected")
	}
	if _, err := NewHTTPClient(certificatePath+".missing", time.Second); err == nil {
		t.Fatal("missing operator certificate must fail closed")
	}
	if err := os.WriteFile(certificatePath, []byte("invalid PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHTTPClient(certificatePath, time.Second); err == nil {
		t.Fatal("invalid operator certificate must fail closed")
	}
}
