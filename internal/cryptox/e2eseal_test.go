package cryptox

import (
	"fmt"
	"os"
	"testing"
)

// TestSealForE2E is a throwaway helper used once to seal a dummy value for the
// provisioning end-to-end test. Skipped unless SEAL_VALUE is set.
func TestSealForE2E(t *testing.T) {
	val := os.Getenv("SEAL_VALUE")
	if val == "" {
		t.Skip("set SEAL_VALUE to use")
	}
	box, err := NewBox(os.Getenv("SEAL_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := box.Seal(val)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("SEALED=%s\n", out)
}
