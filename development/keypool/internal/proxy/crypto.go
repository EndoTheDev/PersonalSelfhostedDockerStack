package proxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

// Encrypt encrypts plaintext with AES-256-GCM using the 32-byte master key.
// Returns (ciphertext, nonce, error). The nonce is fresh per call and must
// be stored alongside the ciphertext for decryption.
func Encrypt(masterKey []byte, plaintext []byte) (ct, nonce []byte, err error) {
	if len(masterKey) != 32 {
		return nil, nil, errors.New("master key must be 32 bytes (AES-256)")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	ct = gcm.Seal(nil, nonce, plaintext, nil)
	return ct, nonce, nil
}

// Decrypt reverses Encrypt.
func Decrypt(masterKey, ct, nonce []byte) ([]byte, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("master key must be 32 bytes (AES-256)")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ct, nil)
}

// DecodeMasterKey parses a base64-encoded 32-byte AES-256 key from the
// ALLOCATED_MASTER_KEY env var. Returns the raw 32 bytes.
func DecodeMasterKey(b64 string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("ALLOCATED_MASTER_KEY is not valid base64")
	}
	if len(b) != 32 {
		return nil, errors.New("ALLOCATED_MASTER_KEY must decode to 32 bytes (AES-256)")
	}
	return b, nil
}

// GenerateMasterKey returns 32 random bytes, base64-encoded — suitable for
// writing to .env as ALLOCATED_MASTER_KEY=...
func GenerateMasterKey() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}