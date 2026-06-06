//go:build !pkcs11

package auth

import (
	"strings"
	"testing"
)

func TestLoadPKCS11TokenSignerRequiresBuildTag(t *testing.T) {
	_, err := LoadPKCS11TokenSigner(PKCS11TokenSignerConfig{
		ModulePath: "/usr/lib/softhsm/libsofthsm2.so",
		TokenLabel: "jwt-access",
		PIN:        "1234",
		KeyLabel:   "jwt-access-key",
	})
	if err == nil {
		t.Fatal("expected pkcs11 build tag error")
	}
	if !strings.Contains(err.Error(), "pkcs11 jwt signer support is not enabled") {
		t.Fatalf("unexpected error: %v", err)
	}
}
