package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strconv"
	"strings"
	"testing"
)

// TestBuildGithubAppJWT_PKCS1 covers the legacy GitHub format (BEGIN RSA
// PRIVATE KEY) and verifies the resulting JWT verifies against the
// corresponding public key.
func TestBuildGithubAppJWT_PKCS1(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	jwt, err := buildGithubAppJWT("12345", string(pemBytes))
	if err != nil {
		t.Fatalf("buildGithubAppJWT: %v", err)
	}
	verifyJWT(t, jwt, &key.PublicKey, "12345")
}

// TestBuildGithubAppJWT_PKCS8 covers the modern format (BEGIN PRIVATE KEY)
// that GitHub now hands out by default.
func TestBuildGithubAppJWT_PKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	jwt, err := buildGithubAppJWT("99999", string(pemBytes))
	if err != nil {
		t.Fatalf("buildGithubAppJWT: %v", err)
	}
	verifyJWT(t, jwt, &key.PublicKey, "99999")
}

// TestBuildGithubAppJWT_RejectsBadPEM ensures we don't silently accept
// non-PEM input.
func TestBuildGithubAppJWT_RejectsBadPEM(t *testing.T) {
	if _, err := buildGithubAppJWT("1", "not a pem block"); err == nil {
		t.Fatal("expected error from non-PEM input")
	}
}

// verifyJWT cracks open the signed token, checks the signature against the
// supplied public key, and asserts the issuer matches.
func verifyJWT(t *testing.T, token string, pub *rsa.PublicKey, wantIss string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
	claimsB, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsB, &claims); err != nil {
		t.Fatalf("parse claims: %v", err)
	}
	// `iss` is JSON-encoded; we passed a string-typed appID so it should
	// round-trip as a string. Older callers might pass numeric — accept
	// either via a uniform stringify.
	gotIss := ""
	switch v := claims["iss"].(type) {
	case string:
		gotIss = v
	case float64:
		gotIss = strconv.FormatInt(int64(v), 10)
	}
	if gotIss != wantIss {
		t.Errorf("iss = %q, want %q", gotIss, wantIss)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("VerifyPKCS1v15: %v", err)
	}
}
