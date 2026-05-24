package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsePlaywrightResultsHappyPath(t *testing.T) {
	tmp := t.TempDir()
	report := filepath.Join(tmp, "results.json")
	body := `{
	  "suites": [{
	    "title": "outer",
	    "suites": [{
	      "title": "inner",
	      "specs": [{
	        "title": "spec one",
	        "tests": [{
	          "results": [{
	            "status": "passed",
	            "duration": 1234,
	            "stdout": [{"text":"hi\n"}],
	            "stderr": [],
	            "attachments": []
	          }]
	        }]
	      }]
	    }],
	    "specs": []
	  }]
	}`
	if err := os.WriteFile(report, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	results, err := parsePlaywrightResults(report, "test-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 test result, got %d", len(results))
	}
	if results[0].Title != "spec one" {
		t.Fatalf("title: %q", results[0].Title)
	}
	if results[0].Status != "passed" {
		t.Fatalf("status: %q", results[0].Status)
	}
	if results[0].DurationMS != 1234 {
		t.Fatalf("duration: %d", results[0].DurationMS)
	}
	if results[0].Logs == "" {
		t.Fatal("stdout should have been captured")
	}
}

func TestParsePlaywrightResultsUsesLastRetry(t *testing.T) {
	tmp := t.TempDir()
	report := filepath.Join(tmp, "results.json")
	body := `{
	  "suites": [{
	    "title": "outer",
	    "specs": [{
	      "title": "flaky",
	      "tests": [{
	        "results": [
	          {"status":"failed","duration":100,"error":{"message":"first try","stack":"x"}},
	          {"status":"passed","duration":200}
	        ]
	      }]
	    }],
	    "suites": []
	  }]
	}`
	if err := os.WriteFile(report, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	results, err := parsePlaywrightResults(report, "test-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "passed" {
		t.Fatalf("expected last-retry passed, got %+v", results)
	}
}

func TestNormaliseStatus(t *testing.T) {
	cases := map[string]string{
		"passed":      "passed",
		"failed":      "failed",
		"timedOut":    "timedout",
		"interrupted": "failed",
		"skipped":     "skipped",
		"weird":       "failed",
	}
	for in, want := range cases {
		if got := normaliseStatus(in); got != want {
			t.Fatalf("%q → %q, want %q", in, got, want)
		}
	}
}
