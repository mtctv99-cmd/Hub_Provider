package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
)

var (
	ErrInvalidKeyLength = errors.New("master key must be 32 bytes (64 hex characters)")
	ErrCiphertextShort  = errors.New("ciphertext is too short")
	ErrDecryptionFailed = errors.New("failed to decrypt payload (authentication tag mismatch or invalid key)")
)

type Store struct {
	mu   sync.RWMutex
	aead cipher.AEAD
	key  []byte
}

func NewStore(masterKeyHex string) (*Store, error) {
	var key []byte
	masterKeyHex = strings.TrimSpace(masterKeyHex)

	if masterKeyHex == "" {
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("failed to generate random key: %w", err)
		}
		log.Println("[WARNING] MASTER_KEY environment variable not set. Using ephemeral in-memory key (data cannot be decrypted across restarts)")
	} else {
		decoded, err := hex.DecodeString(masterKeyHex)
		if err != nil {
			return nil, fmt.Errorf("invalid hex in MASTER_KEY: %w", err)
		}
		if len(decoded) != 32 {
			return nil, ErrInvalidKeyLength
		}
		key = decoded
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher creation failed: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm creation failed: %w", err)
	}

	return &Store{
		aead: gcm,
		key:  key,
	}, nil
}

func (s *Store) Encrypt(plaintext []byte) (payload, nonce []byte, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nonce = make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	payload = s.aead.Seal(nil, nonce, plaintext, nil)
	return payload, nonce, nil
}

func (s *Store) Decrypt(payload, nonce []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(nonce) != s.aead.NonceSize() {
		return nil, fmt.Errorf("invalid nonce size: expected %d, got %d", s.aead.NonceSize(), len(nonce))
	}

	plaintext, err := s.aead.Open(nil, nonce, payload, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}

	return plaintext, nil
}
