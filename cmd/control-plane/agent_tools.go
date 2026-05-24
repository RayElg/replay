package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/minio/minio-go/v7"
)

// Tool-def singleton — built once, reused by both interactive chat and auto-triage.
var (
	toolOnce     sync.Once
	cachedDefs   []toolDef
	cachedByName map[string]toolDef
	cachedParams []anthropic.ToolUnionParam
)

func getTools() ([]toolDef, map[string]toolDef, []anthropic.ToolUnionParam) {
	toolOnce.Do(func() {
		cachedDefs = buildToolDefs()
		cachedByName = make(map[string]toolDef, len(cachedDefs))
		for _, t := range cachedDefs {
			cachedByName[t.Name] = t
		}
		cachedParams = toolUnionParams(cachedDefs)
	})
	return cachedDefs, cachedByName, cachedParams
}

// toolDeps is the bag of services tools may need. Constructed once per chat turn.
type toolDeps struct {
	DB          *sql.DB
	S3          *minio.Client
	Bucket      string
	RunID       string // the run this chat is anchored to — used as default for relative tools
	Repo        string // owner/repo slug from the run — used to select the matching GitHub integration
	WorkspaceID string // scopes integration lookups (GitHub etc.) to the correct tenant
}

// toolHandler runs a tool with model-supplied JSON input and returns a JSON-serialisable result.
// Returning a string is also fine; the caller json-encodes it before passing to Claude.
type toolHandler func(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error)

type toolDef struct {
	Name        string
	Description string
	Schema      map[string]any
	Mutating    bool
	Run         toolHandler
}

