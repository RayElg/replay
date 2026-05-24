package replaycrypto

import (
	"encoding/base64"
	"testing"
)

func mustRing(t *testing.T, primary string, previous ...string) *Keyring {
	t.Helper()
	kr, err := NewKeyring(primary, previous...)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return kr
}

func TestRoundTrip(t *testing.T) {
	kr := mustRing(t, "test-key-please-do-not-use-in-prod")
	for _, plain := range []string{"", "short", "secret value with spaces", "🔐 unicode"} {
		enc, err := kr.Encrypt(plain)
		if err != nil {
			t.Fatalf("encrypt %q: %v", plain, err)
		}
		got, err := kr.Decrypt(enc)
		if err != nil {
			t.Fatalf("decrypt %q: %v", plain, err)
		}
		if got != plain {
			t.Fatalf("round-trip mismatch: want %q got %q", plain, got)
		}
	}
}

func TestPlaintextPassesThrough(t *testing.T) {
	kr := mustRing(t, "anything")
	got, err := kr.Decrypt("raw-plaintext-value")
	if err != nil {
		t.Fatal(err)
	}
	if got != "raw-plaintext-value" {
		t.Fatalf("plaintext pass-through failed: %q", got)
	}
}

func TestDeriveRejectsEmpty(t *testing.T) {
	if _, err := Derive("  "); err != ErrNoKey {
		t.Fatalf("expected ErrNoKey, got %v", err)
	}
	if _, err := NewKeyring(""); err != ErrNoKey {
		t.Fatalf("expected ErrNoKey from NewKeyring, got %v", err)
	}
}

func TestNonceVaries(t *testing.T) {
	kr := mustRing(t, "key")
	a, _ := kr.Encrypt("same")
	b, _ := kr.Encrypt("same")
	if a == b {
		t.Fatalf("nonce reuse detected — got identical envelopes")
	}
}

func TestIsEncrypted(t *testing.T) {
	kr := mustRing(t, "key")
	enc, _ := kr.Encrypt("x")
	if !IsEncrypted(enc) {
		t.Fatal("encrypted envelope not recognised")
	}
	if IsEncrypted("plaintext") {
		t.Fatal("plaintext mis-recognised as encrypted")
	}
}

// TestRotation is the core of the rotation story: data written under the old key
// must stay readable after the operator promotes a new primary and demotes the
// old one to REPLAY_ENCRYPT_KEY_PREVIOUS, while new writes use the new key.
func TestRotation(t *testing.T) {
	old := mustRing(t, "old-key")
	enc, _ := old.Encrypt("legacy-secret")

	rotated := mustRing(t, "new-key", "old-key")

	got, err := rotated.Decrypt(enc)
	if err != nil {
		t.Fatalf("post-rotation decrypt of old ciphertext: %v", err)
	}
	if got != "legacy-secret" {
		t.Fatalf("rotation lost data: want %q got %q", "legacy-secret", got)
	}

	// New writes are tagged with the new primary, not the retired key.
	fresh, _ := rotated.Encrypt("new-secret")
	if _, keyID, _, _ := parseEnvelope(fresh); keyID != rotated.PrimaryID() {
		t.Fatalf("new write tagged %q, want primary %q", keyID, rotated.PrimaryID())
	}

	// A ring that no longer carries the old key cannot read the old value, and
	// says so explicitly rather than silently returning garbage.
	if _, err := mustRing(t, "new-key").Decrypt(enc); err == nil {
		t.Fatal("expected ErrUnknownKey once the old key is dropped")
	}
}

func TestEnvelopeRecordsPrimaryFingerprint(t *testing.T) {
	kr := mustRing(t, "fingerprint-key")
	enc, _ := kr.Encrypt("v")
	_, keyID, _, err := parseEnvelope(enc)
	if err != nil {
		t.Fatal(err)
	}
	if keyID != kr.PrimaryID() {
		t.Fatalf("envelope key id %q != primary %q", keyID, kr.PrimaryID())
	}
}

// TestUnsupportedEnvelopeRejected confirms an envelope version the keyring
// doesn't understand fails loudly rather than silently returning garbage.
func TestUnsupportedEnvelopeRejected(t *testing.T) {
	kr := mustRing(t, "some-key")
	bogus := encPrefix + "v9]]" + base64.StdEncoding.EncodeToString([]byte("x"))
	if _, err := kr.Decrypt(bogus); err == nil {
		t.Fatal("unsupported envelope version decrypted without error")
	}
}

func TestNeedsRewrap(t *testing.T) {
	old := mustRing(t, "old-key")
	oldEnc, _ := old.Encrypt("v")

	rotated := mustRing(t, "new-key", "old-key")
	current, _ := rotated.Encrypt("v")

	cases := []struct {
		name string
		val  string
		want bool
	}{
		{"empty", "", false},
		{"plaintext", "raw-value", true},
		{"current primary", current, false},
		{"retired key", oldEnc, true},
		{"unknown version", encPrefix + "v9]]" + "x", true},
	}
	for _, c := range cases {
		if got := rotated.NeedsRewrap(c.val); got != c.want {
			t.Errorf("%s: NeedsRewrap=%v want %v", c.name, got, c.want)
		}
	}
}

// TestTamperedCiphertextRejected guards the GCM authentication tag.
func TestTamperedCiphertextRejected(t *testing.T) {
	kr := mustRing(t, "auth-key")
	enc, _ := kr.Encrypt("authentic")
	_, keyID, body, _ := parseEnvelope(enc)
	raw, _ := base64.StdEncoding.DecodeString(body)
	raw[len(raw)-1] ^= 0xff // flip a tag byte
	tampered := encPrefix + envelopeV2 + ":" + keyID + "]]" + base64.StdEncoding.EncodeToString(raw)
	if _, err := kr.Decrypt(tampered); err == nil {
		t.Fatal("tampered ciphertext decrypted without error")
	}
}
