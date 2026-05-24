package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// TestResult holds the outcome of a single Playwright test case.
// runScriptTest returns one per test; executeJob inserts a run_result row for each.
type TestResult struct {
	Title      string
	Status     string // "passed" | "failed" | "skipped" | "timedout"
	DurationMS int64
	Logs       string
	Screenshot []byte
	VideoPath  string // stable /tmp path, caller must os.Remove after upload
	TracePath  string // stable /tmp path, caller must os.Remove after upload
}

// ── Playwright JSON reporter wire types ──────────────────────────────────────
// Matches the output written by `--reporter=json` (Playwright ≥ 1.20).

type pwReport struct {
	Suites []pwSuite `json:"suites"`
}

type pwSuite struct {
	Title  string    `json:"title"`
	Suites []pwSuite `json:"suites"`
	Specs  []pwSpec  `json:"specs"`
}

type pwSpec struct {
	Title string   `json:"title"`
	Tests []pwTest `json:"tests"`
}

type pwTest struct {
	Results []pwTestResult `json:"results"`
}

type pwTestResult struct {
	Status      string         `json:"status"` // passed | failed | timedOut | skipped | interrupted
	Duration    int64          `json:"duration"`
	Error       *pwTestError   `json:"error"`
	Stdout      []pwOutputLine `json:"stdout"`
	Stderr      []pwOutputLine `json:"stderr"`
	Attachments []pwAttachment `json:"attachments"`
}

type pwTestError struct {
	Message string `json:"message"`
	Stack   string `json:"stack"`
}

type pwOutputLine struct {
	Text string `json:"text"`
}

type pwAttachment struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Path        string `json:"path"`
}

// parsePlaywrightResults reads the JSON reporter output file and returns one
// TestResult per test spec. Artifacts referenced by path are copied to stable
// /tmp paths (prefixed with runID + index) so the outputDir can be cleaned up
// by the caller before artifact upload begins.
//
// Returns an error only for file-read or JSON-parse failures. An empty slice
// with a nil error means the report was valid but had no specs (e.g. compile
// error before any test ran — the caller should fall back to a synthetic result).
func parsePlaywrightResults(reportPath, runID string) ([]TestResult, error) {
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, err
	}
	var report pwReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	var results []TestResult
	var walkSuite func(s pwSuite)
	walkSuite = func(s pwSuite) {
		for _, sub := range s.Suites {
			walkSuite(sub)
		}
		for _, spec := range s.Specs {
			for _, test := range spec.Tests {
				if len(test.Results) == 0 {
					continue
				}
				// Use the final attempt (last retry wins).
				r := test.Results[len(test.Results)-1]

				status := normaliseStatus(r.Status)

				var logBuf strings.Builder
				// Error stack is the most useful thing for a failing test.
				if r.Error != nil && r.Error.Stack != "" {
					logBuf.WriteString(r.Error.Stack)
					logBuf.WriteString("\n")
				}
				for _, o := range r.Stdout {
					logBuf.WriteString(o.Text)
				}
				for _, o := range r.Stderr {
					logBuf.WriteString(o.Text)
				}

				idx := len(results)
				tr := TestResult{
					Title:      spec.Title,
					Status:     status,
					DurationMS: r.Duration,
					Logs:       logBuf.String(),
				}

				for _, att := range r.Attachments {
					switch att.Name {
					case "screenshot":
						if imgData, err := os.ReadFile(att.Path); err == nil {
							tr.Screenshot = imgData
						}
					case "video":
						stable := fmt.Sprintf("/tmp/video-%s-%d.webm", runID, idx)
						if vd, err := os.ReadFile(att.Path); err == nil {
							if os.WriteFile(stable, vd, 0644) == nil {
								tr.VideoPath = stable
							}
						}
					case "trace":
						stable := fmt.Sprintf("/tmp/trace-%s-%d.zip", runID, idx)
						if td, err := os.ReadFile(att.Path); err == nil {
							if os.WriteFile(stable, td, 0644) == nil {
								tr.TracePath = stable
							}
						}
					}
				}

				results = append(results, tr)
			}
		}
	}

	for _, s := range report.Suites {
		walkSuite(s)
	}
	return results, nil
}

// normaliseStatus maps Playwright's status strings to the values accepted by the
// run_results CHECK constraint: passed | failed | skipped | timedout.
func normaliseStatus(s string) string {
	switch s {
	case "passed":
		return "passed"
	case "skipped":
		return "skipped"
	case "timedOut":
		return "timedout"
	default:
		// "failed", "interrupted", unexpected values → failed
		return "failed"
	}
}
