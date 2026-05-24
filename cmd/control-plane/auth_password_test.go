package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// resetLoginAttempts wipes the package-global state between subtests. Without
// this, attempt counts leak across cases and the sliding-window assertions get
// non-deterministic.
func resetLoginAttempts(t *testing.T) {
	t.Helper()
	loginAttemptsMu.Lock()
	defer loginAttemptsMu.Unlock()
	loginAttempts = map[string]*loginAttempt{}
}

func TestLoginAttemptKeyIsCaseInsensitive(t *testing.T) {
	if loginAttemptKey("User@Example.com", "1.2.3.4") != loginAttemptKey("user@example.com", "1.2.3.4") {
		t.Fatal("email casing should not split rate-limit buckets — attackers would just bounce case")
	}
	if loginAttemptKey("a@b.com", "1.2.3.4") == loginAttemptKey("a@b.com", "5.6.7.8") {
		t.Fatal("different IPs should produce different keys")
	}
}

func TestRecordLoginAttemptUnderThresholdAllows(t *testing.T) {
	resetLoginAttempts(t)
	key := "u@example.com|1.2.3.4"
	for i := 0; i < loginMaxAttempts; i++ {
		ok, retry := recordLoginAttempt(key)
		if !ok {
			t.Fatalf("attempt %d should be allowed (max=%d)", i+1, loginMaxAttempts)
		}
		if retry != 0 {
			t.Fatalf("retryAfter should be zero when allowed, got %v", retry)
		}
	}
}

func TestRecordLoginAttemptBlocksAtThreshold(t *testing.T) {
	resetLoginAttempts(t)
	key := "u@example.com|1.2.3.4"
	for i := 0; i < loginMaxAttempts; i++ {
		if ok, _ := recordLoginAttempt(key); !ok {
			t.Fatalf("attempt %d should still be allowed", i+1)
		}
	}
	ok, retry := recordLoginAttempt(key)
	if ok {
		t.Fatal("attempt at threshold+1 must be blocked")
	}
	if retry <= 0 || retry > loginWindow {
		t.Fatalf("retryAfter should be between 0 and %v, got %v", loginWindow, retry)
	}
}

func TestRecordLoginAttemptKeysAreIsolated(t *testing.T) {
	resetLoginAttempts(t)
	for i := 0; i < loginMaxAttempts; i++ {
		recordLoginAttempt("a@x.com|1.2.3.4")
	}
	if ok, _ := recordLoginAttempt("a@x.com|1.2.3.4"); ok {
		t.Fatal("first bucket should be exhausted")
	}
	if ok, _ := recordLoginAttempt("b@x.com|1.2.3.4"); !ok {
		t.Fatal("different email on same IP should have its own bucket")
	}
	if ok, _ := recordLoginAttempt("a@x.com|9.9.9.9"); !ok {
		t.Fatal("same email from different IP should have its own bucket")
	}
}

func TestRecordLoginAttemptSlidingWindow(t *testing.T) {
	resetLoginAttempts(t)
	key := "stale@x.com|1.2.3.4"
	// Plant timestamps that have already fallen out of the window.
	loginAttemptsMu.Lock()
	stale := time.Now().Add(-2 * loginWindow)
	loginAttempts[key] = &loginAttempt{timestamps: []time.Time{stale, stale, stale, stale, stale}}
	loginAttemptsMu.Unlock()
	if ok, _ := recordLoginAttempt(key); !ok {
		t.Fatal("entries outside the window must be dropped, freeing the slot")
	}
}

func TestClearLoginAttemptsResetsBucket(t *testing.T) {
	resetLoginAttempts(t)
	key := "u@x.com|1.2.3.4"
	for i := 0; i < loginMaxAttempts; i++ {
		recordLoginAttempt(key)
	}
	clearLoginAttempts(key)
	if ok, _ := recordLoginAttempt(key); !ok {
		t.Fatal("clearLoginAttempts should let the next attempt through")
	}
}

