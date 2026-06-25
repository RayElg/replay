package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RayElg/replay/internal/replaycrypto"
	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// resolveKeyring is the runner-side equivalent of secrets.go in the control
// plane: built once at startup from REPLAY_ENCRYPT_KEY, plus
// REPLAY_ENCRYPT_KEY_PREVIOUS so the runner can still decrypt values written
// under a retired key during rotation. Lazy: env vars are only decrypted at job
// time, and we don't want to fail the runner boot just because the operator
// hasn't enabled at-rest encryption yet.
var (
	encryptKeyOnce sync.Once
	keyring        *replaycrypto.Keyring
)

func resolveKeyring() *replaycrypto.Keyring {
	encryptKeyOnce.Do(func() {
		raw := os.Getenv("REPLAY_ENCRYPT_KEY")
		if raw == "" {
			return
		}
		var previous []string
		for _, p := range strings.FieldsFunc(os.Getenv("REPLAY_ENCRYPT_KEY_PREVIOUS"), func(r rune) bool {
			return r == ',' || r == '\n' || r == '\t' || r == ' '
		}) {
			previous = append(previous, p)
		}
		kr, err := replaycrypto.NewKeyring(raw, previous...)
		if err != nil {
			slog.Warn("encrypt keyring build failed; env_var decrypt will fail", "error", err)
			return
		}
		keyring = kr
	})
	return keyring
}

// decryptEnvVarValue decrypts a stored env-var value. Plaintext values (stored
// when no encryption key is configured, since at-rest encryption is opt-in) are
// returned unchanged so the runner doesn't break on mixed-state databases.
func decryptEnvVarValue(v string) (string, error) {
	if v == "" || !replaycrypto.IsEncrypted(v) {
		return v, nil
	}
	kr := resolveKeyring()
	if kr == nil {
		return "", fmt.Errorf("env var is encrypted but REPLAY_ENCRYPT_KEY is not configured on the runner")
	}
	return kr.Decrypt(v)
}

// defaultWorkspaceID matches the seed row inserted by migration 0001. Cross-module
// shared constant — keep in sync with cmd/control-plane/auth.go.
const defaultWorkspaceID = "00000000-0000-0000-0000-000000000001"

// normalizeNullable converts pgmqtt's stringified SQL-NULL ("null" / "NULL")
// back to an empty string so callers can use the standard "" sentinel.
func normalizeNullable(s string) string {
	if s == "null" || s == "NULL" {
		return ""
	}
	return s
}

// titleFor produces a stable display title for a synthetic TestResult when the
// runner needs to invent one (script fetch failed, no JSON report, etc.).
// Prefers the explicit test_filter, falls back to a short prefix of an ID —
// guarded so a short or empty ID doesn't panic the slice.
func titleFor(job JobPayload, fallbackID string) string {
	if job.TestFilter != "" {
		return job.TestFilter
	}
	return shortID(fallbackID, 8)
}

func shortID(s string, n int) string {
	if s == "" {
		return "(unknown)"
	}
	if len(s) <= n {
		return s
	}
	return s[:n]
}

