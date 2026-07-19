package email

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPassword = "s3cr3t-do-not-log"

func validSenderConfig(host string, port int) Config {
	return Config{
		Host:      host,
		Port:      port,
		Username:  "smtp-user",
		Password:  testPassword,
		FromEmail: "no-reply@maxposty.example",
		FromName:  "Команда MaxPosty",
		AppURL:    testAppURL,
		SiteURL:   testSiteURL,
	}
}

func TestNewSMTPSenderValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewSMTPSender(validSenderConfig("smtp.example", 587)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := map[string]Config{
		"missing host":   {Port: 587, FromEmail: "a@b.example", AppURL: testAppURL, SiteURL: testSiteURL},
		"bad port":       {Host: "h", Port: 0, FromEmail: "a@b.example", AppURL: testAppURL, SiteURL: testSiteURL},
		"missing from":   {Host: "h", Port: 587, AppURL: testAppURL, SiteURL: testSiteURL},
		"invalid from":   {Host: "h", Port: 587, FromEmail: "not-an-email", AppURL: testAppURL, SiteURL: testSiteURL},
		"missing appurl": {Host: "h", Port: 587, FromEmail: "a@b.example", SiteURL: testSiteURL},
	}
	for name, cfg := range cases {
		if _, err := NewSMTPSender(cfg); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestSMTPSenderStartTLSDeliversWelcome(t *testing.T) {
	t.Parallel()
	server := startFakeSMTP(t, false, false)
	defer server.close()

	host, portStr, err := net.SplitHostPort(server.addr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	sender, err := NewSMTPSender(validSenderConfig(host, port))
	if err != nil {
		t.Fatal(err)
	}
	sender.tlsConfig = insecureTestTLS()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sender.SendWelcome(ctx, WelcomeRecipient{Email: "new@user.example", DisplayName: "Иван"}); err != nil {
		t.Fatalf("SendWelcome: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if !server.usedTLS {
		t.Error("expected the session to upgrade via STARTTLS")
	}
	if !server.authSeen {
		t.Error("expected the client to authenticate")
	}
	if !strings.Contains(server.mailFrom, "<no-reply@maxposty.example>") {
		t.Errorf("MAIL FROM = %q", server.mailFrom)
	}
	if len(server.rcptTo) != 1 || !strings.Contains(server.rcptTo[0], "<new@user.example>") {
		t.Errorf("RCPT TO = %v", server.rcptTo)
	}
	assertMessageShape(t, server.message)
	if strings.Contains(server.message, testPassword) {
		t.Error("password leaked into the transmitted message")
	}
}

func TestSMTPSenderImplicitTLSDeliversWelcome(t *testing.T) {
	t.Parallel()
	server := startFakeSMTP(t, true, false)
	defer server.close()

	sender, err := NewSMTPSender(validSenderConfig("127.0.0.1", 465))
	if err != nil {
		t.Fatal(err)
	}
	sender.tlsConfig = insecureTestTLS()
	sender.dialAddr = server.addr()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sender.SendWelcome(ctx, WelcomeRecipient{Email: "new@user.example"}); err != nil {
		t.Fatalf("SendWelcome: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if !server.usedTLS {
		t.Error("expected the session to run over implicit TLS")
	}
	if !server.authSeen {
		t.Error("expected the client to authenticate")
	}
	assertMessageShape(t, server.message)
}

func TestSMTPSenderAuthFailureDoesNotLeakPassword(t *testing.T) {
	t.Parallel()
	server := startFakeSMTP(t, false, true) // rejects AUTH
	defer server.close()

	host, portStr, _ := net.SplitHostPort(server.addr())
	port, _ := strconv.Atoi(portStr)

	sender, err := NewSMTPSender(validSenderConfig(host, port))
	if err != nil {
		t.Fatal(err)
	}
	sender.tlsConfig = insecureTestTLS()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = sender.SendWelcome(ctx, WelcomeRecipient{Email: "new@user.example"})
	if err == nil {
		t.Fatal("expected an authentication error")
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Errorf("password leaked in error: %v", err)
	}
}

func TestSMTPSenderDialFailureDoesNotLeakPassword(t *testing.T) {
	t.Parallel()
	// Bind then close a port to guarantee a connection refusal.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	sender, err := NewSMTPSender(validSenderConfig(host, port))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = sender.SendWelcome(ctx, WelcomeRecipient{Email: "new@user.example"})
	if err == nil {
		t.Fatal("expected a dial error")
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Errorf("password leaked in error: %v", err)
	}
}

func assertMessageShape(t *testing.T, message string) {
	t.Helper()
	if !strings.Contains(message, "multipart/alternative") {
		t.Error("message is not multipart/alternative")
	}
	if !strings.Contains(message, "text/plain") || !strings.Contains(message, "text/html") {
		t.Error("message is missing the plain-text or HTML alternative part")
	}
	if !strings.Contains(strings.ToLower(message), "=?utf-8?q?") {
		t.Error("subject is not Q-encoded UTF-8")
	}
	if !strings.Contains(message, "MIME-Version: 1.0") {
		t.Error("message is missing the MIME-Version header")
	}
}

func insecureTestTLS() *tls.Config {
	// #nosec G402 -- the in-process test SMTP server presents a self-signed certificate; verification is intentionally skipped in tests only.
	return &tls.Config{
		InsecureSkipVerify: true,
	}
}

// fakeSMTP is a single-connection SMTP server used to drive the sender. When
// implicit is true it wraps the connection in TLS on accept (port 465 style);
// otherwise it advertises STARTTLS and upgrades on request (port 587 style).
type fakeSMTP struct {
	ln         net.Listener
	serverTLS  *tls.Config
	implicit   bool
	rejectAuth bool
	done       chan struct{}

	mu       sync.Mutex
	usedTLS  bool
	authSeen bool
	mailFrom string
	rcptTo   []string
	message  string
}

func startFakeSMTP(t *testing.T, implicit, rejectAuth bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{
		ln:         ln,
		serverTLS:  &tls.Config{Certificates: []tls.Certificate{newSelfSignedCert(t)}, MinVersion: tls.VersionTLS12},
		implicit:   implicit,
		rejectAuth: rejectAuth,
		done:       make(chan struct{}),
	}
	go f.serve()
	return f
}

func (f *fakeSMTP) addr() string { return f.ln.Addr().String() }

func (f *fakeSMTP) close() {
	_ = f.ln.Close()
	<-f.done
}

func (f *fakeSMTP) serve() {
	defer close(f.done)
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	f.handle(conn)
}

func (f *fakeSMTP) setUsedTLS() {
	f.mu.Lock()
	f.usedTLS = true
	f.mu.Unlock()
}

func (f *fakeSMTP) handle(conn net.Conn) {
	if f.implicit {
		secured := tls.Server(conn, f.serverTLS)
		if err := secured.Handshake(); err != nil {
			return
		}
		conn = secured
		f.setUsedTLS()
	}

	tp := textproto.NewConn(conn)
	_ = tp.PrintfLine("220 maxposty-test ESMTP ready")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			f.mu.Lock()
			secured := f.usedTLS
			f.mu.Unlock()
			if !f.implicit && !secured {
				_ = tp.PrintfLine("250-maxposty-test")
				_ = tp.PrintfLine("250 STARTTLS")
			} else {
				_ = tp.PrintfLine("250-maxposty-test")
				_ = tp.PrintfLine("250 AUTH PLAIN")
			}
		case strings.HasPrefix(cmd, "STARTTLS"):
			_ = tp.PrintfLine("220 Ready to start TLS")
			secured := tls.Server(conn, f.serverTLS)
			if err := secured.Handshake(); err != nil {
				return
			}
			conn = secured
			tp = textproto.NewConn(conn)
			f.setUsedTLS()
		case strings.HasPrefix(cmd, "AUTH"):
			f.mu.Lock()
			f.authSeen = true
			f.mu.Unlock()
			if f.rejectAuth {
				_ = tp.PrintfLine("535 5.7.8 Authentication credentials invalid")
			} else {
				_ = tp.PrintfLine("235 2.7.0 Authentication successful")
			}
		case strings.HasPrefix(cmd, "MAIL FROM"):
			f.mu.Lock()
			f.mailFrom = line
			f.mu.Unlock()
			_ = tp.PrintfLine("250 2.1.0 Ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			f.mu.Lock()
			f.rcptTo = append(f.rcptTo, line)
			f.mu.Unlock()
			_ = tp.PrintfLine("250 2.1.5 Ok")
		case strings.HasPrefix(cmd, "DATA"):
			_ = tp.PrintfLine("354 End data with <CR><LF>.<CR><LF>")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.message = string(data)
			f.mu.Unlock()
			_ = tp.PrintfLine("250 2.0.0 Ok: queued")
		case strings.HasPrefix(cmd, "QUIT"):
			_ = tp.PrintfLine("221 2.0.0 Bye")
			return
		default:
			_ = tp.PrintfLine("250 2.0.0 Ok")
		}
	}
}

func newSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "maxposty-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
