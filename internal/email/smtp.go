package email

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Config describes how to reach the SMTP relay and how to fill in the welcome
// email. Host, Port and FromEmail are required; AppURL and SiteURL feed the
// template. Password is never logged or embedded in errors.
type Config struct {
	Host      string
	Port      int
	Username  string
	Password  string
	FromEmail string
	FromName  string
	AppURL    string
	SiteURL   string
}

func (c Config) validate() error {
	if c.Host == "" {
		return errors.New("SMTP host is required")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("SMTP port must be between 1 and 65535, got %d", c.Port)
	}
	if c.FromEmail == "" {
		return errors.New("SMTP from address is required")
	}
	if strings.ContainsAny(c.FromEmail, "\r\n") || !strings.Contains(c.FromEmail, "@") {
		return errors.New("SMTP from address must be a valid email address")
	}
	if c.AppURL == "" || c.SiteURL == "" {
		return errors.New("welcome email requires application and site URLs")
	}
	return nil
}

// SMTPSender delivers the welcome email over SMTP using the standard library. It
// selects the transport security mode from the port: 465 uses implicit TLS,
// while every other port (typically 587 or 25) attempts STARTTLS.
type SMTPSender struct {
	cfg Config

	// tlsConfig and dialAddr are test seams. In production tlsConfig is nil (a
	// verifying config is built from the host) and dialAddr is empty (the host
	// and port are used). Tests inject a self-signed trust config and an
	// ephemeral listener address here.
	tlsConfig *tls.Config
	dialAddr  string
}

// NewSMTPSender validates cfg and returns a ready sender. When validation fails
// the caller should fall back to a NoopSender so sign-in keeps working.
func NewSMTPSender(cfg Config) (*SMTPSender, error) {
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.FromEmail = strings.TrimSpace(cfg.FromEmail)
	cfg.FromName = strings.TrimSpace(cfg.FromName)
	cfg.AppURL = strings.TrimSpace(cfg.AppURL)
	cfg.SiteURL = strings.TrimSpace(cfg.SiteURL)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &SMTPSender{cfg: cfg}, nil
}

// SendWelcome renders and delivers the welcome email to recipient.
func (s *SMTPSender) SendWelcome(ctx context.Context, recipient WelcomeRecipient) error {
	recipient.Email = strings.TrimSpace(recipient.Email)
	if recipient.Email == "" {
		return errors.New("recipient email is required")
	}
	if strings.ContainsAny(recipient.Email, "\r\n") {
		return errors.New("recipient email contains control characters")
	}

	htmlBody, textBody, err := Render(WelcomeData{
		DisplayName: recipient.DisplayName,
		AppURL:      s.cfg.AppURL,
		SiteURL:     s.cfg.SiteURL,
	})
	if err != nil {
		return err
	}

	msg, err := s.buildMessage(recipient.Email, welcomeSubject, textBody, htmlBody)
	if err != nil {
		return err
	}
	return s.send(ctx, recipient.Email, msg)
}