type JobPayload struct {
	RunID      string `json:"run_id"`
	ProjectID  string `json:"project_id"`
	Branch     string `json:"branch"`
	CommitSHA  string `json:"commit_sha"`
	Status     string `json:"status"`
	TestFilter string `json:"test_filter"`
	ScriptID   string `json:"script_id"`
	EnvID      string `json:"env_id"`
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	runnerID := uuid.New().String()
	slog.Info("runner starting", "runner_id", runnerID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- Database connection ---
	dbURL := os.Getenv("RUNNER_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://replay:replay@localhost:5432/postgres?sslmode=disable"
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// --- MinIO Client ---
	storeURL := os.Getenv("RUNNER_OBJECT_STORE") // s3://minio:9000/replay-artifacts
	s3Client, bucket, err := initS3(storeURL)
	if err != nil {
		slog.Error("failed to init S3", "error", err)
		os.Exit(1)
	}

	// --- MQTT Client ---
	mqttURLStr := os.Getenv("RUNNER_MQTT_BROKER_URL")
	if mqttURLStr == "" {
		mqttURLStr = "tcp://localhost:1883"
	}
	mqttURL, err := url.Parse(mqttURLStr)
	if err != nil {
		slog.Error("failed to parse MQTT URL", "error", err)
		os.Exit(1)
	}

	// If RUNNER_ENV_ID is set this runner only handles jobs for that environment.
	// Cross-env runners (no RUNNER_ENV_ID) subscribe to all environment topics.
	runnerEnvID := os.Getenv("RUNNER_ENV_ID")
	var mqttTopic string
	if runnerEnvID != "" {
		mqttTopic = "$share/replay-runners/runs/+/" + runnerEnvID + "/queue"
		slog.Info("runner bound to environment", "env_id", runnerEnvID)
	} else {
		mqttTopic = "$share/replay-runners/runs/+/+/queue"
		slog.Info("runner is cross-env")
	}

	// Bound the number of concurrent Playwright invocations. Each one launches a
	// headless browser (~500MB resident), so on a small machine 2–4 is plenty.
	// Above the cap, OnPublishReceived blocks before acking — MQTT redelivers the
	// message to another runner.
	concurrency := 2
	if v := os.Getenv("RUNNER_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			concurrency = n
		}
	}
	jobSlots := make(chan struct{}, concurrency)
	var activeJobs atomic.Int32
	slog.Info("runner ready", "concurrency", concurrency)

	// Broker auth: pgmqtt:auth requires every CONNECT to present a username
	// and password. The runner uses a dedicated `replay_runner` LOGIN role
	// provisioned by control-plane (cmd/control-plane/mqtt_auth.go); both
	// sides read the same secret from REPLAY_RUNNER_MQTT_PASSWORD (here
	// surfaced as RUNNER_MQTT_PASSWORD to keep runner env naming consistent).
	mqttUser := os.Getenv("RUNNER_MQTT_USERNAME")
	if mqttUser == "" {
		mqttUser = "replay_runner"
	}
	mqttPass := os.Getenv("RUNNER_MQTT_PASSWORD")

	cliCfg := autopaho.ClientConfig{
		ServerUrls:      []*url.URL{mqttURL},
		KeepAlive:       20,
		ConnectUsername: mqttUser,
		ConnectPassword: []byte(mqttPass),
		OnConnectionUp: func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
			slog.Info("mqtt connection up")
			if _, err := cm.Subscribe(ctx, &paho.Subscribe{
				Subscriptions: []paho.SubscribeOptions{
					{Topic: mqttTopic, QoS: 1},
				},
			}); err != nil {
				slog.Error("failed to subscribe", "error", err)
			}
		},
		ClientConfig: paho.ClientConfig{
			ClientID: "runner-" + runnerID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					var job JobPayload
					if err := json.Unmarshal(pr.Packet.Payload, &job); err != nil {
						slog.Error("failed to unmarshal job", "error", err)
						return true, nil
					}
					if job.Status != "queued" {
						return true, nil
					}
					// Block until a slot is available; MQTT redelivery handles backpressure.
					select {
					case jobSlots <- struct{}{}:
					case <-ctx.Done():
						return true, nil
					}
					activeJobs.Add(1)
					go func() {
						defer func() {
							activeJobs.Add(-1)
							<-jobSlots
						}()
						executeJob(ctx, db, s3Client, bucket, job, runnerID)
					}()
					return true, nil
				},
			},
		},
	}

	cm, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		slog.Error("failed to connect to mqtt", "error", err)
		os.Exit(1)
	}

	// Heartbeat loop. Workspace ID must match the seeded workspace in the control plane's
	// migration 0001 (also referenced as defaultWorkspaceID in cmd/control-plane/auth.go).
	heartbeatWorkspace := os.Getenv("RUNNER_WORKSPACE_ID")
	if heartbeatWorkspace == "" {
		heartbeatWorkspace = defaultWorkspaceID
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ticker.C:
				topic := "runners/" + heartbeatWorkspace + "/" + runnerID + "/heartbeat"
				active := activeJobs.Load()
				status := "idle"
				if active > 0 {
					status = "busy"
				}
				payload, _ := json.Marshal(map[string]any{
					"status":      status,
					"active_jobs": active,
					"capacity":    concurrency,
					"timestamp":   time.Now().Format(time.RFC3339),
				})
				cm.Publish(ctx, &paho.Publish{Topic: topic, Payload: payload})
			case <-ctx.Done():
				return
			}
		}
	}()

	<-ctx.Done()
	slog.Info("runner shutting down")
}

