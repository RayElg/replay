package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

const agentSystemPromptPreamble = `You are the Replay triage agent. You watch end-to-end Playwright tests run on behalf of a developer.

For every run you receive:
- The script's source code (TypeScript using @playwright/test)
- Stdout/stderr from the test process
- A trace summary (actions, console messages, network errors) — only when a Playwright trace was captured
- List of artifacts (screenshots, video frames, etc.) available for inspection
- Run metadata: status, branch, commit, environment

Tool calls and their results from earlier turns are replayed for you — if you already read a file last turn you do not need to re-read it.`

const agentSystemPromptStyle = `When triaging a failure, prefer the trace summary first (it's already in your context if available). Use read_artifact to inspect screenshots or video frames when they would clarify the failure. If the trace shows an assertion mismatch or a selector not found, use github_search_code to locate the corresponding implementation in the app code — that's how you'll know whether the test or the app is wrong.

IMPORTANT: When you call rerun_run, the tool enqueues the new run but does NOT wait for it to finish. The run will execute asynchronously. You will not be notified when it completes. After calling rerun_run, tell the user you've queued the rerun and suggest they check back in a moment (or refresh) to see the new run and its results.

You have a budget of 20 tool-using turns per message. On the very last turn the tools list is empty — at that point summarise what you have learned and give the user your best answer or next step.

Style: concise and technical. When a run failed, identify the most likely cause and propose a concrete fix or next diagnostic step. When the user asks something general about a passing run, summarise what was tested. When proposing script changes, prefer propose_script_edit (find-and-replace; cheaper and easier to review) over propose_script_patch (full rewrite). Show the user a short rationale in your reply. If you have read the actual source code and are certain you have located the exact lines responsible for a failure, include a suggested fix as a code block directly in your reply — don't wait for the user to ask.

When you finish triaging a failure, record your conclusion with submit_triage_verdict (classification + confidence + one-paragraph summary). It surfaces as a badge on the run and is how the team scans failures at a glance. Call it once, as the last step of triage.`

// buildSystemPrompt assembles the system prompt with the live tool list inlined,
// so adding a tool in buildToolDefs() updates the prompt automatically.
func buildSystemPrompt(defs []toolDef) string {
	var readOnly, mutating []string
	for _, d := range defs {
		line := fmt.Sprintf("- %s: %s", d.Name, firstSentence(d.Description))
		if d.Mutating {
			mutating = append(mutating, line)
		} else {
			readOnly = append(readOnly, line)
		}
	}
	var sb strings.Builder
	sb.WriteString(agentSystemPromptPreamble)
	sb.WriteString("\n\nYou have these tools (read-only):\n")
	sb.WriteString(strings.Join(readOnly, "\n"))
	if len(mutating) > 0 {
		sb.WriteString("\n\nMutating tools (these change real state — only call when the user has asked you to act or it's clearly what they want):\n")
		sb.WriteString(strings.Join(mutating, "\n"))
	}
	sb.WriteString("\n\n")
	sb.WriteString(agentSystemPromptStyle)
	return sb.String()
}

// firstSentence trims a description to its first sentence so the system prompt
// stays compact. Tool descriptions remain rich in the API tool definitions.
func firstSentence(s string) string {
	for i, ch := range s {
		if ch == '.' && i > 10 {
			return s[:i+1]
		}
	}
	return s
}

const (
	maxFrames      = 5
	maxLogBytes    = 8000
	maxToolLoops   = 20
	maxTokens      = 12000
	thinkingBudget = 2048 // tokens reserved for extended thinking per turn
)

type chatRequest struct {
	Message string `json:"message"`
}

// resolveRootRunID looks up runs.root_run_id for any given run id. The agent chat
// is keyed by the root, so reruns join the same conversation transparently.
// Workspace-agnostic — only safe to call from background jobs that already have
// a trusted run id. HTTP handlers must use resolveRootRunIDScoped.
func resolveRootRunID(ctx context.Context, db *sql.DB, runID string) (string, error) {
	var root string
	err := db.QueryRowContext(ctx, `SELECT root_run_id FROM runs WHERE id = $1`, runID).Scan(&root)
	if err != nil {
		return "", err
	}
	return root, nil
}

