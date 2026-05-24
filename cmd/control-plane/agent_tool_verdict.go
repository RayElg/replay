package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Triage verdict tool. The agent calls this once it has reached a conclusion
// about a failure, writing a structured classification onto the run so the UI
// can show it at a glance and other code (auto PR comments) can branch on it.

// triageClassifications is the closed set the model may choose from. Kept in
// one place so the tool schema, validation, and the auto-triage prompt agree.
var triageClassifications = []string{
	"real_failure", // the app under test is broken
	"test_bug",     // the test/selector/assertion is wrong, app is fine
	"flake",        // non-deterministic; passed before/after without changes
	"environment",  // infra/config/network/data issue, not the app or the test
	"inconclusive", // not enough signal to decide
}

var triageConfidences = []string{"low", "medium", "high"}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// actionableVerdict reports whether a verdict warrants posting to a PR: a
// concrete, fixable conclusion the team can act on, held at a confidence worth
// surfacing. Flakes/environment/inconclusive are deliberately excluded.
func actionableVerdict(classification, confidence string) bool {
	return (classification == "real_failure" || classification == "test_bug") &&
		(confidence == "medium" || confidence == "high")
}

func handleSubmitTriageVerdict(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		Classification string `json:"classification"`
		Confidence     string `json:"confidence"`
		Summary        string `json:"summary"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	args.Classification = strings.TrimSpace(args.Classification)
	args.Confidence = strings.TrimSpace(args.Confidence)
	args.Summary = strings.TrimSpace(args.Summary)
	if !contains(triageClassifications, args.Classification) {
		return nil, fmt.Errorf("classification must be one of %s", strings.Join(triageClassifications, ", "))
	}
	if !contains(triageConfidences, args.Confidence) {
		return nil, fmt.Errorf("confidence must be one of %s", strings.Join(triageConfidences, ", "))
	}
	if args.Summary == "" {
		return nil, fmt.Errorf("summary is required")
	}

	res, err := d.DB.ExecContext(ctx, `
		UPDATE runs
		   SET triage_classification = $1,
		       triage_confidence     = $2,
		       triage_summary        = $3,
		       triaged_at            = now()
		 WHERE id = $4 AND workspace_id = $5`,
		args.Classification, args.Confidence, args.Summary, d.RunID, d.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("save verdict: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return map[string]any{
		"run_id":         d.RunID,
		"classification": args.Classification,
		"confidence":     args.Confidence,
		"actionable":     actionableVerdict(args.Classification, args.Confidence),
		"note":           "Verdict recorded on the run. It now shows in the run detail panel.",
	}, nil
}