func initS3(storeURL string) (*minio.Client, string, error) {
	u, err := url.Parse(storeURL)
	if err != nil {
		return nil, "", err
	}
	endpoint := u.Host
	bucket := strings.TrimPrefix(u.Path, "/")

	accessKey := os.Getenv("MINIO_ROOT_USER")
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	secretKey := os.Getenv("MINIO_ROOT_PASSWORD")
	if secretKey == "" {
		secretKey = "minioadmin"
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: u.Scheme == "https",
		Region: "us-east-1",
	})
	return client, bucket, err
}

func executeJob(ctx context.Context, db *sql.DB, s3 *minio.Client, bucket string, job JobPayload, runnerID string) {
	slog.Info("executing job", "run_id", job.RunID, "script_id", job.ScriptID)

	// Only transition queued → running (skip if cancelled before pickup). Capture
	// workspace_id here to scope the script/env lookups below to the run's tenant.
	var workspaceID string
	err := db.QueryRowContext(ctx,
		`UPDATE runs SET status = 'running', started_at = NOW()
		   WHERE id = $1 AND status = 'queued'
		 RETURNING workspace_id`,
		job.RunID).Scan(&workspaceID)
	if err == sql.ErrNoRows {
		slog.Info("skipping run; no longer queued (likely cancelled)", "run_id", job.RunID)
		return
	}
	if err != nil {
		slog.Error("failed to update status", "run_id", job.RunID, "error", err)
		return
	}

	start := time.Now()

	// pgmqtt renders SQL NULL through its jinja2 templates as either "null" or
	// "NULL" depending on broker version — normalise both to empty string so
	// downstream UUID-typed queries don't choke.
	scriptID := normalizeNullable(job.ScriptID)
	envID := normalizeNullable(job.EnvID)

	// Build env vars: environment-level base, then per-run overrides (run wins on conflict).
	merged := map[string]string{}
	var envSlug string
	var decryptFailures []string

	if envID != "" {
		var varsJSON []byte
		if err = db.QueryRowContext(ctx,
			`SELECT env_vars, slug FROM environments WHERE id = $1 AND workspace_id = $2`, envID, workspaceID,
		).Scan(&varsJSON, &envSlug); err == nil {
			var vars map[string]string
			if json.Unmarshal(varsJSON, &vars) == nil {
				for k, v := range vars {
					plain, derr := decryptEnvVarValue(v)
					if derr != nil {
						decryptFailures = append(decryptFailures, k)
						continue
					}
					merged[k] = plain
				}
			}
		} else {
			slog.Warn("failed to fetch env vars", "env_id", envID, "error", err)
		}
	}

	// Per-run overrides from runs.env_vars (set by webhook callers, e.g. GitHub Actions).
	var runVarsJSON []byte
	if err = db.QueryRowContext(ctx,
		`SELECT COALESCE(env_vars, '{}') FROM runs WHERE id = $1`, job.RunID,
	).Scan(&runVarsJSON); err == nil {
		var runVars map[string]string
		if json.Unmarshal(runVarsJSON, &runVars) == nil {
			for k, v := range runVars {
				plain, derr := decryptEnvVarValue(v)
				if derr != nil {
					decryptFailures = append(decryptFailures, k)
					continue
				}
				merged[k] = plain
			}
		}
	} else {
		slog.Warn("failed to fetch run env_vars", "run_id", job.RunID, "error", err)
	}

	// Undecryptable values are dropped, not passed through as ciphertext. Surface
	// it loudly — it's almost always a key mismatch with the control-plane.
	if len(decryptFailures) > 0 {
		slog.Error("runner could not decrypt env var(s); they were dropped — set REPLAY_ENCRYPT_KEY to match the control-plane (and REPLAY_ENCRYPT_KEY_PREVIOUS during rotation)",
			"run_id", job.RunID, "count", len(decryptFailures), "vars", decryptFailures)
	}

	var envVars []string
	for k, v := range merged {
		envVars = append(envVars, k+"="+v)
	}

	// Always inject Replay-provided metadata so scripts can identify their context
	envVars = append(envVars,
		"REPLAY_RUN_ID="+job.RunID,
		"REPLAY_BRANCH="+job.Branch,
		"REPLAY_COMMIT="+job.CommitSHA,
		"REPLAY_ENV="+envSlug,
	)

	// ── Execute ──────────────────────────────────────────────────────────────
	var testResults []TestResult
	success := false

	if scriptID != "" {
		var content string
		err = db.QueryRowContext(ctx, `SELECT content FROM scripts WHERE id = $1 AND workspace_id = $2`, scriptID, workspaceID).Scan(&content)
		if err != nil {
			testResults = []TestResult{{Title: titleFor(job, scriptID), Status: "failed",
				Logs: fmt.Sprintf("failed to fetch script %s: %v", scriptID, err)}}
		} else {
			var combinedLogs string
			testResults, combinedLogs, success, err = runScriptTest(ctx, content, job.RunID, envVars)
			if err != nil {
				combinedLogs += fmt.Sprintf("\nInternal error: %v", err)
				success = false
			}
			if len(testResults) == 0 {
				testResults = []TestResult{{Title: titleFor(job, scriptID), Status: "failed", Logs: combinedLogs}}
			}
		}
	} else {
		// All Replay runs must have a script_id — this path should never be reached
		// in normal operation. Fail fast with a clear message.
		testResults = []TestResult{{
			Title: "no-script run", Status: "failed",
			Logs: "this run has no script_id — all Replay runs require a script",
		}}
	}

	elapsed := time.Since(start)
	runStatus := "passed"
	if !success {
		runStatus = "failed"
	}

	// Guard against cancellations that arrived during execution: only finalise if
	// we're still in 'running' state (the cancel endpoint flips status to 'cancelled').
	finRes, err := db.ExecContext(ctx,
		`UPDATE runs SET status = $1, finished_at = NOW() WHERE id = $2 AND status = 'running'`,
		runStatus, job.RunID)
	if err != nil {
		slog.Error("db error status", "error", err)
	} else if n, _ := finRes.RowsAffected(); n == 0 {
		slog.Info("run was cancelled during execution; discarding result", "run_id", job.RunID)
		return
	}

	// ── Persist per-test results and upload artifacts ─────────────────────────
	for i, tr := range testResults {
		resultID := uuid.New().String()
		durMS := tr.DurationMS
		if durMS == 0 && len(testResults) == 1 {
			durMS = elapsed.Milliseconds()
		}
		_, err = db.ExecContext(ctx,
			`INSERT INTO run_results (id, run_id, test_name, status, duration_ms, logs) VALUES ($1, $2, $3, $4, $5, $6)`,
			resultID, job.RunID, tr.Title, tr.Status, durMS, tr.Logs)
		if err != nil {
			slog.Error("db error result", "error", err)
		}

		if tr.Screenshot != nil {
			uploadArtifact(ctx, db, s3, bucket, job.RunID, resultID, "screenshot", "screenshot.png", "image/png", tr.Screenshot)
		}
		if tr.VideoPath != "" {
			if videoData, err := os.ReadFile(tr.VideoPath); err == nil {
				uploadArtifact(ctx, db, s3, bucket, job.RunID, resultID, "video", "recording.webm", "video/webm", videoData)
				frames, ferr := extractKeyframes(ctx, tr.VideoPath, 3)
				if ferr != nil {
					slog.Warn("frame extraction failed", "run_id", job.RunID, "test_idx", i, "error", ferr)
				}
				for j, frame := range frames {
					uploadArtifact(ctx, db, s3, bucket, job.RunID, resultID, "video_frame", fmt.Sprintf("frame_%02d.jpg", j), "image/jpeg", frame)
				}
				os.Remove(tr.VideoPath)
			} else {
				slog.Error("failed to read video file", "path", tr.VideoPath, "error", err)
			}
		}
		if tr.TracePath != "" {
			if traceData, err := os.ReadFile(tr.TracePath); err == nil {
				uploadArtifact(ctx, db, s3, bucket, job.RunID, resultID, "trace", "trace.zip", "application/zip", traceData)
				if sum, perr := parseTraceZip(tr.TracePath); perr == nil {
					if jsonBytes, jerr := json.MarshalIndent(sum, "", "  "); jerr == nil {
						uploadArtifact(ctx, db, s3, bucket, job.RunID, resultID, "trace_summary", "trace_summary.json", "application/json", jsonBytes)
					}
					insertSteps(ctx, db, resultID, sum.Steps)
				} else {
					slog.Warn("trace parse failed", "run_id", job.RunID, "test_idx", i, "error", perr)
				}
				os.Remove(tr.TracePath)
			} else {
				slog.Error("failed to read trace file", "path", tr.TracePath, "error", err)
			}
		}
	}

	slog.Info("job finished", "run_id", job.RunID, "status", runStatus, "tests", len(testResults))
}