// resolveRootRunIDScoped is the tenant-safe variant: returns sql.ErrNoRows if the
// run id is not in the caller's workspace. Use from HTTP handlers.
func resolveRootRunIDScoped(ctx context.Context, db *sql.DB, runID string) (string, error) {
	var root string
	err := db.QueryRowContext(ctx,
		`SELECT root_run_id FROM runs WHERE id = $1 AND workspace_id = $2`,
		runID, workspaceFromContext(ctx),
	).Scan(&root)
	if err != nil {
		return "", err
	}
	return root, nil
}

// runContext is the per-call snapshot we feed Claude for ONE run. Built fresh on every
// chat turn so we always show the latest state (artifacts can land asynchronously).
type runContext struct {
	RunID          string
	Status         string
	Branch         string
	CommitSHA      string
	Repo           string // owner/repo slug, empty if not set
	ScriptID       string
	ScriptName     string
	ScriptContent  string
	ScriptAgentsMD string
	EnvSlug        string
	CreatedAt      time.Time
	Logs           string
	Frames         [][]byte
	FrameMimes     []string
	TraceSummary   string // raw JSON text, may be empty
	Artifacts      []artifactRef
}

type artifactRef struct {
	ID       string
	Kind     string
	Filename string
}

// convoContext is a chain of runs sharing the same root — i.e. one conversation,
// possibly with reruns. Runs are ordered oldest → newest; the last entry is the
// most recent run and carries full detail (frames, full logs). Older runs are
// summarized to keep the context size manageable.
type convoContext struct {
	RootRunID string
	Runs      []*runContext // oldest → newest, len >= 1
}

