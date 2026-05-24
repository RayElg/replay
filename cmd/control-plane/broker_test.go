package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestIssueBrokerJWT_ShapeAndSignature(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	t.Setenv("REPLAY_BROKER_JWT_PRIVATE_KEY", base64.StdEncoding.EncodeToString(priv.Seed()))

	const (
		ws  = "00000000-0000-0000-0000-000000000001"
		uid = "user-42"
		cid = "replay-ws-abc-1"
	)
	tok, err := issueBrokerJWT(ws, uid, cid)
	if err != nil {
		t.Fatalf("issueBrokerJWT: %v", err)
	}

	// Standard 3-part JWT.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(parts))
	}

	// Header.
	hdr, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("header decode: %v", err)
	}
	if !strings.Contains(string(hdr), `"EdDSA"`) {
		t.Errorf("header should advertise EdDSA, got %s", hdr)
	}

	// Payload — verify the pgmqtt claim shape end-to-end so a future refactor
	// can't silently drop one of the fields pgmqtt enforces.
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	var c brokerClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if c.Subject != uid {
		t.Errorf("sub: want %q, got %q", uid, c.Subject)
	}
	if c.ClientID != cid {
		t.Errorf("client_id: want %q, got %q", cid, c.ClientID)
	}
	if len(c.SubClaims) != 1 || c.SubClaims[0] != "runs/"+ws+"/#" {
		t.Errorf("sub_claims should scope to workspace, got %v", c.SubClaims)
	}
	// Empty (not nil) — pgmqtt may treat "no claim key" differently from
	// "empty array" depending on version, and empty array is the explicit
	// "deny all publishes" signal.
	if c.PubClaims == nil {
		t.Error("pub_claims should be present (empty array), got nil")
	}
	if len(c.PubClaims) != 0 {
		t.Errorf("pub_claims should be empty, got %v", c.PubClaims)
	}
	if c.ExpiresAt <= time.Now().Unix() {
		t.Errorf("exp must be in the future, got %d (now=%d)", c.ExpiresAt, time.Now().Unix())
	}
	if c.IssuedAt == 0 {
		t.Error("iat should be set")
	}

	// Signature verifies against the derived public key — i.e., pgmqtt
	// configured with our derived public key will accept this token.
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("sig decode: %v", err)
	}
	body := parts[0] + "." + parts[1]
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), []byte(body), sig) {
		t.Fatal("signature failed to verify against derived public key")
	}
}

func TestIssueBrokerJWT_MissingKey(t *testing.T) {
	t.Setenv("REPLAY_BROKER_JWT_PRIVATE_KEY", "")
	_, err := issueBrokerJWT("ws", "uid", "cid")
	if err == nil {
		t.Fatal("expected error when private key env var is empty")
	}
}

func TestIssueBrokerJWT_BadKeySize(t *testing.T) {
	// 16 bytes of base64 instead of 32 — should be rejected, not used as a
	// truncated seed.
	t.Setenv("REPLAY_BROKER_JWT_PRIVATE_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	_, err := issueBrokerJWT("ws", "uid", "cid")
	if err == nil {
		t.Fatal("expected error when seed has wrong length")
	}
}

func TestBrokerJWTPublicKey_DerivesCorrectly(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	t.Setenv("REPLAY_BROKER_JWT_PRIVATE_KEY", base64.StdEncoding.EncodeToString(priv.Seed()))
	derived, err := brokerJWTPublicKey()
	if err != nil {
		t.Fatalf("brokerJWTPublicKey: %v", err)
	}
	if string(derived) != string(pub) {
		t.Fatal("derived public key does not match original keypair's public half")
	}
}

func TestParsePGTextArray(t *testing.T) {
	cases := map[string][]string{
		"{}":                   nil,
		`{"tls","jwt"}`:        {"tls", "jwt"},
		`{tls,jwt,multi_node}`: {"tls", "jwt", "multi_node"},
		`{"jwt"}`:              {"jwt"},
	}
	for in, want := range cases {
		got := parsePGTextArray(in)
		if len(got) != len(want) {
			t.Errorf("%q: len want %d, got %d (%v)", in, len(want), len(got), got)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q: index %d want %q got %q", in, i, want[i], got[i])
			}
		}
	}
}
