package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyGithubSignature(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	secret := "s3cr3t"
	good := signBody(secret, body)

	cases := []struct {
		name   string
		sig    string
		secret string
		body   []byte
		want   bool
	}{
		{"valid", good, secret, body, true},
		{"wrong secret", good, "other", body, false},
		{"tampered body", good, secret, []byte(`{"ref":"refs/heads/evil"}`), false},
		{"empty secret", good, "", body, false},
		{"missing prefix", hex.EncodeToString([]byte("x")), secret, body, false},
		{"empty sig", "", secret, body, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verifyGithubSignature(c.sig, c.secret, c.body); got != c.want {
				t.Fatalf("verifyGithubSignature = %v, want %v", got, c.want)
			}
		})
	}
}

func TestChangedPaths(t *testing.T) {
	var p githubPushPayload
	p.Commits = make([]struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
	}, 2)
	p.Commits[0].Added = []string{"tests/a.spec.ts"}
	p.Commits[0].Modified = []string{"tests/b.spec.ts"}
	p.Commits[1].Modified = []string{"tests/b.spec.ts", "src/cart.ts"} // dup b + new

	set := changedPaths(&p)
	for _, want := range []string{"tests/a.spec.ts", "tests/b.spec.ts", "src/cart.ts"} {
		if !set[want] {
			t.Errorf("expected %q in changed set", want)
		}
	}
	if len(set) != 3 {
		t.Errorf("expected 3 unique paths, got %d", len(set))
	}
}