// loadConversationContext fetches all runs sharing the given root and returns
// per-run contexts. Caps total frames across the conversation at maxFrames and
// only attaches them to the most recent run.
func loadConversationContext(ctx context.Context, db *sql.DB, s3 *minio.Client, bucket, rootRunID string) (*convoContext, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id FROM runs WHERE root_run_id = $1 ORDER BY created_at ASC, id ASC`, rootRunID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var rid string
		rows.Scan(&rid)
		ids = append(ids, rid)
	}
	rows.Close()
	if len(ids) == 0 {
		// Defensive fallback: the root might not have its own row visible yet
		// (race during create); just load it directly.
		ids = []string{rootRunID}
	}

	cc := &convoContext{RootRunID: rootRunID, Runs: make([]*runContext, 0, len(ids))}
	for i, rid := range ids {
		attachFrames := i == len(ids)-1 // only for the most recent run
		rc, err := loadRunContext(ctx, db, s3, bucket, rid, attachFrames)
		if err != nil {
			slog.Warn("loadRunContext failed", "run_id", rid, "error", err)
			continue
		}
		cc.Runs = append(cc.Runs, rc)
	}
	return cc, nil
}

func loadRunContext(ctx context.Context, db *sql.DB, s3 *minio.Client, bucket, runID string, attachFrames bool) (*runContext, error) {
	rc := &runContext{RunID: runID}
	var scriptID, scriptName, scriptContent, scriptAgentsMD, envSlug, branch, commit, repo sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT r.status, r.branch, r.commit_sha, r.repo, r.script_id, s.name, s.content, s.agents_md, e.slug, r.created_at
		FROM runs r
		LEFT JOIN scripts s ON s.id = r.script_id
		LEFT JOIN environments e ON e.id = r.env_id
		WHERE r.id = $1`, runID).Scan(&rc.Status, &branch, &commit, &repo, &scriptID, &scriptName, &scriptContent, &scriptAgentsMD, &envSlug, &rc.CreatedAt)
	if err != nil {
		return nil, err
	}
	rc.Branch = branch.String
	rc.CommitSHA = commit.String
	rc.Repo = repo.String
	rc.ScriptID = scriptID.String
	rc.ScriptName = scriptName.String
	rc.ScriptContent = scriptContent.String
	rc.ScriptAgentsMD = scriptAgentsMD.String
	rc.EnvSlug = envSlug.String

	resRows, err := db.QueryContext(ctx, `SELECT logs FROM run_results WHERE run_id = $1`, runID)
	if err != nil {
		return nil, err
	}
	defer resRows.Close()
	var sb strings.Builder
	for resRows.Next() {
		var l string
		resRows.Scan(&l)
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	rc.Logs = sb.String()

	// Pull all artifact rows so the agent can reference them by id/kind/filename through tools.
	artRows, err := db.QueryContext(ctx, `
		SELECT a.id, a.kind, a.storage_key
		FROM artifacts a JOIN run_results rr ON a.run_result_id = rr.id
		WHERE rr.run_id = $1
		ORDER BY a.id`, runID)
	if err != nil {
		return nil, err
	}
	defer artRows.Close()
	var traceSummaryKey string
	for artRows.Next() {
		var id, kind, key string
		artRows.Scan(&id, &kind, &key)
		base := key
		if i := strings.LastIndex(key, "/"); i >= 0 {
			base = key[i+1:]
		}
		rc.Artifacts = append(rc.Artifacts, artifactRef{ID: id, Kind: kind, Filename: base})
		if kind == "trace_summary" && traceSummaryKey == "" {
			traceSummaryKey = key
		}
	}

	// Fetch the trace_summary JSON inline so it shows up in the first user turn.
	if traceSummaryKey != "" {
		if obj, oerr := s3.GetObject(ctx, bucket, traceSummaryKey, minio.GetObjectOptions{}); oerr == nil {
			data, rerr := io.ReadAll(obj)
			obj.Close()
			if rerr == nil {
				rc.TraceSummary = string(data)
			}
		}
	}

	if !attachFrames {
		// Older runs in a conversation chain skip image attachment to keep the
		// context size manageable. They're still visible to the agent via the
		// artifact list and can be fetched on demand with read_artifact.
		return rc, nil
	}

	// Prefer screenshots first, then video_frame keyframes. Limit total so we stay under Claude's image cap and keep latency low.
	imgRows, err := db.QueryContext(ctx, `
		SELECT a.storage_key,
		       CASE a.kind WHEN 'screenshot' THEN 0 ELSE 1 END AS prio
		FROM artifacts a
		JOIN run_results rr ON a.run_result_id = rr.id
		WHERE rr.run_id = $1 AND a.kind IN ('screenshot','video_frame')
		ORDER BY prio, a.storage_key
		LIMIT $2`, runID, maxFrames)
	if err != nil {
		return nil, err
	}
	defer imgRows.Close()
	for imgRows.Next() {
		var key string
		var prio int
		imgRows.Scan(&key, &prio)
		obj, err := s3.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
		if err != nil {
			slog.Warn("agent: get artifact failed", "key", key, "error", err)
			continue
		}
		data, err := io.ReadAll(obj)
		obj.Close()
		if err != nil {
			slog.Warn("agent: read artifact failed", "key", key, "error", err)
			continue
		}
		mime := "image/jpeg"
		lk := strings.ToLower(key)
		switch {
		case strings.HasSuffix(lk, ".png"):
			mime = "image/png"
		case strings.HasSuffix(lk, ".webp"):
			mime = "image/webp"
		case strings.HasSuffix(lk, ".gif"):
			mime = "image/gif"
		}
		rc.Frames = append(rc.Frames, data)
		rc.FrameMimes = append(rc.FrameMimes, mime)
	}
	return rc, nil
}

// truncate keeps the first n bytes of s but never cuts mid-rune. If the byte
// budget would end inside a multi-byte UTF-8 codepoint, we step back to the
// previous rune boundary so the JSON encoder downstream doesn't choke.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n...[truncated]"
}

