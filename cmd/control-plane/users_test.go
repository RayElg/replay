package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestRandomTokenShape(t *testing.T) {
	tok, err := randomToken(24)
	if err != nil {
		t.Fatalf("randomToken err: %v", err)
	}
	// 24 bytes → base64 RawURLEncoding → 32 chars.
	if len(tok) != 32 {
		t.Fatalf("expected 32 chars for 24 raw bytes, got %d (%q)", len(tok), tok)
	}
	// RawURLEncoding has no padding.
	if strings.ContainsAny(tok, "=+/") {
		t.Fatalf("token must be base64url with no padding, got %q", tok)
	}
	// Decode round-trip must succeed.
	if _, err := base64.RawURLEncoding.DecodeString(tok); err != nil {
		t.Fatalf("token is not valid base64url: %v", err)
	}
}

func TestRandomTokenUnique(t *testing.T) {
	// Quick collision sanity. With 24 bytes of entropy collisions across 100
	// draws are astronomically improbable; if this ever fails the RNG is broken.
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		tok, err := randomToken(24)
		if err != nil {
			t.Fatalf("randomToken err: %v", err)
		}
		if seen[tok] {
			t.Fatalf("randomToken collision after %d draws: %q", i, tok)
		}
		seen[tok] = true
	}
}

func TestExternalURLBaseRequiresEnv(t *testing.T) {
	t.Setenv("REPLAY_EXTERNAL_URL", "")
	if _, err := externalURLBase(); err == nil {
		t.Fatal("externalURLBase should error when REPLAY_EXTERNAL_URL is unset")
	}
}

func TestExternalURLBaseTrimsAndStripsSlash(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://replay.example.com", "https://replay.example.com"},
		{"https://replay.example.com/", "https://replay.example.com"},
		{"  https://replay.example.com/  ", "https://replay.example.com"},
		{"https://replay.example.com///", "https://replay.example.com"},
	}
	for _, c := range cases {
		t.Setenv("REPLAY_EXTERNAL_URL", c.in)
		got, err := externalURLBase()
		if err != nil {
			t.Fatalf("in=%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("in=%q got %q want %q", c.in, got, c.want)
		}
	}
}

func TestInviteAndResetLifetimesAreSane(t *testing.T) {
	// Surfacing in a test so a future change that flips the policy has to
	// edit a test along with the constant — keeps the README table accurate.
	if inviteLifetime.Hours() != 7*24 {
		t.Fatalf("inviteLifetime should be 7 days, got %v", inviteLifetime)
	}
	if resetLifetime.Hours() != 1 {
		t.Fatalf("resetLifetime should be 1 hour, got %v", resetLifetime)
	}
}
