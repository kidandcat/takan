package vault

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"hash"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// totpParams holds RFC 6238 options (Google Authenticator defaults).
type totpParams struct {
	Secret  string // raw base32 secret (no spaces)
	Digits  int
	Period  int
	Algo    string // sha1 | sha256 | sha512
}

// NormalizeTOTPForStore cleans a user-provided secret (base32 or otpauth URI) for storage.
// Standard GA params store bare secret; non-defaults keep an otpauth URI.
func NormalizeTOTPForStore(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	p, err := normalizeTOTPSecret(raw)
	if err != nil {
		// Store as-is (spaces stripped) so we don't drop user data on parse edge cases
		return cleanBase32(raw)
	}
	if p.Digits != 6 || p.Period != 30 || p.Algo != "sha1" {
		return fmt.Sprintf("otpauth://totp/item?secret=%s&digits=%d&period=%d&algorithm=%s",
			p.Secret, p.Digits, p.Period, strings.ToUpper(p.Algo))
	}
	return p.Secret
}

// CurrentOTPCode returns the current authenticator code for a stored secret.
func CurrentOTPCode(rawSecret string, now time.Time) (code string, remaining int, period int, err error) {
	return currentOTP(rawSecret, now)
}

// normalizeTOTPSecret accepts base32 secrets or otpauth://totp/... URIs.
func normalizeTOTPSecret(raw string) (totpParams, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return totpParams{}, fmt.Errorf("empty totp secret")
	}
	p := totpParams{Digits: 6, Period: 30, Algo: "sha1"}
	if strings.HasPrefix(strings.ToLower(raw), "otpauth://") {
		u, err := url.Parse(raw)
		if err != nil {
			return p, fmt.Errorf("invalid otpauth uri: %w", err)
		}
		if !strings.EqualFold(u.Host, "totp") && !strings.HasPrefix(strings.ToLower(u.Path), "//totp") {
			// otpauth://totp/Label?secret=…
			if u.Host != "totp" && u.Scheme == "otpauth" {
				// Host is "totp" for standard URIs
			}
		}
		q := u.Query()
		sec := q.Get("secret")
		if sec == "" {
			return p, fmt.Errorf("otpauth uri missing secret")
		}
		p.Secret = cleanBase32(sec)
		if d := q.Get("digits"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && (n == 6 || n == 8) {
				p.Digits = n
			}
		}
		if per := q.Get("period"); per != "" {
			if n, err := strconv.Atoi(per); err == nil && n > 0 && n <= 120 {
				p.Period = n
			}
		}
		if alg := strings.ToLower(q.Get("algorithm")); alg != "" {
			switch alg {
			case "sha1", "sha256", "sha512":
				p.Algo = alg
			}
		}
		return p, nil
	}
	p.Secret = cleanBase32(raw)
	if p.Secret == "" {
		return p, fmt.Errorf("empty totp secret after normalize")
	}
	return p, nil
}

func cleanBase32(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	// Some exports pad with = ; keep padding for decode
	return s
}

// currentOTP returns the current TOTP code and seconds remaining in this period.
func currentOTP(rawSecret string, now time.Time) (code string, remaining int, period int, err error) {
	p, err := normalizeTOTPSecret(rawSecret)
	if err != nil {
		return "", 0, 0, err
	}
	key, err := decodeBase32Secret(p.Secret)
	if err != nil {
		return "", 0, 0, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	counter := uint64(now.Unix()) / uint64(p.Period)
	code, err = hotp(key, counter, p.Digits, p.Algo)
	if err != nil {
		return "", 0, p.Period, err
	}
	elapsed := int(now.Unix() % int64(p.Period))
	remaining = p.Period - elapsed
	if remaining <= 0 {
		remaining = p.Period
	}
	return code, remaining, p.Period, nil
}

func decodeBase32Secret(s string) ([]byte, error) {
	// Try with and without padding
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	if b, err := enc.DecodeString(s); err == nil {
		return b, nil
	}
	encP := base32.StdEncoding
	// Add padding if needed
	if m := len(s) % 8; m != 0 {
		s = s + strings.Repeat("=", 8-m)
	}
	b, err := encP.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid base32 totp secret: %w", err)
	}
	return b, nil
}

func hotp(key []byte, counter uint64, digits int, algo string) (string, error) {
	var h func() hash.Hash
	switch strings.ToLower(algo) {
	case "sha256":
		h = sha256.New
	case "sha512":
		h = sha512.New
	default:
		h = sha1.New
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, counter)
	mac := hmac.New(h, key)
	_, _ = mac.Write(buf)
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	if int(offset)+4 > len(sum) {
		return "", fmt.Errorf("invalid hotp offset")
	}
	truncated := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	n := truncated % mod
	return fmt.Sprintf("%0*d", digits, n), nil
}