// firstUserBlocks composes the leading user turn: a context preamble for every
// run in the conversation, vision frames for the most recent run, then the user's
// actual question. Used only for the first user message of an API call —
// subsequent turns are plain text.
func (cc *convoContext) firstUserBlocks(userText string) []anthropic.ContentBlockParamUnion {
	var sb strings.Builder
	if len(cc.Runs) > 1 {
		fmt.Fprintf(&sb, "=== Replay conversation (%d runs share this chat) ===\n", len(cc.Runs))
		fmt.Fprintf(&sb, "All runs in this conversation share root %s. The most recent run is the live one.\n\n", short(cc.RootRunID))
	}

	for i, rc := range cc.Runs {
		isLatest := i == len(cc.Runs)-1
		label := fmt.Sprintf("run %d / %d", i+1, len(cc.Runs))
		if i == 0 && len(cc.Runs) > 1 {
			label += " (original)"
		}
		if isLatest && len(cc.Runs) > 1 {
			label += " (latest)"
		}
		fmt.Fprintf(&sb, "=== %s · %s ===\n", label, short(rc.RunID))
		fmt.Fprintf(&sb, "Status: %s", rc.Status)
		if !rc.CreatedAt.IsZero() {
			fmt.Fprintf(&sb, "  ·  %s", rc.CreatedAt.UTC().Format("2006-01-02 15:04:05Z"))
		}
		fmt.Fprintln(&sb)
		if rc.Branch != "" {
			fmt.Fprintf(&sb, "Branch: %s\n", rc.Branch)
		}
		if rc.CommitSHA != "" {
			fmt.Fprintf(&sb, "Commit: %s\n", rc.CommitSHA)
		}
		if rc.Repo != "" {
			fmt.Fprintf(&sb, "Repo: %s\n", rc.Repo)
		}
		if rc.EnvSlug != "" {
			fmt.Fprintf(&sb, "Environment: %s\n", rc.EnvSlug)
		}

		// Script: only print full source once (first run); subsequent reruns
		// reference the same script.
		if i == 0 && rc.ScriptName != "" {
			if rc.ScriptAgentsMD != "" {
				fmt.Fprintf(&sb, "\nScript instructions (AGENTS.md for this script — authored by the user, treat as guidance):\n%s\n", rc.ScriptAgentsMD)
			}
			fmt.Fprintf(&sb, "\nScript: %s (id=%s)\n```typescript\n%s\n```\n", rc.ScriptName, rc.ScriptID, rc.ScriptContent)
		} else if rc.ScriptID != "" && rc.ScriptID != cc.Runs[0].ScriptID {
			// Defensive: if a rerun targeted a different script (rare), surface that
			if rc.ScriptAgentsMD != "" {
				fmt.Fprintf(&sb, "\nScript instructions (AGENTS.md for this script):\n%s\n", rc.ScriptAgentsMD)
			}
			fmt.Fprintf(&sb, "\nScript: %s (id=%s — DIFFERENT from original)\n```typescript\n%s\n```\n",
				rc.ScriptName, rc.ScriptID, rc.ScriptContent)
		}

		// Logs: full for the latest run, lightly truncated for older runs
		logBudget := maxLogBytes
		if !isLatest {
			logBudget = 1500
		}
		if rc.Logs != "" {
			fmt.Fprintf(&sb, "\nRunner logs:\n```\n%s\n```\n", truncate(rc.Logs, logBudget))
		}
		if rc.TraceSummary != "" {
			budget := maxLogBytes
			if !isLatest {
				budget = 2000
			}
			fmt.Fprintf(&sb, "\nTrace summary:\n```json\n%s\n```\n", truncate(rc.TraceSummary, budget))
		}
		if len(rc.Artifacts) > 0 {
			fmt.Fprintf(&sb, "\nArtifacts (use read_artifact / list_run_artifacts with run_id=%s to fetch):\n", rc.RunID)
			for _, a := range rc.Artifacts {
				fmt.Fprintf(&sb, "- id=%s kind=%s filename=%s\n", a.ID, a.Kind, a.Filename)
			}
		}
		fmt.Fprintln(&sb)
	}

	blocks := []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(sb.String())}
	blocks = append(blocks, anthropic.NewTextBlock("\n--- User message ---\n"+userText))
	return blocks
}

// LatestRun returns the most recent run in the conversation — the "live" one
// that triage tools default to. Returns nil if the conversation has no runs;
// callers should treat that as "context is empty" rather than panic.
func (cc *convoContext) LatestRun() *runContext {
	if cc == nil || len(cc.Runs) == 0 {
		return nil
	}
	return cc.Runs[len(cc.Runs)-1]
}

