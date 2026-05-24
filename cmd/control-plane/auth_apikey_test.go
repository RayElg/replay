package main

import (
	"net/http"
	"testing"
)

func TestNormaliseScopesDefaultsToAdmin(t *testing.T) {
	got := normaliseScopes(nil)
	if len(got) != 1 || got[0] != "admin" {
		t.Fatalf("expected [admin] for nil input, got %v", got)
	}
	got = normaliseScopes([]string{"  ", "garbage"})
	if len(got) != 1 || got[0] != "admin" {
		t.Fatalf("expected [admin] for all-invalid input, got %v", got)
	}
}

func TestNormaliseScopesDropsUnknownAndDedups(t *testing.T) {
	got := normaliseScopes([]string{"READ", "read", "frobnicate", "webhook"})
	if len(got) != 2 {
		t.Fatalf("expected 2 unique valid scopes, got %v", got)
	}
}

func TestApiKeyScopesAllow(t *testing.T) {
	cases := []struct {
		name   string
		scopes []string
		method string
		path   string
		want   bool
	}{
		{"empty scopes default to admin", nil, "POST", "/api/runs", true},
		{"admin allows everything", []string{"admin"}, "POST", "/api/runs", true},
		{"read allows GET", []string{"read"}, "GET", "/api/runs", true},
		{"read refuses POST", []string{"read"}, "POST", "/api/runs", false},
		{"webhook allows webhook path only", []string{"webhook"}, "POST", "/api/webhooks/run", true},
		{"webhook refuses other POST", []string{"webhook"}, "POST", "/api/runs", false},
		{"runner-only refuses everything", []string{"runner"}, "GET", "/api/runs", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := apiKeyScopesAllow(tc.scopes, tc.method, tc.path)
			if got != tc.want {
				t.Fatalf("scopes=%v method=%s path=%s: want %v got %v", tc.scopes, tc.method, tc.path, tc.want, got)
			}
		})
	}
}

func TestGenerateAPIKeyShape(t *testing.T) {
	full, prefix, err := generateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !startsWith(full, apiKeyPrefix) {
		t.Fatalf("full key missing prefix: %q", full)
	}
	if len(prefix) != 9 {
		t.Fatalf("prefix should be 9 chars, got %q", prefix)
	}
	if hashAPIKey(full) == hashAPIKey(full+"a") {
		t.Fatal("hash collision between distinct keys")
	}
}

// startsWith is a local helper to avoid pulling strings into the test for one call.
func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// _ keeps net/http imported even if all tests below get refactored away.
var _ = http.MethodGet
