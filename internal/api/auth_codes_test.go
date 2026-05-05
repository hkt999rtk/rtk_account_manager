package api

import (
	"strconv"
	"testing"
)

func TestGenerateOTPProduces6DigitStrings(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		code, err := generateOTP()
		if err != nil {
			t.Fatalf("generateOTP: %v", err)
		}
		if len(code) != 6 {
			t.Fatalf("expected 6-char code, got %q", code)
		}
		n, err := strconv.Atoi(code)
		if err != nil || n < 0 || n > 999999 {
			t.Fatalf("expected numeric code in [0,999999], got %q", code)
		}
		seen[code] = true
	}
	if len(seen) < 10 {
		t.Fatalf("expected code variety, got only %d distinct codes in 50 samples", len(seen))
	}
}
