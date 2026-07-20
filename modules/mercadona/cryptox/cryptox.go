// Package cryptox encrypts secrets at rest with AES-GCM.
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

// Box encrypts/decrypts strings using a passphrase-derived 32-byte key.
type Box struct {
	gcm cipher.AEAD
}

// New derives an AES-256-GCM key from secret (any length; hashed with SHA-256).
func New(secret string) (*Box, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, fmt.Errorf("cryptox: empty encryption key")
	}
	sum := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{gcm: gcm}, nil
}

// Seal encrypts plaintext and returns a base64url ciphertext (nonce||seal).
func (b *Box) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := b.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// Open decrypts a Seal() result.
func (b *Box) Open(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("cryptox: decode: %w", err)
	}
	ns := b.gcm.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("cryptox: ciphertext too short")
	}
	plain, err := b.gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("cryptox: decrypt: %w", err)
	}
	return string(plain), nil
}
