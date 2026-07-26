package emaildelivery

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestCipherRoundTripAndWrongKey(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytesOf(32, 1))
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	payload := Payload{RecipientEmail: "user@example.com", Token: "secret-token"}
	nonce, ciphertext, err := cipher.Encrypt(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), payload.Token) {
		t.Fatal("ciphertext contains plaintext token")
	}
	got, err := cipher.Decrypt(nonce, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if got != payload {
		t.Fatalf("payload = %+v, want %+v", got, payload)
	}
	other, err := NewCipher(base64.StdEncoding.EncodeToString(bytesOf(32, 2)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Decrypt(nonce, ciphertext); err == nil {
		t.Fatal("wrong key unexpectedly decrypted payload")
	}
}

func TestCipherRejectsInvalidKey(t *testing.T) {
	for _, key := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := NewCipher(key); err == nil {
			t.Fatalf("NewCipher(%q) unexpectedly succeeded", key)
		}
	}
}

func bytesOf(size int, value byte) []byte {
	result := make([]byte, size)
	for i := range result {
		result[i] = value
	}
	return result
}