// baseRunnerEnv returns the minimal host env Playwright/npx needs, excluding the
// runner's secrets. Allowlist (not denylist) so a future secret can't leak by
// default. PLAYWRIGHT_* keeps the base image's browser-path vars; PATH+HOME let
// npx find node and the pre-installed browsers.
func baseRunnerEnv() []string {
	allowExact := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
		"LANG": true, "TZ": true, "TERM": true, "TMPDIR": true,
		"HOSTNAME": true, "PWD": true, "XDG_CACHE_HOME": true, "XDG_CONFIG_HOME": true,
	}
	allowPrefix := []string{"LC_", "PLAYWRIGHT_", "NODE_", "NPM_", "npm_"}
	var out []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if allowExact[name] {
			out = append(out, kv)
			continue
		}
		for _, p := range allowPrefix {
			if strings.HasPrefix(name, p) {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

// runScriptTest writes the user script to a temp file inside /playwright-project/tests/,
// runs it via `npx playwright test --reporter=json`, parses per-test results from the
// JSON report, and returns one TestResult per test spec.
// Falls back to a single synthetic TestResult if the JSON report is missing or unparseable.
// envVars is a list of "KEY=value" strings injected into the test process environment.
func runScriptTest(ctx context.Context, scriptContent, runID string, envVars []string) ([]TestResult, string, bool, error) {
	projectDir := "/playwright-project"
	outputDir := fmt.Sprintf("/tmp/playwright-out-%s", runID)
	os.MkdirAll(outputDir, 0755)
	defer os.RemoveAll(outputDir)

	testFile := filepath.Join(projectDir, "tests", runID+".spec.ts")
	if err := os.WriteFile(testFile, []byte(scriptContent), 0644); err != nil {
		return nil, "", false, fmt.Errorf("write script: %w", err)
	}
	defer os.Remove(testFile)

	jsonResultPath := fmt.Sprintf("/tmp/pw-results-%s.json", runID)
	defer os.Remove(jsonResultPath)

	cmd := exec.CommandContext(ctx, "npx", "playwright", "test", testFile,
		"--output", outputDir, "--reporter=json")
	cmd.Dir = projectDir
	cmd.Env = append(baseRunnerEnv(), envVars...)
	cmd.Env = append(cmd.Env, "PLAYWRIGHT_JSON_OUTPUT_NAME="+jsonResultPath)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	success := err == nil
	logs := buf.String()

	results, parseErr := parsePlaywrightResults(jsonResultPath, runID)
	if parseErr != nil || len(results) == 0 {
		if parseErr != nil {
			slog.Warn("playwright json parse failed, using synthetic result", "run_id", runID, "error", parseErr)
		}
		status := "passed"
		if !success {
			status = "failed"
		}
		tr := TestResult{Title: runID[:8], Status: status, Logs: logs}

		if pngs, _ := filepath.Glob(filepath.Join(outputDir, "**", "*.png")); len(pngs) > 0 {
			tr.Screenshot, _ = os.ReadFile(pngs[0])
		}
		if tr.Screenshot == nil {
			if pngs, _ := filepath.Glob(filepath.Join(outputDir, "*.png")); len(pngs) > 0 {
				tr.Screenshot, _ = os.ReadFile(pngs[0])
			}
		}
		if webms, _ := filepath.Glob(filepath.Join(outputDir, "**", "*.webm")); len(webms) > 0 {
			stablePath := fmt.Sprintf("/tmp/video-%s-0.webm", runID)
			if data, err := os.ReadFile(webms[0]); err == nil {
				if os.WriteFile(stablePath, data, 0644) == nil {
					tr.VideoPath = stablePath
				}
			}
		}
		if zips, _ := filepath.Glob(filepath.Join(outputDir, "**", "trace.zip")); len(zips) > 0 {
			stablePath := fmt.Sprintf("/tmp/trace-%s-0.zip", runID)
			if data, err := os.ReadFile(zips[0]); err == nil {
				if os.WriteFile(stablePath, data, 0644) == nil {
					tr.TracePath = stablePath
				}
			}
		}
		return []TestResult{tr}, logs, success, nil
	}

	return results, logs, success, nil
}

// extractKeyframes pulls `count` evenly-spaced frames from a video, returning them as JPEGs.
// The final frame (state-at-end / state-at-failure) is always included as the last entry.
// Uses ffmpeg's select filter so we don't need to know the video duration up-front.
func extractKeyframes(ctx context.Context, videoPath string, count int) ([][]byte, error) {
	if count < 1 {
		count = 1
	}
	outDir, err := os.MkdirTemp("", "frames-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir tmp: %w", err)
	}
	defer os.RemoveAll(outDir)

	// Probe duration first so we can pick explicit timestamps. ffprobe returns seconds as a float.
	// If probing fails (corrupt header, ffprobe missing) we fall through with a
	// conservative default — better to grab one frame than to give up entirely.
	probe := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries",
		"format=duration", "-of", "default=noprint_wrappers=1:nokey=1", videoPath)
	var probeOut bytes.Buffer
	probe.Stdout = &probeOut
	var duration float64
	if err := probe.Run(); err != nil {
		slog.Warn("ffprobe failed; falling back to single-frame extraction", "video", videoPath, "error", err)
		duration = 0
	} else {
		fmt.Sscanf(strings.TrimSpace(probeOut.String()), "%f", &duration)
	}
	if duration <= 0 {
		// Try to grab whatever frame ffmpeg can find without a known duration.
		duration = 0
		count = 1
	}

	// Pick timestamps: spread the first count-1 evenly through [0, duration), then add the very last frame.
	// For a 10s video with count=3 we get t=0, t=3.33, t=~9.95.
	// When duration is unknown (probe failed) we just grab t=0.
	timestamps := make([]float64, 0, count)
	if duration <= 0 {
		timestamps = append(timestamps, 0)
	} else {
		last := duration - 0.05
		if last < 0 {
			last = 0
		}
		if count == 1 {
			timestamps = append(timestamps, last)
		} else {
			for i := 0; i < count-1; i++ {
				timestamps = append(timestamps, float64(i)*duration/float64(count-1))
			}
			timestamps = append(timestamps, last)
		}
	}

	frames := make([][]byte, 0, count)
	for i, ts := range timestamps {
		out := filepath.Join(outDir, fmt.Sprintf("f%02d.jpg", i))
		cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-ss", fmt.Sprintf("%.3f", ts),
			"-i", videoPath, "-frames:v", "1", "-q:v", "5", out)
		if err := cmd.Run(); err != nil {
			slog.Warn("ffmpeg frame extract failed", "ts", ts, "error", err)
			continue
		}
		if data, err := os.ReadFile(out); err == nil {
			frames = append(frames, data)
		}
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("no frames extracted")
	}
	return frames, nil
}

