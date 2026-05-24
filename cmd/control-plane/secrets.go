package main

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/RayElg/replay/internal/replaycrypto"
)

// Symmetric encryption for at-rest secret storage (integration tokens,
// environment variable values). The implementation lives in the shared
// replaycrypto package so the runner can decrypt env vars without taking a
// copy of the format. This file is the per-process keyring cache and the
// REPLAY_ENCRYPT_KEY / REPLAY_ENCRYPT_KEY_PREVIOUS env-var handling.
//
// Key rotation: set REPLAY_ENCRYPT_KEY to the new key and
// REPLAY_ENCRYPT_KEY_PREVIOUS to the retired one (comma- or whitespace-
// separated list; more than one is allowed). New writes use the new key while
// reads of data written under any previous key keep working. See the
// replaycrypto package doc for the full procedure.

var errNoSecretKey = errors.New("REPLAY_ENCRYPT_KEY not set — cannot encrypt/decrypt secrets")

// minKeyLen is the warned-below entropy bar. 32 hex / base64 chars ≈ 24 random bytes,
// enough to survive SHA-256 derivation without being a passphrase-style weak input.
const minKeyLen = 24

var (
	keyringOnce      sync.Once
	cachedKeyring    *replaycrypto.Keyring
	cachedKeyringErr error
)

// loadKeyring resolves the encryption keyring from env once per process.
// Subsequent calls reuse the cached value (or cached error) — operators rotate
// keys by restarting, not by mid-process re-read.
func loadKeyring() (*replaycrypto.Keyring, error) {
	keyringOnce.Do(func() {
		raw := os.Getenv("REPLAY_ENCRYPT_KEY")
		if raw == "" {
			cachedKeyringErr = errNoSecretKey
			return
		}
		if len(raw) < minKeyLen {
			slog.Warn("encryption key is short; use a random 32-byte hex value (openssl rand -hex 32)", "length", len(raw), "minimum", minKeyLen)
		}
		previous := splitKeys(os.Getenv("REPLAY_ENCRYPT_KEY_PREVIOUS"))
		kr, err := replaycrypto.NewKeyring(raw, previous...)
		if err != nil {
			cachedKeyringErr = err
			return
		}
		if len(previous) > 0 {
			slog.Info("encryption key rotation active", "previous_keys", len(previous), "primary_fingerprint", kr.PrimaryID())
		}
		cachedKeyring = kr
	})
	return cachedKeyring, cachedKeyringErr
}

// encryptionConfigured reports whether an at-rest encryption key is loaded. The
// settings UI surfaces this so operators can tell when secret values are being
// stored as plaintext rather than encrypted.
func encryptionConfigured() bool {
	_, err := loadKeyring()
	return err == nil
}

// splitKeys parses a comma- or whitespace-separated list of keys, dropping
// blanks so an empty env var yields no entries.
func splitKeys(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\t' || r == ' '
	})
}

func encryptSecret(plain string) (string, error) {
	kr, err := loadKeyring()
	if err != nil {
		return "", err
	}
	return kr.Encrypt(plain)
}

func decryptSecret(enc string) (string, error) {
	// Empty or unencrypted (plaintext) values can be decoded without a key — the
	// shared package returns them as-is. Keeps reads working on installs
	// that haven't configured REPLAY_ENCRYPT_KEY yet.
	if enc == "" || !replaycrypto.IsEncrypted(enc) {
		return enc, nil
	}
	kr, err := loadKeyring()
	if err != nil {
		return "", err
	}
	return kr.Decrypt(enc)
}
