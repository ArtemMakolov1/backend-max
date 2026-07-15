package maxclient

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// VerifyContactHMAC verifies the proof attached by MAX to a request_contact
// message. JSON decoding already turns \r\n escapes into CRLF; the explicit
// replacement also handles the literal representation documented by MAX.
// The supplied proof is accepted only when it decodes to one full SHA-256
// digest in a documented/common transport representation.
func (c *Client) VerifyContactHMAC(vcfInfo, provided string) bool {
	providedBytes, ok := decodeContactProof(strings.TrimSpace(provided))
	if !ok {
		return false
	}
	canonicalVCF := strings.ReplaceAll(vcfInfo, `\r\n`, "\r\n")
	mac := hmac.New(sha256.New, []byte(c.token))
	_, _ = mac.Write([]byte(canonicalVCF))
	want := mac.Sum(nil)
	return len(providedBytes) == len(want) && subtle.ConstantTimeCompare(providedBytes, want) == 1
}

func decodeContactProof(value string) ([]byte, bool) {
	if decoded, err := hex.DecodeString(value); err == nil && len(decoded) == sha256.Size {
		return decoded, true
	}
	encodings := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	}
	for _, encoding := range encodings {
		if decoded, err := encoding.DecodeString(value); err == nil && len(decoded) == sha256.Size {
			return decoded, true
		}
	}
	return nil, false
}

// NormalizeVerifiedContactPhone parses the phone from a verified vCard. The
// caller must invoke it only after VerifyContactHMAC succeeds and must not log
// or persist the returned value.
func NormalizeVerifiedContactPhone(vcfInfo string) (string, bool) {
	if vcfInfo == "" || len(vcfInfo) > 16<<10 {
		return "", false
	}
	canonical := strings.ReplaceAll(vcfInfo, `\r\n`, "\r\n")
	canonical = strings.ReplaceAll(canonical, "\r\n", "\n")
	var normalized string
	for _, line := range strings.Split(canonical, "\n") {
		separator := strings.IndexByte(line, ':')
		if separator <= 0 {
			continue
		}
		property := strings.ToUpper(strings.TrimSpace(line[:separator]))
		if parameters := strings.IndexByte(property, ';'); parameters >= 0 {
			property = property[:parameters]
		}
		if property != "TEL" {
			continue
		}
		value := strings.TrimSpace(line[separator+1:])
		var digits strings.Builder
		for index, char := range value {
			switch {
			case char >= '0' && char <= '9':
				digits.WriteRune(char)
			case char == '+' && index == 0, char == ' ', char == '-', char == '(', char == ')':
				// Formatting characters are removed.
			default:
				return "", false
			}
		}
		phone := digits.String()
		if len(phone) < 7 || len(phone) > 15 {
			return "", false
		}
		if len(phone) == 11 && phone[0] == '8' {
			phone = "7" + phone[1:]
		}
		phone = "+" + phone
		if normalized != "" && normalized != phone {
			return "", false
		}
		normalized = phone
	}
	return normalized, normalized != ""
}
