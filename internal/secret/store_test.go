package secret

import (
	"bytes"
	"testing"
)

func TestStoreEncryptDecrypt(t *testing.T) {
	keyHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	store, err := NewStore(keyHex)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	testData := []byte("confidential_gemini_cookie_and_token_123456789")
	payload, nonce, err := store.Encrypt(testData)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	decrypted, err := store.Decrypt(payload, nonce)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if !bytes.Equal(testData, decrypted) {
		t.Fatalf("expected %s, got %s", string(testData), string(decrypted))
	}
}

func TestStoreTamperedPayload(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	payload, nonce, _ := store.Encrypt([]byte("my_secret"))
	payload[0] ^= 0xFF // Tamper byte

	_, err = store.Decrypt(payload, nonce)
	if err == nil {
		t.Fatalf("expected decryption error on tampered payload, got nil")
	}
}
