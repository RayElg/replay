// Package replaycrypto provides symmetric encryption for at-rest secret storage
// (integration tokens, environment variable values). Shared between
// cmd/control-plane (encrypt + decrypt) and cmd/runner (decrypt only) so both
// agree on the wire format without copy-paste drift.
//
// On-the-wire format (the prefixed string is stored and retrieved verbatim):
//
//	[[enc:v2:<keyid>]]base64(nonce[12] || ciphertext_with_gcm_tag)
//
// <keyid> is an 8-hex fingerprint of the derived key, so every value records
// which key encrypted it. That fingerprint is what makes key rotation safe: a
// Keyring holds one primary key (used for all new writes) plus any number of
// previous keys (used only for decryption), and Decrypt selects the matching
// key by fingerprint.
//
// Unencrypted values: at-rest encryption is opt-in, so a value with no
// [[enc:…]] prefix is stored as plaintext, and Decrypt returns it unchanged.
//
// Key rotation procedure:
//  1. Generate a new key:  openssl rand -hex 32
//  2. On the control plane and every runner set
//     REPLAY_ENCRYPT_KEY=<new> and REPLAY_ENCRYPT_KEY_PREVIOUS=<old>,
//     then restart. New writes use <new>; reads of data written under <old>
//     keep working through the previous-key slot.
//  3. Re-save each environment / integration so its values are rewritten under
//     the new key, then drop REPLAY_ENCRYPT_KEY_PREVIOUS once nothing references
//     the retired key anymore.
package replaycrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// encPrefix marks a value as ciphertext. Unencrypted (plaintext) values lack
	// it and are returned by Decrypt as-is.
	encPrefix  = "[[enc:"
	envelopeV2 = "v2" // [[enc:v2:<keyid>]] — key selected by fingerprint
)

var (
	// ErrNoKey is returned when callers ask to encrypt/decrypt but no key is loaded.
	ErrNoKey = errors.New("REPLAY_ENCRYPT_KEY not set — cannot encrypt/decrypt secrets")
	// ErrUnknownKey means a v2 envelope names a fingerprint no loaded key matches —
	// typically the value was written by a key that has since been retired.
	ErrUnknownKey = errors.New("no loaded key matches the envelope fingerprint; set REPLAY_ENCRYPT_KEY_PREVIOUS to the retired key during rotation")
)

// Key is a derived AES-256 key plus a short fingerprint tying ciphertext to the
// key that produced it. Derive once at startup.
type Key struct {
	bytes [32]byte
	id    string
}

// ID returns the key's 8-hex fingerprint — the value embedded in v2 envelopes.
func (k *Key) ID() string { return k.id }

// Derive turns raw operator-supplied key material into an AES-256 key. Empty
// input → ErrNoKey so callers can distinguish "not configured" from "bad key".
func Derive(raw string) (*Key, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrNoKey
	}
	sum := sha256.Sum256([]byte(raw))
	// Fingerprint the derived key (not the raw input) so the id leaks nothing
	// about the operator's key material.
	fp := sha256.Sum256(sum[:])
	return &Key{bytes: sum, id: hex.EncodeToString(fp[:4])}, nil
}

// Keyring holds the key used for new writes (primary) plus any retired keys kept
// for decryption during rotation. Build one per process with NewKeyring.
type Keyring struct {
	primary *Key
	byID    map[string]*Key
}

// NewKeyring derives the primary key plus any previous (decryption-only) keys.
// Empty primary → ErrNoKey. Blank previous entries are skipped, so callers can
// pass an unsplit/empty env var without filtering it first. Duplicate keys
// (e.g. the primary repeated in the previous list) are de-duplicated.
func NewKeyring(primary string, previous ...string) (*Keyring, error) {
	pk, err := Derive(primary)
	if err != nil {
		return nil, err
	}
	kr := &Keyring{primary: pk, byID: map[string]*Key{pk.id: pk}}
	for _, p := range previous {
		if strings.TrimSpace(p) == "" {
			continue
		}
		k, err := Derive(p)
		if err != nil {
			return nil, err
		}
		kr.byID[k.id] = k // map keying de-duplicates (e.g. primary repeated)
	}
	return kr, nil
}

// PrimaryID is the fingerprint new writes are tagged with.
func (kr *Keyring) PrimaryID() string { return kr.primary.id }

// Encrypt produces a v2 envelope under the primary key. Empty input → empty
// output, so callers can store optional plaintext fields unconditionally.
func (kr *Keyring) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	if kr == nil || kr.primary == nil {
		return "", ErrNoKey
	}
	sealed, err := seal(kr.primary, plain)
	if err != nil {
		return "", err
	}
	return encPrefix + envelopeV2 + ":" + kr.primary.id + "]]" + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. Values without the envelope prefix are returned
// as-is (unencrypted plaintext values). Empty input → empty output.
func (kr *Keyring) Decrypt(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	if !IsEncrypted(enc) {
		return enc, nil
	}
	if kr == nil || kr.primary == nil {
		return "", ErrNoKey
	}
	version, keyID, body, err := parseEnvelope(enc)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", err
	}
	switch version {
	case envelopeV2:
		k := kr.byID[keyID]
		if k == nil {
			return "", fmt.Errorf("%w (id %s)", ErrUnknownKey, keyID)
		}
		return open(k, raw)
	default:
		return "", fmt.Errorf("unsupported encryption envelope version %q", version)
	}
}

// IsEncrypted reports whether a stored value carries the envelope prefix.
// Useful for migrations and callers that want to skip already-encrypted rows.
func IsEncrypted(v string) bool { return strings.HasPrefix(v, encPrefix) }

// NeedsRewrap reports whether a stored value should be re-encrypted under the
// current primary key. It is true for unencrypted plaintext (an upgrade) and
// v2 envelopes written under a retired (non-primary) key; false for empty
// values and values already sealed under the primary. The rewrap CLI uses it
// to skip rows that are already current, so retiring an old key only has to
// touch what actually references it.
func (kr *Keyring) NeedsRewrap(enc string) bool {
	if enc == "" {
		return false
	}
	if !IsEncrypted(enc) {
		return true // unencrypted → encrypt under the primary
	}
	version, keyID, _, err := parseEnvelope(enc)
	if err != nil {
		return true // malformed; let the rewrap attempt surface the error
	}
	return version != envelopeV2 || keyID != kr.primary.id
}

// parseEnvelope splits "[[enc:<version>[:<keyid>]]]<body>" into its parts.
func parseEnvelope(enc string) (version, keyID, body string, err error) {
	rest := enc[len(encPrefix):]
	end := strings.Index(rest, "]]")
	if end < 0 {
		return "", "", "", errors.New("malformed encryption envelope: missing ]] terminator")
	}
	header := rest[:end]
	body = rest[end+2:]
	if i := strings.IndexByte(header, ':'); i >= 0 {
		return header[:i], header[i+1:], body, nil
	}
	return header, "", body, nil
}

func seal(k *Key, plain string) ([]byte, error) {
	gcm, err := newGCM(k)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	// dst == nonce → Seal returns nonce||ciphertext in a single buffer.
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

func open(k *Key, raw []byte) (string, error) {
	gcm, err := newGCM(k)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func newGCM(k *Key) (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.bytes[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
