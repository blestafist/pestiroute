// Package crypto provides protected credential envelope primitives.
package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	KeySize   = 32
	NonceSize = 12
)

var (
	ErrKeyFile        = errors.New("invalid master key file")
	ErrEnvelope       = errors.New("invalid credential envelope")
	ErrAuthentication = errors.New("credential envelope authentication failed")
)

// MasterKey keeps key bytes private so ordinary formatting cannot disclose them.
type MasterKey struct{ value [KeySize]byte }

func (MasterKey) String() string                { return "[REDACTED]" }
func (MasterKey) GoString() string              { return "[REDACTED]" }
func (MasterKey) Format(s fmt.State, verb rune) { _, _ = io.WriteString(s, "[REDACTED]") }

// Envelope matches the versioned credential columns in schema v2.
type Envelope struct {
	FormatVersion int
	KeyVersion    string
	Nonce         []byte
	Ciphertext    []byte
}

// Seal encrypts plaintext using AES-256-GCM and record-bound authenticated data.
func Seal(key MasterKey, formatVersion int, keyVersion, purpose, recordID, accountID string, plaintext []byte) (Envelope, error) {
	aad, err := associatedData(formatVersion, purpose, recordID, accountID)
	if err != nil || keyVersion == "" {
		return Envelope{}, ErrEnvelope
	}
	block, err := aes.NewCipher(key.value[:])
	if err != nil {
		return Envelope{}, ErrEnvelope
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, ErrEnvelope
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Envelope{}, ErrEnvelope
	}
	return Envelope{
		FormatVersion: formatVersion,
		KeyVersion:    keyVersion,
		Nonce:         nonce,
		Ciphertext:    gcm.Seal(nil, nonce, plaintext, aad),
	}, nil
}

// Open authenticates envelope metadata and returns plaintext only on success.
func Open(key MasterKey, envelope Envelope, purpose, recordID, accountID string) ([]byte, error) {
	aad, err := associatedData(envelope.FormatVersion, purpose, recordID, accountID)
	if err != nil || envelope.KeyVersion == "" {
		return nil, ErrEnvelope
	}
	block, err := aes.NewCipher(key.value[:])
	if err != nil {
		return nil, ErrEnvelope
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(envelope.Nonce) != gcm.NonceSize() || len(envelope.Ciphertext) < gcm.Overhead() {
		return nil, ErrEnvelope
	}
	plaintext, err := gcm.Open(nil, envelope.Nonce, envelope.Ciphertext, aad)
	if err != nil {
		return nil, ErrAuthentication
	}
	return plaintext, nil
}

func associatedData(formatVersion int, purpose, recordID, accountID string) ([]byte, error) {
	if formatVersion <= 0 || purpose == "" || recordID == "" {
		return nil, ErrEnvelope
	}
	fields := []string{purpose, recordID, accountID}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint64(formatVersion))
	for _, field := range fields {
		if uint64(len(field)) > uint64(^uint32(0)) {
			return nil, ErrEnvelope
		}
		_ = binary.Write(&buf, binary.BigEndian, uint32(len(field)))
		_, _ = buf.WriteString(field)
	}
	return buf.Bytes(), nil
}
