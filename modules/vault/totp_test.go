package vault

import (
	"testing"
	"time"
)

// RFC 6238 Appendix B test vector (SHA-1, 8 digits, 30s period).
// Secret is ASCII "12345678901234567890" encoded as base32 without padding issues.
func TestRFC6238SHA1(t *testing.T) {
	// Seed: 12345678901234567890
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // base32 of that seed
	// At T=59 → counter 1 → 94287082 (8 digits)
	// Our default is 6 digits; test hotp path via currentOTP with custom period/digits via otpauth
	uri := "otpauth://totp/test?secret=" + secret + "&digits=8&period=30&algorithm=SHA1"
	code, rem, period, err := currentOTP(uri, time.Unix(59, 0))
	if err != nil {
		t.Fatal(err)
	}
	if period != 30 {
		t.Fatalf("period=%d", period)
	}
	if rem < 1 || rem > 30 {
		t.Fatalf("remaining=%d", rem)
	}
	if code != "94287082" {
		t.Fatalf("code=%s want 94287082", code)
	}
}

func TestCurrentOTPSixDigits(t *testing.T) {
	// "Hello!" base32
	secret := "JBSWY3DPEHPK3PXP"
	code, rem, period, err := currentOTP(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 6 {
		t.Fatalf("code len=%d code=%s", len(code), code)
	}
	if period != 30 || rem < 1 || rem > 30 {
		t.Fatalf("period=%d rem=%d", period, rem)
	}
	// Same window → same code
	code2, _, _, err := currentOTP(secret, time.Now())
	if err != nil || code2 != code {
		t.Fatalf("stability: %s vs %s err=%v", code, code2, err)
	}
}

func TestNormalizeOtpauth(t *testing.T) {
	p, err := normalizeTOTPSecret("otpauth://totp/Example:user@ex.com?secret=JBSWY3DPEHPK3PXP&issuer=Example&digits=6&period=30")
	if err != nil {
		t.Fatal(err)
	}
	if p.Secret != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("secret=%s", p.Secret)
	}
}