func insertSteps(ctx context.Context, db *sql.DB, resultID string, steps []TraceStep) {
	for _, s := range steps {
		_, err := db.ExecContext(ctx, `
			INSERT INTO steps (run_result_id, idx, api_name, selector, url, status, start_ms, duration_ms, error)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			resultID, s.Idx, s.APIName, s.Selector, s.URL, s.Status,
			int(s.StartS*1000), s.DurationMS, s.Error)
		if err != nil {
			slog.Warn("insert step failed", "result_id", resultID, "idx", s.Idx, "error", err)
		}
	}
}

func uploadArtifact(ctx context.Context, db *sql.DB, s3 *minio.Client, bucket, runID, resultID, kind, filename, contentType string, data []byte) {
	objKey := fmt.Sprintf("runs/%s/results/%s/%s", runID, resultID, filename)
	_, err := s3.PutObject(ctx, bucket, objKey, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		slog.Error("failed to upload artifact", "run_id", runID, "kind", kind, "error", err)
		return
	}
	slog.Info("uploaded artifact", "run_id", runID, "kind", kind, "key", objKey)
	_, err = db.ExecContext(ctx,
		`INSERT INTO artifacts (id, run_result_id, kind, storage_key, size_bytes) VALUES ($1, $2, $3, $4, $5)`,
		uuid.New().String(), resultID, kind, objKey, len(data))
	if err != nil {
		slog.Error("db error artifact", "error", err)
	}
}