func short(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

type historyMsg struct {
	ID      string
	Who     string // "user" | "agent" | "system"
	Kind    string // "chat" | "tool_call" | "tool_result"
	Content string
	Source  string
}

// loadChatHistory returns every persisted row for the conversation, in insertion
// order: user/agent chat turns, and the tool_call/tool_result pairs the agent
// emitted in between. Tool rows are replayed in buildAPIMessages so the model
// can see what its earlier tools returned without re-running them.
//
// workspaceID is a defense-in-depth filter — pass empty string from trusted
// background jobs that already vetted run ownership.
func loadChatHistory(ctx context.Context, db *sql.DB, runID, workspaceID string) ([]historyMsg, error) {
	const baseQ = `
		SELECT id, who, kind, content, COALESCE(source, '') FROM agent_messages
		WHERE run_id = $1 AND kind IN ('chat', 'tool_call', 'tool_result')`
	const orderQ = ` ORDER BY created_at ASC, id ASC`
	var rows *sql.Rows
	var err error
	if workspaceID == "" {
		rows, err = db.QueryContext(ctx, baseQ+orderQ, runID)
	} else {
		rows, err = db.QueryContext(ctx, baseQ+` AND workspace_id = $2`+orderQ, runID, workspaceID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []historyMsg
	for rows.Next() {
		var m historyMsg
		rows.Scan(&m.ID, &m.Who, &m.Kind, &m.Content, &m.Source)
		out = append(out, m)
	}
	return out, nil
}

// buildAPIMessages converts persisted history + a fresh user message into the
// Anthropic MessageParam list. Tool calls and their results are replayed so the
// model has the same evidence it had at the time it spoke — otherwise it tends
// to re-run the same reads each turn or contradict its own prior conclusions.
//
// Reconstruction rules:
//   - user chat → user message (the first one carries the run-context preamble + vision)
//   - agent chat → assistant message (text-only, the final synthesis from that turn)
//   - agent tool_call → assistant message containing one tool_use block
//   - system tool_result → user message containing one tool_result block
//
// We emit each tool_call/tool_result as its own one-block message rather than
// trying to recover the original "(text + tool_use_a + tool_use_b)" grouping —
// the model handles either form and the simpler shape is far easier to keep
// correct under partial history (e.g. a tool_call whose result row is missing
// because the process crashed mid-execution).
func buildAPIMessages(cc *convoContext, history []historyMsg, newUserText string) []anthropic.MessageParam {
	var msgs []anthropic.MessageParam
	contextAttached := false

	// Track which tool_use IDs we've already replayed as assistant messages, so
	// we only emit a tool_result for ones the model "remembers" calling.
	seenToolUse := map[string]bool{}

	for _, h := range history {
		switch h.Kind {
		case "chat":
			if strings.TrimSpace(h.Content) == "" {
				continue
			}
			if h.Who == "user" {
				if !contextAttached {
					msgs = append(msgs, anthropic.NewUserMessage(cc.firstUserBlocks(h.Content)...))
					contextAttached = true
				} else {
					msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(h.Content)))
				}
			} else if h.Who == "agent" {
				msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(h.Content)))
			}
		case "tool_call":
			var meta struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal([]byte(h.Content), &meta); err != nil || meta.ID == "" {
				continue
			}
			seenToolUse[meta.ID] = true
			msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.ContentBlockParamUnion{
				OfToolUse: &anthropic.ToolUseBlockParam{
					ID:    meta.ID,
					Name:  meta.Name,
					Input: meta.Input,
				},
			}))
		case "tool_result":
			var meta struct {
				ToolUseID string `json:"tool_use_id"`
				IsError   bool   `json:"is_error"`
				Content   string `json:"content"`
			}
			if err := json.Unmarshal([]byte(h.Content), &meta); err != nil || meta.ToolUseID == "" {
				continue
			}
			if !seenToolUse[meta.ToolUseID] {
				// Result without a matching tool_use replay — skip rather than confuse the model.
				continue
			}
			msgs = append(msgs, anthropic.NewUserMessage(
				anthropic.NewToolResultBlock(meta.ToolUseID, meta.Content, meta.IsError),
			))
		}
	}

	if !contextAttached {
		msgs = append(msgs, anthropic.NewUserMessage(cc.firstUserBlocks(newUserText)...))
	} else {
		msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(newUserText)))
	}
	return msgs
}

