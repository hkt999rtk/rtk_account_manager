package emaildelivery

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type Payload struct {
	RecipientEmail   string  `json:"recipient_email"`
	RecipientName    *string `json:"recipient_name,omitempty"`
	Token            string  `json:"token,omitempty"`
	ExpiresAt        string  `json:"expires_at,omitempty"`
	OrganizationID   string  `json:"organization_id,omitempty"`
	OrganizationName string  `json:"organization_name,omitempty"`
	RequestedQuota   int     `json:"requested_quota,omitempty"`
	ApprovedQuota    *int    `json:"approved_quota,omitempty"`
	DecisionReason   *string `json:"decision_reason,omitempty"`
}

type Cipher struct {
	aead cipher.AEAD
}

func NewCipher(encodedKey string) (*Cipher, error) {
	encodedKey = strings.TrimSpace(encodedKey)
	if encodedKey == "" {
		return nil, errors.New("EMAIL_OUTBOX_ENCRYPTION_KEY is required")
	}
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(encodedKey)
	}
	if err != nil {
		key, err = base64.RawURLEncoding.DecodeString(encodedKey)
	}
	if err != nil {
		return nil, fmt.Errorf("decode EMAIL_OUTBOX_ENCRYPTION_KEY: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("EMAIL_OUTBOX_ENCRYPTION_KEY must decode to exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func (c *Cipher) Encrypt(payload Payload) (nonce, ciphertext []byte, err error) {
	if c == nil || c.aead == nil {
		return nil, nil, errors.New("email outbox cipher unavailable")
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return nonce, c.aead.Seal(nil, nonce, plain, nil), nil
}

func (c *Cipher) Decrypt(nonce, ciphertext []byte) (Payload, error) {
	if c == nil || c.aead == nil {
		return Payload{}, errors.New("email outbox cipher unavailable")
	}
	plain, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return Payload{}, fmt.Errorf("decrypt email payload: %w", err)
	}
	var payload Payload
	if err := json.Unmarshal(plain, &payload); err != nil {
		return Payload{}, fmt.Errorf("decode email payload: %w", err)
	}
	return payload, nil
}
