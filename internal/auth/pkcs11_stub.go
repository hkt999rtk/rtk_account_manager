//go:build !pkcs11

package auth

import "fmt"

type PKCS11TokenSignerConfig struct {
	ModulePath string
	TokenLabel string
	SlotID     string
	PIN        string
	KeyLabel   string
}

func LoadPKCS11TokenSigner(cfg PKCS11TokenSignerConfig) (RS256TokenSigner, error) {
	return RS256TokenSigner{}, fmt.Errorf("pkcs11 jwt signer support is not enabled; rebuild with -tags pkcs11")
}