// writeSSE writes one Server-Sent Event with a JSON payload. The writer is
// shared between http.ResponseWriter and the agentBus byte channel, so we
// accept the smallest interface that both satisfy.
func writeSSE(w interface{ Write([]byte) (int, error) }, event string, payload any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

// runAgentTurn streams one Claude turn, accumulating an assistant Message.
// emit is nil for fully silent background runs, or an eventEmitter to surface
// SSE events to HTTP clients or the agentBus.
// Returns the fully accumulated Message so the caller can inspect tool_use blocks.
func runAgentTurn(
	ctx context.Context,
	emit eventEmitter,
	anthClient *anthropic.Client, model, systemPrompt string,
	apiMsgs []anthropic.MessageParam, tools []anthropic.ToolUnionParam,
) (anthropic.Message, error) {
	stream := anthClient.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: maxTokens,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages:  apiMsgs,
		Tools:     tools,
		// Extended thinking gives the model room to plan before responding — especially
		// useful for failure triage where the next move is often "look at X, then Y".
		// Temperature must be 1 when thinking is enabled.
		Thinking:    anthropic.ThinkingConfigParamOfEnabled(thinkingBudget),
		Temperature: anthropic.Float(1),
	})

	var msg anthropic.Message
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			slog.Warn("accumulate failed", "error", err)
		}
		if emit == nil {
			continue
		}
		// Surface text + thinking deltas to the client as they arrive.
		if v, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			delta := v.Delta.AsAny()
			switch d := delta.(type) {
			case anthropic.TextDelta:
				if d.Text != "" {
					emit.emit("delta", map[string]string{"text": d.Text})
				}
			case anthropic.ThinkingDelta:
				if d.Thinking != "" {
					emit.emit("thinking_delta", map[string]string{"text": d.Thinking})
				}
			}
		}
		// Notify the client when a new content block starts so the UI can open
		// a fresh thinking pane / text bubble before deltas arrive.
		if v, ok := ev.AsAny().(anthropic.ContentBlockStartEvent); ok {
			switch v.ContentBlock.Type {
			case "thinking":
				emit.emit("thinking_start", map[string]any{"index": v.Index})
			case "text":
				emit.emit("text_start", map[string]any{"index": v.Index})
			}
		}
	}
	return msg, stream.Err()
}

type agentLoopResult struct {
	FinalText string
	TokensIn  int64
	TokensOut int64
}

