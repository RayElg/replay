package main

import (
	"strings"
	"testing"
)

func TestActionableVerdict(t *testing.T) {
	cases := []struct {
		class, conf string
		want        bool
	}{
		{"real_failure", "high", true},
		{"real_failure", "medium", true},
		{"real_failure", "low", false},
		{"test_bug", "medium", true},
		{"flake", "high", false},
		{"environment", "high", false},
		{"inconclusive", "high", false},
	}
	for _, c := range cases {
		if got := actionableVerdict(c.class, c.conf); got != c.want {
			t.Errorf("actionableVerdict(%q,%q)=%v want %v", c.class, c.conf, got, c.want)
		}
	}
}

func TestTriageEnumsClosed(t *testing.T) {
	if !contains(triageClassifications, "real_failure") || contains(triageClassifications, "bogus") {
		t.Fatal("triageClassifications membership wrong")
	}
	if !contains(triageConfidences, "medium") || contains(triageConfidences, "very_high") {
		t.Fatal("triageConfidences membership wrong")
	}
}

func TestFirstOpenPR(t *testing.T) {
	if n := firstOpenPR([]byte(`[{"number":7,"state":"closed"},{"number":12,"state":"open"}]`)); n != 12 {
		t.Errorf("expected first open PR 12, got %d", n)
	}
	if n := firstOpenPR([]byte(`[{"number":7,"state":"closed"}]`)); n != 0 {
		t.Errorf("expected 0 when no open PR, got %d", n)
	}
	if n := firstOpenPR([]byte(`not json`)); n != 0 {
		t.Errorf("expected 0 on bad json, got %d", n)
	}
}

func TestBuildPRCommentBody(t *testing.T) {
	t.Setenv("REPLAY_EXTERNAL_URL", "")
	root := "abcdef12-0000-0000-0000-000000000000"
	commit := "0123456789abcdef0123456789abcdef01234567"
	cfg := &githubConfig{Owner: "acme", Repo: "shop"}
	body := buildPRCommentBody("Real failure: cart total is wrong.", root, commit, prMarker(root), cfg)
	if !strings.Contains(body, prMarker(root)) {
		t.Error("body must embed the upsert marker")
	}
	if !strings.Contains(body, "Real failure: cart total is wrong.") {
		t.Error("body must preserve the agent's content")
	}
	if !strings.Contains(body, short(root)) {
		t.Error("footer should reference the short run id")
	}
	if !strings.Contains(body, "https://github.com/acme/shop/commit/"+commit) {
		t.Error("footer should link the commit to GitHub")
	}
	if !strings.Contains(body, short(commit)) {
		t.Error("commit link text should be the short SHA")
	}

	// No commit, no external URL: footer degrades gracefully, no broken links.
	bare := buildPRCommentBody("x", root, "", prMarker(root), nil)
	if strings.Contains(bare, "commit") {
		t.Error("no commit reference expected when commitSHA is empty")
	}
	if strings.Contains(bare, "](") {
		t.Error("no markdown links expected without external URL or commit")
	}
}
