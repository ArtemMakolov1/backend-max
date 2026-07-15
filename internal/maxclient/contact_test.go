package maxclient

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifyContactHMACAcceptsCRLFHexAndBase64AndRejectsTampering(t *testing.T) {
	t.Parallel()
	const token = "secret-bot-token"
	vcf := "BEGIN:VCARD\r\nVERSION:3.0\r\nTEL;TYPE=cell:79990000000\r\nFN:Ivan Ivanov\r\nEND:VCARD\r\n"
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(vcf))
	digest := mac.Sum(nil)
	client := mustClient(t, "https://platform-api2.max.ru", token, http.DefaultClient)
	for name, proof := range map[string]string{
		"lower hex": hex.EncodeToString(digest),
		"upper hex": strings.ToUpper(hex.EncodeToString(digest)),
		"base64":    base64.StdEncoding.EncodeToString(digest),
		"raw url":   base64.RawURLEncoding.EncodeToString(digest),
	} {
		t.Run(name, func(t *testing.T) {
			if !client.VerifyContactHMAC(vcf, proof) {
				t.Fatal("valid MAX contact proof was rejected")
			}
		})
	}
	if !client.VerifyContactHMAC(strings.ReplaceAll(vcf, "\r\n", `\r\n`), hex.EncodeToString(digest)) {
		t.Fatal("documented literal CRLF representation was rejected")
	}
	for _, proof := range []string{"", "not-a-proof", hex.EncodeToString(digest[:31]), hex.EncodeToString(append([]byte{}, digest...)) + "00"} {
		if client.VerifyContactHMAC(vcf, proof) {
			t.Fatalf("invalid proof %q was accepted", proof)
		}
	}
	if client.VerifyContactHMAC(strings.Replace(vcf, "Ivan", "Eve", 1), hex.EncodeToString(digest)) {
		t.Fatal("tampered vCard was accepted")
	}
}

func TestNormalizeVerifiedContactPhoneIsStrictAndTransient(t *testing.T) {
	t.Parallel()
	valid := "BEGIN:VCARD\r\nTEL;TYPE=cell:+7 (999) 000-00-00\r\nEND:VCARD\r\n"
	if got, ok := NormalizeVerifiedContactPhone(valid); !ok || got != "+79990000000" {
		t.Fatalf("normalized phone = %q, %v", got, ok)
	}
	legacy := "BEGIN:VCARD\r\nTEL:8 999 000 00 00\r\nEND:VCARD\r\n"
	if got, ok := NormalizeVerifiedContactPhone(legacy); !ok || got != "+79990000000" {
		t.Fatalf("legacy normalized phone = %q, %v", got, ok)
	}
	for _, invalid := range []string{
		"BEGIN:VCARD\r\nTELX:79990000000\r\nEND:VCARD\r\n",
		"BEGIN:VCARD\r\nTEL:79990000000\r\nTEL:78880000000\r\nEND:VCARD\r\n",
		"BEGIN:VCARD\r\nTEL:call-me\r\nEND:VCARD\r\n",
	} {
		if _, ok := NormalizeVerifiedContactPhone(invalid); ok {
			t.Fatalf("invalid vCard was accepted: %q", invalid)
		}
	}
}

func TestSendAuthContactRequestUsesPrivateRequestContactButton(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/messages" || r.URL.Query().Get("user_id") != "777" {
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "shared-token" {
			t.Error("bot token was not sent in Authorization")
		}
		var body struct {
			Text        string `json:"text"`
			Attachments []struct {
				Type    string `json:"type"`
				Payload struct {
					Buttons [][]map[string]string `json:"buttons"`
				} `json:"payload"`
			} `json:"attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body.Text, "314159") || len(body.Attachments) != 1 ||
			body.Attachments[0].Type != "inline_keyboard" || len(body.Attachments[0].Payload.Buttons) != 2 ||
			body.Attachments[0].Payload.Buttons[0][0]["type"] != "request_contact" ||
			body.Attachments[0].Payload.Buttons[0][0]["payload"] != "" ||
			body.Attachments[0].Payload.Buttons[1][0]["type"] != "callback" ||
			body.Attachments[0].Payload.Buttons[1][0]["payload"] != "auth_contact_confirm_secret" {
			t.Fatalf("request_contact body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "shared-token", server.Client())
	if err := client.SendAuthContactRequest(context.Background(), "777", "314159", "auth_contact_confirm_secret"); err != nil {
		t.Fatal(err)
	}
}