// buildMessage assembles a MIME multipart/alternative message with a plain-text
// and an HTML part. The subject and any non-ASCII display name are Q-encoded and
// the UTF-8 bodies are base64 encoded so Cyrillic content survives transport.
func (s *SMTPSender) buildMessage(to, subject, textBody, htmlBody string) ([]byte, error) {
	if strings.ContainsAny(s.cfg.FromEmail, "\r\n") || strings.ContainsAny(to, "\r\n") {
		return nil, errors.New("email address contains control characters")
	}

	msgID, err := messageID(s.cfg.FromEmail)
	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	textPart, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=UTF-8"},
		"Content-Transfer-Encoding": {"base64"},
	})
	if err != nil {
		return nil, fmt.Errorf("create text part: %w", err)
	}
	if _, err := io.WriteString(textPart, encodeBase64Lines([]byte(textBody))); err != nil {
		return nil, fmt.Errorf("write text part: %w", err)
	}

	htmlPart, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/html; charset=UTF-8"},
		"Content-Transfer-Encoding": {"base64"},
	})
	if err != nil {
		return nil, fmt.Errorf("create HTML part: %w", err)
	}
	if _, err := io.WriteString(htmlPart, encodeBase64Lines([]byte(htmlBody))); err != nil {
		return nil, fmt.Errorf("write HTML part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	var msg bytes.Buffer
	writeHeader(&msg, "From", formatAddress(s.cfg.FromName, s.cfg.FromEmail))
	writeHeader(&msg, "To", "<"+to+">")
	writeHeader(&msg, "Subject", mime.QEncoding.Encode("UTF-8", subject))
	writeHeader(&msg, "Date", time.Now().Format(time.RFC1123Z))
	writeHeader(&msg, "Message-ID", msgID)
	writeHeader(&msg, "MIME-Version", "1.0")
	writeHeader(&msg, "Content-Type", "multipart/alternative; boundary=\""+writer.Boundary()+"\"")
	msg.WriteString("\r\n")
	msg.Write(body.Bytes())
	return msg.Bytes(), nil
}

// send opens the SMTP connection, negotiates TLS, authenticates when credentials
// are set and transmits the message. The context deadline bounds the dial.
func (s *SMTPSender) send(ctx context.Context, to string, msg []byte) error {
	dialer := &net.Dialer{}
	if deadline, ok := ctx.Deadline(); ok {
		dialer.Deadline = deadline
	}

	client, err := s.dial(ctx, dialer)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP authentication failed: %w", err)
		}
	}
	if err := client.Mail(s.cfg.FromEmail); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("SMTP RCPT TO: %w", err)
	}
	dataWriter, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := dataWriter.Write(msg); err != nil {
		_ = dataWriter.Close()
		return fmt.Errorf("write SMTP message: %w", err)
	}
	if err := dataWriter.Close(); err != nil {
		return fmt.Errorf("finalize SMTP message: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("SMTP QUIT: %w", err)
	}
	return nil
}

// dial connects and returns a client with transport security established. Port
// 465 uses implicit TLS (tls.Dial + smtp.NewClient); other ports dial in the
// clear and upgrade with STARTTLS when the server advertises it. Credentials are
// never sent over an unencrypted connection.
func (s *SMTPSender) dial(ctx context.Context, dialer *net.Dialer) (*smtp.Client, error) {
	addr := s.address()

	if s.cfg.Port == 465 {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, s.clientTLSConfig())
		if err != nil {
			return nil, fmt.Errorf("dial SMTP over TLS: %w", err)
		}
		client, err := smtp.NewClient(conn, s.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("create SMTP client: %w", err)
		}
		return client, nil
	}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial SMTP: %w", err)
	}
	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("create SMTP client: %w", err)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(s.clientTLSConfig()); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("start TLS: %w", err)
		}
	} else if s.cfg.Username != "" {
		_ = client.Close()
		return nil, errors.New("SMTP server does not support STARTTLS; refusing to send credentials over plaintext")
	}
	return client, nil
}

func (s *SMTPSender) address() string {
	if s.dialAddr != "" {
		return s.dialAddr
	}
	return net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
}

func (s *SMTPSender) clientTLSConfig() *tls.Config {
	var cfg *tls.Config
	if s.tlsConfig != nil {
		cfg = s.tlsConfig.Clone()
	} else {
		cfg = &tls.Config{}
	}
	if cfg.ServerName == "" {
		cfg.ServerName = s.cfg.Host
	}
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	return cfg
}

func writeHeader(buf *bytes.Buffer, name, value string) {
	buf.WriteString(name)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteString("\r\n")
}

// formatAddress renders a From header value, Q-encoding the display name so
// non-ASCII names remain valid in the header.
func formatAddress(name, email string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "<" + email + ">"
	}
	return mime.QEncoding.Encode("UTF-8", name) + " <" + email + ">"
}

// encodeBase64Lines base64-encodes data and wraps it at 76 characters per line
// with CRLF, as required for MIME base64 transfer encoding.
func encodeBase64Lines(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	const lineLen = 76
	var b strings.Builder
	for len(encoded) > lineLen {
		b.WriteString(encoded[:lineLen])
		b.WriteString("\r\n")
		encoded = encoded[lineLen:]
	}
	b.WriteString(encoded)
	return b.String()
}

// messageID builds a globally unique Message-ID using random bytes and the
// From address domain.
func messageID(fromEmail string) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate message id: %w", err)
	}
	domain := "localhost"
	if at := strings.LastIndex(fromEmail, "@"); at >= 0 && at+1 < len(fromEmail) {
		domain = fromEmail[at+1:]
	}
	return "<" + hex.EncodeToString(buf[:]) + "@" + domain + ">", nil
}
