// Package secret provides authenticated encryption for stored GitLab tokens
// and webhook secrets.
//
// Seal output is sealedPrefix || nonce || ciphertext || GCM tag, with
// additional data that binds the value to its project and field, so a value
// copied to another row or column fails to open. Encrypt output is the legacy
// format nonce || ciphertext || tag without additional data; it is kept only
// to migrate stored values with Rewrap.
package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const keySize = 32

// sealedPrefix marks the versioned format. A legacy value starts with a
// random nonce and matches it only by chance; Rewrap handles that case.
const sealedPrefix = "\x00tb1"

// TokenAAD binds a value to a project's GitLab access token.
func TokenAAD(projectID int64) []byte {
	return []byte(fmt.Sprintf("tribunal/project/%d/token", projectID))
}

// WebhookSecretAAD binds a value to a project's webhook secret.
func WebhookSecretAAD(projectID int64) []byte {
	return []byte(fmt.Sprintf("tribunal/project/%d/webhook_secret", projectID))
}

// IsSealed reports whether value carries the versioned prefix.
func IsSealed(value []byte) bool {
	return bytes.HasPrefix(value, []byte(sealedPrefix))
}

// Cipher encrypts and decrypts values with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// New returns a Cipher for a 32-byte AES-256 key.
func New(key []byte) (*Cipher, error) {
	if len(key) != keySize {
		return nil, errors.New("secret: key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: create GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt returns nonce || ciphertext || tag. The nonce is random.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("secret: nil cipher")
	}
	nonceSize := c.aead.NonceSize()
	out := make([]byte, nonceSize, nonceSize+len(plaintext)+c.aead.Overhead())
	if _, err := io.ReadFull(rand.Reader, out); err != nil {
		return nil, fmt.Errorf("secret: read nonce: %w", err)
	}
	return c.aead.Seal(out, out[:nonceSize], plaintext, nil), nil
}

// Decrypt authenticates and decrypts a value produced by Encrypt.
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("secret: nil cipher")
	}
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) < nonceSize+c.aead.Overhead() {
		return nil, errors.New("secret: ciphertext too short")
	}
	plaintext, err := c.aead.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
	if err != nil {
		return nil, errors.New("secret: decrypt failed")
	}
	return plaintext, nil
}

// Seal encrypts plaintext bound to aad in the versioned format.
func (c *Cipher) Seal(plaintext, aad []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("secret: nil cipher")
	}
	nonceSize := c.aead.NonceSize()
	out := make([]byte, len(sealedPrefix)+nonceSize, len(sealedPrefix)+nonceSize+len(plaintext)+c.aead.Overhead())
	copy(out, sealedPrefix)
	nonce := out[len(sealedPrefix):]
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secret: read nonce: %w", err)
	}
	return c.aead.Seal(out, nonce, plaintext, aad), nil
}

// Open authenticates and decrypts a Seal value bound to aad. Legacy values
// are rejected; migrate them with Rewrap.
func (c *Cipher) Open(value, aad []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("secret: nil cipher")
	}
	if !IsSealed(value) {
		return nil, errors.New("secret: value is not sealed")
	}
	body := value[len(sealedPrefix):]
	nonceSize := c.aead.NonceSize()
	if len(body) < nonceSize+c.aead.Overhead() {
		return nil, errors.New("secret: ciphertext too short")
	}
	plaintext, err := c.aead.Open(nil, body[:nonceSize], body[nonceSize:], aad)
	if err != nil {
		return nil, errors.New("secret: decrypt failed")
	}
	return plaintext, nil
}

// Rewrap returns value in the sealed format bound to aad. A value that is
// already sealed for aad is returned unchanged with changed=false. An empty
// value stays empty. A value that opens neither way is an error.
func (c *Cipher) Rewrap(value, aad []byte) (out []byte, changed bool, err error) {
	if len(value) == 0 {
		return value, false, nil
	}
	if IsSealed(value) {
		if _, err := c.Open(value, aad); err == nil {
			return value, false, nil
		}
	}
	plaintext, err := c.Decrypt(value)
	if err != nil {
		return nil, false, err
	}
	out, err = c.Seal(plaintext, aad)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}