func TestClientIPPrefersXForwardedFor(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		remote  string
		want    string
	}{
		{"XFF single", map[string]string{"X-Forwarded-For": "5.5.5.5"}, "127.0.0.1:1234", "5.5.5.5"},
		{"XFF multi", map[string]string{"X-Forwarded-For": "5.5.5.5, 10.0.0.1"}, "127.0.0.1:1234", "5.5.5.5"},
		{"XFF trimmed whitespace", map[string]string{"X-Forwarded-For": "  5.5.5.5  ,  10.0.0.1"}, "127.0.0.1:1234", "5.5.5.5"},
		{"X-Real-IP only", map[string]string{"X-Real-IP": "6.6.6.6"}, "127.0.0.1:1234", "6.6.6.6"},
		{"RemoteAddr fallback", nil, "127.0.0.1:1234", "127.0.0.1"},
		{"RemoteAddr no port", nil, "127.0.0.1", "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			got := clientIP(r)
			if got != tc.want {
				t.Fatalf("clientIP=%q want %q", got, tc.want)
			}
		})
	}
}

func TestCookieSecureEnvOverridesAutoDetect(t *testing.T) {
	r, _ := http.NewRequest("GET", "https://x/", nil) // r.TLS still nil — that's fine
	r.Header.Set("X-Forwarded-Proto", "https")

	// Auto-detect: HTTPS-y request → secure.
	t.Setenv("REPLAY_COOKIE_SECURE", "")
	if !cookieSecure(r) {
		t.Fatal("auto-detect should return true when request is HTTPS")
	}

	// Explicit "false" overrides auto-detect even on HTTPS — useful for behind
	// an internal LB that terminates TLS but cookies still need to work to a
	// dev port.
	t.Setenv("REPLAY_COOKIE_SECURE", "false")
	if cookieSecure(r) {
		t.Fatal("REPLAY_COOKIE_SECURE=false must win over auto-detect")
	}

	// Explicit "true" overrides plain HTTP — operator opt-in for prod even
	// when the immediate hop is plain.
	plainReq, _ := http.NewRequest("GET", "/", nil)
	t.Setenv("REPLAY_COOKIE_SECURE", "true")
	if !cookieSecure(plainReq) {
		t.Fatal("REPLAY_COOKIE_SECURE=true must win over plain-HTTP auto-detect")
	}
}

func TestCookieSecureAcceptsBoolVariants(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	for _, v := range []string{"true", "TRUE", "1"} {
		t.Setenv("REPLAY_COOKIE_SECURE", v)
		if !cookieSecure(r) {
			t.Fatalf("REPLAY_COOKIE_SECURE=%q should be truthy", v)
		}
	}
	for _, v := range []string{"false", "FALSE", "0"} {
		t.Setenv("REPLAY_COOKIE_SECURE", v)
		if cookieSecure(r) {
			t.Fatalf("REPLAY_COOKIE_SECURE=%q should be falsy", v)
		}
	}
}

func TestSessionCookieIsHardenedDefaults(t *testing.T) {
	rec := newRecorder()
	r, _ := http.NewRequest("GET", "/", nil)
	t.Setenv("REPLAY_COOKIE_SECURE", "true")
	setSessionCookie(rec, r, "test-session-id")

	got := rec.Header().Get("Set-Cookie")
	for _, want := range []string{"HttpOnly", "SameSite=Lax", "Secure"} {
		if !strings.Contains(got, want) {
			t.Errorf("session cookie missing %q flag: %s", want, got)
		}
	}
	if !strings.Contains(got, "Path=/") {
		t.Errorf("session cookie should be scoped Path=/, got %s", got)
	}
}

// newRecorder is a minimal http.ResponseWriter for header inspection.
func newRecorder() *headerRecorder { return &headerRecorder{h: http.Header{}} }

type headerRecorder struct {
	h          http.Header
	statusCode int
}

func (r *headerRecorder) Header() http.Header         { return r.h }
func (r *headerRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *headerRecorder) WriteHeader(s int)           { r.statusCode = s }

// _ pins os.Getenv usage in case the file shrinks during refactors.
var _ = os.Getenv