// runAgentLoop runs the full tool-use loop for one user turn.
// emit controls SSE delivery: nil = silent, httpEmitter = interactive chat, busEmitter = auto-triage.
// Tool calls and results are always persisted to agent_messages under rootRunID.
// messageSource labels the final assistant message (empty = user-initiated, 'auto_triage' = background).
func runAgentLoop(
	ctx context.Context,
	db *sql.DB,
	emit eventEmitter,
	anthClient *anthropic.Client, model string,
	apiMsgs []anthropic.MessageParam,
	defs []toolDef,
	tools []anthropic.ToolUnionParam,
	toolByName map[string]toolDef,
	deps *toolDeps,
	rootRunID string,
	messageSource string,
) (agentLoopResult, error) {
	var res agentLoopResult
	systemPrompt := buildSystemPrompt(defs)

	for iter := 0; iter < maxToolLoops; iter++ {
		// On the final allowed turn, drop the tool list so the model is forced to
		// emit a text reply with whatever it has — instead of just running out of
		// turns mid-tool-use and disappearing on the user.
		turnTools := tools
		if iter == maxToolLoops-1 {
			turnTools = nil
		}
		msg, err := runAgentTurn(ctx, emit, anthClient, model, systemPrompt, apiMsgs, turnTools)
		if err != nil {
			return res, err
		}
		res.TokensIn += msg.Usage.InputTokens
		res.TokensOut += msg.Usage.OutputTokens

		for _, block := range msg.Content {
			if v, ok := block.AsAny().(anthropic.TextBlock); ok {
				res.FinalText += v.Text
			}
		}
		apiMsgs = append(apiMsgs, msg.ToParam())

		var toolUses []anthropic.ToolUseBlock
		for _, block := range msg.Content {
			if v, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
				toolUses = append(toolUses, v)
			}
		}
		if len(toolUses) == 0 || msg.StopReason != anthropic.StopReasonToolUse {
			break
		}

		var toolResultBlocks []anthropic.ContentBlockParamUnion
		var imageBlocks []anthropic.ContentBlockParamUnion
		for _, tu := range toolUses {
			def, known := toolByName[tu.Name]
			toolCallMeta := map[string]any{
				"id":    tu.ID,
				"name":  tu.Name,
				"input": json.RawMessage(tu.Input),
			}
			if emit != nil {
				emit.emit("tool_call", toolCallMeta)
			}
			if rawCall, _ := json.Marshal(toolCallMeta); rawCall != nil {
				_, _ = db.ExecContext(ctx,
					`INSERT INTO agent_messages (id, run_id, who, kind, content) VALUES ($1, $2, 'agent', 'tool_call', $3)`,
					uuid.New().String(), rootRunID, string(rawCall))
			}

			var resultPayload any
			var resultErrText string
			if !known {
				resultErrText = fmt.Sprintf("unknown tool: %s", tu.Name)
			} else {
				out, terr := def.Run(ctx, deps, tu.Input)
				if terr != nil {
					resultErrText = terr.Error()
				} else {
					resultPayload = out
				}
			}
			var resultString string
			isErr := resultErrText != ""
			if isErr {
				resultString = resultErrText
			} else {
				encoded, _ := json.Marshal(resultPayload)
				resultString = string(encoded)
			}

			toolResultMeta := map[string]any{
				"tool_use_id": tu.ID,
				"name":        tu.Name,
				"is_error":    isErr,
				"content":     resultString,
			}
			if emit != nil {
				emit.emit("tool_result", toolResultMeta)
			}
			if rawResult, _ := json.Marshal(toolResultMeta); rawResult != nil {
				_, _ = db.ExecContext(ctx,
					`INSERT INTO agent_messages (id, run_id, who, kind, content) VALUES ($1, $2, 'system', 'tool_result', $3)`,
					uuid.New().String(), rootRunID, string(rawResult))
			}

			toolResultBlocks = append(toolResultBlocks, anthropic.NewToolResultBlock(tu.ID, resultString, isErr))

			if !isErr && tu.Name == "read_image" && resultPayload != nil {
				if resultMap, ok := resultPayload.(map[string]any); ok {
					if b64Str, hasB64 := resultMap["base64"].(string); hasB64 && b64Str != "" {
						if mediaType, hasType := resultMap["media_type"].(string); hasType {
							imageBlocks = append(imageBlocks, anthropic.NewImageBlock(anthropic.Base64ImageSourceParam{
								Data:      b64Str,
								MediaType: anthropic.Base64ImageSourceMediaType(mediaType),
							}))
						}
					}
				}
			}
		}
		apiMsgs = append(apiMsgs, anthropic.NewUserMessage(toolResultBlocks...))
		if len(imageBlocks) > 0 {
			apiMsgs = append(apiMsgs, anthropic.NewUserMessage(imageBlocks...))
		}
	}

	agentMsgID := uuid.New().String()
	var sourceVal interface{}
	if messageSource != "" {
		sourceVal = messageSource
	}
	// Anthropic rejects empty text blocks; if the model only produced tool calls
	// with no accompanying text, substitute a minimal non-empty placeholder so
	// this row doesn't break the next turn's history replay.
	finalText := res.FinalText
	if strings.TrimSpace(finalText) == "" {
		finalText = "(no text — tools only)"
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO agent_messages (id, run_id, who, kind, source, content, model, tokens_in, tokens_out)
		 VALUES ($1, $2, 'agent', 'chat', $3, $4, $5, $6, $7)`,
		agentMsgID, rootRunID, sourceVal, finalText, model, res.TokensIn, res.TokensOut,
	); err != nil {
		slog.Error("persist agent reply failed", "error", err)
	}

	if emit != nil {
		emit.emit("done", map[string]any{
			"id":         agentMsgID,
			"tokens_in":  res.TokensIn,
			"tokens_out": res.TokensOut,
		})
	}

	return res, nil
}

func registerAgentRoutes(r chi.Router, db *sql.DB, s3 *minio.Client, bucket string, anthClient *anthropic.Client, model string) {
	defs, toolByName, tools := getTools()

	r.Get("/api/runs/{id}/agent/messages", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		// Conversation history lives on the root run — resolve so reruns share the same chat.
		root, err := resolveRootRunIDScoped(r.Context(), db, id)
		if err != nil {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		id = root
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, who, kind, content, created_at, source
			FROM agent_messages
			WHERE run_id = $1 AND workspace_id = $2
			ORDER BY created_at ASC`, id, workspaceFromContext(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []map[string]interface{}{}
		for rows.Next() {
			var mid, who, kind, content string
			var ts time.Time
			var source *string
			rows.Scan(&mid, &who, &kind, &content, &ts, &source)
			out = append(out, map[string]interface{}{
				"id": mid, "who": who, "kind": kind, "content": content, "created_at": ts, "source": source,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})

	r.Get("/api/runs/{id}/agent/conversation", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		// Resolve to root run so reruns report the same conversation.
		root, err := resolveRootRunIDScoped(r.Context(), db, id)
		if err != nil {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		// Fetch all runs in this conversation.
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, status, created_at FROM runs
			WHERE root_run_id = $1
			ORDER BY created_at ASC, id ASC`, root)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var runs []map[string]interface{}
		for rows.Next() {
			var rid, status string
			var ts time.Time
			rows.Scan(&rid, &status, &ts)
			runs = append(runs, map[string]interface{}{
				"id": rid, "status": status, "created_at": ts,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"root_run_id": root,
			"runs":        runs,
		})
	})

	r.Post("/api/runs/{id}/agent/chat", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		// Conversation chat is keyed by root run — resolve so reruns continue the same thread.
		root, err := resolveRootRunIDScoped(r.Context(), db, id)
		if err != nil {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		id = root
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		req.Message = strings.TrimSpace(req.Message)
		if req.Message == "" {
			http.Error(w, "message required", http.StatusBadRequest)
			return
		}
		if anthClient == nil {
			http.Error(w, "ANTHROPIC_API_KEY not configured", http.StatusServiceUnavailable)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		userMsgID := uuid.New().String()
		if _, err := db.ExecContext(r.Context(),
			`INSERT INTO agent_messages (id, run_id, who, kind, content) VALUES ($1, $2, 'user', 'chat', $3)`,
			userMsgID, id, req.Message); err != nil {
			writeSSE(w, "error", map[string]string{"error": err.Error()})
			flusher.Flush()
			return
		}
		writeSSE(w, "user_saved", map[string]string{"id": userMsgID})
		flusher.Flush()

		cc, err := loadConversationContext(r.Context(), db, s3, bucket, id)
		if err != nil {
			writeSSE(w, "error", map[string]string{"error": "load conversation: " + err.Error()})
			flusher.Flush()
			return
		}
		history, err := loadChatHistory(r.Context(), db, id, workspaceFromContext(r.Context()))
		if err != nil {
			writeSSE(w, "error", map[string]string{"error": "load history: " + err.Error()})
			flusher.Flush()
			return
		}
		// Strip the message we just inserted so it doesn't get double-sent.
		// Find by ID — history now contains tool rows so the inserted user message
		// is not guaranteed to be the last entry.
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].ID == userMsgID {
				history = append(history[:i], history[i+1:]...)
				break
			}
		}

		latest := cc.LatestRun()
		if latest == nil {
			writeSSE(w, "error", map[string]string{"error": "conversation has no runs to triage"})
			flusher.Flush()
			return
		}
		apiMsgs := buildAPIMessages(cc, history, req.Message)
		deps := &toolDeps{
			DB:          db,
			S3:          s3,
			Bucket:      bucket,
			RunID:       latest.RunID,
			Repo:        latest.Repo,
			WorkspaceID: workspaceFromContext(r.Context()),
		}
		emit := &httpEmitter{w: w, flusher: flusher}

		if _, err := runAgentLoop(r.Context(), db, emit, anthClient, model,
			apiMsgs, defs, tools, toolByName, deps, id, ""); err != nil {
			writeSSE(w, "error", map[string]string{"error": err.Error()})
			flusher.Flush()
		}
	})

	// GET /api/runs/{id}/agent/live — SSE stream for background auto-triage events.
	// Clients subscribe while a run is selected; events are forwarded from agentBus
	// until the client disconnects or a triage finishes (the "done" sentinel). If no
	// auto-triage is active, the stream stays open with nothing to relay.
	r.Get("/api/runs/{id}/agent/live", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		root, err := resolveRootRunIDScoped(r.Context(), db, id)
		if err != nil {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		ch := agentBus.subscribe(root)
		defer agentBus.unsubscribe(root, ch)

		// Relay bus events until the client goes away or triage signals done.
		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				w.Write(msg) //nolint:errcheck
				flusher.Flush()
				// Propagate the "done" sentinel so the client knows triage finished.
				if strings.Contains(string(msg), "event: done") {
					return
				}
			}
		}
	})

}