func buildToolDefs() []toolDef {
	defs := []toolDef{
		// ── Read-only ───────────────────────────────────────────────────
		{
			Name:        "read_script",
			Description: "Read the full source of a Playwright test script by ID. Returns name, filename, and content. Use this to inspect the script attached to the current run or any related script.",
			Schema: map[string]any{
				"type":     "object",
				"required": []string{"script_id"},
				"properties": map[string]any{
					"script_id": map[string]any{"type": "string", "description": "UUID of the script"},
				},
			},
			Run: handleReadScript,
		},
		{
			Name:        "list_similar_failures",
			Description: "List recent failed runs for the same script as this run. Useful for noticing whether a failure is new or a recurring flake. Returns at most 10 entries, most recent first.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 25},
				},
			},
			Run: handleListSimilarFailures,
		},
		{
			Name:        "read_artifact",
			Description: "Read a text-like artifact (logs, trace_summary JSON) by artifact ID. Binary artifacts (screenshots, video, trace.zip) cannot be read this way; use read_image for image artifacts instead.",
			Schema: map[string]any{
				"type":     "object",
				"required": []string{"artifact_id"},
				"properties": map[string]any{
					"artifact_id": map[string]any{"type": "string"},
				},
			},
			Run: handleReadArtifact,
		},
		{
			Name:        "read_image",
			Description: "View an image artifact (screenshot or video_frame) by artifact ID. Returns the image in base64-encoded format that you can view inline.",
			Schema: map[string]any{
				"type":     "object",
				"required": []string{"artifact_id"},
				"properties": map[string]any{
					"artifact_id": map[string]any{"type": "string"},
				},
			},
			Run: handleReadImage,
		},
		{
			Name:        "get_trace_summary",
			Description: "Return the parsed Playwright trace summary for the current run if available: action timeline, console messages, network errors. Prefer this over read_artifact when you specifically want the structured trace.",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Run: handleGetTraceSummary,
		},
		{
			Name:        "list_run_artifacts",
			Description: "List artifacts attached to the current run (or a specific run_id). Each entry has id, kind (screenshot|video|video_frame|trace|trace_summary), and filename. Use the id with read_artifact for text artifacts.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"run_id": map[string]any{"type": "string", "description": "Optional. Defaults to the current run."},
				},
			},
			Run: handleListRunArtifacts,
		},

		// ── GitHub (read-only, against the connected repo) ─────────────
		{
			Name:        "github_search_code",
			Description: "Search code in the connected GitHub repository. Use this to locate the implementation of a selector, page, or function the test references — useful for confirming whether a failure is a test bug or an app bug. Returns paths matching the query. The repo must have a Replay GitHub integration configured.",
			Schema: map[string]any{
				"type":     "object",
				"required": []string{"query"},
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "GitHub code-search query (e.g. \"cartTotal lang:typescript\" or \"data-testid=\\\"cart-total\\\"\")"},
					"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20},
				},
			},
			Run: handleGithubSearchCode,
		},
		{
			Name:        "github_read_file",
			Description: "Read a single file from the connected GitHub repository at a given path. Optionally specify a ref (branch/sha); defaults to the integration's configured branch (usually main). Refuses files larger than 200KB.",
			Schema: map[string]any{
				"type":     "object",
				"required": []string{"path"},
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Repo-relative path, e.g. \"src/cart/total.ts\""},
					"ref":  map[string]any{"type": "string", "description": "Branch, tag, or commit SHA. Optional."},
				},
			},
			Run: handleGithubReadFile,
		},
		{
			Name:        "github_list_directory",
			Description: "List a directory in the connected GitHub repository at a given path. Returns name/type/size for each entry. Use with empty path to list the repo root.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Repo-relative directory path. Use empty string for the repo root."},
					"ref":  map[string]any{"type": "string"},
				},
			},
			Run: handleGithubListDirectory,
		},
		// ── Mutating (each effects real state) ───────────────────────────
		{
			Name:        "rerun_run",
			Description: "Enqueue a fresh run using the SAME script and environment as the current (or specified) run. Use this to test whether a failure is reproducible. Returns the new run_id.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"run_id": map[string]any{"type": "string", "description": "Optional. Defaults to the current run."},
					"reason": map[string]any{"type": "string", "description": "Short user-facing note about why you re-ran."},
				},
			},
			Mutating: true,
			Run:      handleRerunRun,
		},
		{
			Name:        "propose_script_edit",
			Description: "Propose a small change to a script by replacing a unique substring. Prefer this over propose_script_patch when the change is localised — it uses much fewer tokens and produces a clean diff. The `find` text must match EXACTLY ONCE in the current script (whitespace and indentation included). Returns an error if it matches zero or multiple times.",
			Schema: map[string]any{
				"type":     "object",
				"required": []string{"script_id", "summary", "find", "replace"},
				"properties": map[string]any{
					"script_id": map[string]any{"type": "string"},
					"summary":   map[string]any{"type": "string", "description": "One-line description of the change"},
					"rationale": map[string]any{"type": "string", "description": "Why this fix addresses the failure"},
					"find":      map[string]any{"type": "string", "description": "Exact substring to replace. Must appear exactly once in the current script."},
					"replace":   map[string]any{"type": "string", "description": "The replacement text."},
				},
			},
			Mutating: true,
			Run:      handleProposeScriptEdit,
		},
		{
			Name:        "propose_script_patch",
			Description: "Propose a full-script rewrite. Prefer propose_script_edit for small changes — only use this when most of the file is changing. The patch is recorded in script_patches and a human must apply or reject it; this tool does NOT modify the script directly.",
			Schema: map[string]any{
				"type":     "object",
				"required": []string{"script_id", "summary", "new_content"},
				"properties": map[string]any{
					"script_id":   map[string]any{"type": "string"},
					"summary":     map[string]any{"type": "string", "description": "One-line description of the change"},
					"rationale":   map[string]any{"type": "string", "description": "Why this fix addresses the failure"},
					"new_content": map[string]any{"type": "string", "description": "The complete replacement script content"},
				},
			},
			Mutating: true,
			Run:      handleProposeScriptPatch,
		},
		{
			Name:        "submit_triage_verdict",
			Description: "Record your structured conclusion about the current run's failure. Call this exactly once, after you've investigated, as the final step of triage. `classification`: real_failure (the app is broken), test_bug (the test/selector/assertion is wrong), flake (non-deterministic), environment (infra/config/data, not app or test), or inconclusive. `confidence`: low | medium | high. `summary`: one short paragraph a developer can read at a glance. The verdict appears as a badge on the run.",
			Schema: map[string]any{
				"type":     "object",
				"required": []string{"classification", "confidence", "summary"},
				"properties": map[string]any{
					"classification": map[string]any{"type": "string", "enum": triageClassifications},
					"confidence":     map[string]any{"type": "string", "enum": triageConfidences},
					"summary":        map[string]any{"type": "string", "description": "One-paragraph plain-language verdict."},
				},
			},
			Mutating: true,
			Run:      handleSubmitTriageVerdict,
		},
		{
			Name:        "post_pr_comment",
			Description: "Post (or update) a comment on the GitHub pull request associated with the current run, via the connected GitHub integration. Use this to deliver your triage findings to the team on the PR. Re-invoking for the same run edits the existing comment rather than adding a new one, so it's safe to call again after a rerun. Requires a GitHub integration and an open PR for the run's branch/commit.",
			Schema: map[string]any{
				"type":     "object",
				"required": []string{"body"},
				"properties": map[string]any{
					"body":      map[string]any{"type": "string", "description": "Markdown comment body. Lead with the verdict, then the cause and any proposed fix."},
					"pr_number": map[string]any{"type": "integer", "description": "Optional. The PR number to comment on. If omitted, resolved from the run's commit/branch."},
				},
			},
			Mutating: true,
			Run:      handlePostPRComment,
		},
	}

	// Local-repo tools are gated on REPLAY_ALLOWED_REPO_PATHS being populated.
	// When unset we don't even register them, so the model isn't tempted to call
	// a tool that will only ever return an "access disabled" error.
	if len(allowedRepoPaths) > 0 {
		defs = append(defs, localRepoToolDefs()...)
	}

	return defs
}

// toolUnionParams converts our definitions into the SDK's ToolUnionParam list.
func toolUnionParams(defs []toolDef) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(defs))
	for _, d := range defs {
		schemaProps, _ := d.Schema["properties"].(map[string]any)
		schemaReq, _ := d.Schema["required"].([]string)
		t := anthropic.ToolParam{
			Name:        d.Name,
			Description: anthropic.String(d.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: schemaProps,
				Required:   schemaReq,
			},
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &t})
	}
	return out
}
