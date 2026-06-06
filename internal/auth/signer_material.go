package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
)

func LoadPEMTokenSigner(privateKeyPath, publicKeyPath string) (RS256TokenSigner, error) {
	privatePEM, err := os.ReadFile(strings.TrimSpace(privateKeyPath))
	if err != nil {
		return RS256TokenSigner{}, fmt.Errorf("read jwt private key: %w", err)
	}
	privateKey, err := parseRSAPrivateKey(privatePEM)
	if err != nil {
		return RS256TokenSigner{}, err
	}
	publicPEM, err := os.ReadFile(strings.TrimSpace(publicKeyPath))
	if err != nil {
		return RS256TokenSigner{}, fmt.Errorf("read jwt public key: %w", err)
	}
	publicKey, err := parseRSAPublicKey(publicPEM)
	if err != nil {
		return RS256TokenSigner{}, err
	}
	if privatePublic, ok := privateKey.Public().(*rsa.PublicKey); !ok || privatePublic.N.Cmp(publicKey.N) != 0 || privatePublic.E != publicKey.E {
		return RS256TokenSigner{}, fmt.Errorf("jwt public key does not match private key")
	}
	return RS256TokenSigner{Signer: privateKey, PublicKey: publicKey}, nil
}

func parseRSAPrivateKey(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("parse jwt private key")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse jwt private key: %w", err)
		}
		return key, nil
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse jwt private key: %w", err)
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("jwt private key is not a signer")
		}
		if _, ok := signer.Public().(*rsa.PublicKey); !ok {
			return nil, fmt.Errorf("jwt private key must be RSA")
		}
		return signer, nil
	default:
		return nil, fmt.Errorf("unsupported jwt private key type %q", block.Type)
	}
}

func parseRSAPublicKey(data []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("parse jwt public key")
	}
	publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse jwt public key: %w", err)
	}
	rsaPublicKey, ok := publicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("jwt public key must be RSA")
	}
	return rsaPublicKey, nil
}
