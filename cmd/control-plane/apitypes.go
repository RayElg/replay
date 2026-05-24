package main

import "time"

// API response types — one canonical definition for every shape the frontend depends on.
// JSON field names must stay in sync with ui/lib/types.ts.

type RunListItem struct {
	ID               string     `json:"id"`
	ProjectID        string     `json:"project_id"`
	Branch           string     `json:"branch"`
	CommitSHA        string     `json:"commit_sha"`
	Repo             *string    `json:"repo"`
	Status           string     `json:"status"`
	AutoTriaged      bool       `json:"auto_triaged"`
	HasAgentActivity bool       `json:"has_agent_activity"`
	TestFilter       string     `json:"test_filter"`
	ScriptID         *string    `json:"script_id"`
	ScriptName       *string    `json:"script_name"`
	EnvID            *string    `json:"env_id"`
	EnvName          *string    `json:"env_name"`
	EnvSlug          *string    `json:"env_slug"`
	StartedAt        *time.Time `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at"`
	CreatedAt        time.Time  `json:"created_at"`
	RootRunID        string     `json:"root_run_id"`
	WebhookSource    *string    `json:"webhook_source"`

	// Triage verdict — populated once the agent has triaged the run via the
	// submit_triage_verdict tool. Null on un-triaged runs.
	TriageClassification *string    `json:"triage_classification"`
	TriageConfidence     *string    `json:"triage_confidence"`
	TriageSummary        *string    `json:"triage_summary"`
	TriagedAt            *time.Time `json:"triaged_at"`
}

type StepResponse struct {
	Idx        int    `json:"idx"`
	APIName    string `json:"api_name"`
	Selector   string `json:"selector"`
	URL        string `json:"url"`
	Status     string `json:"status"`
	StartMS    int    `json:"start_ms"`
	DurationMS int    `json:"duration_ms"`
	Error      string `json:"error"`
}

type RunResultResponse struct {
	ID         string         `json:"id"`
	TestName   string         `json:"test_name"`
	Status     string         `json:"status"`
	DurationMS int            `json:"duration_ms"`
	Logs       string         `json:"logs"`
	Steps      []StepResponse `json:"steps"`
}

type ArtifactResponse struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	StorageKey string `json:"storage_key"`
	SizeBytes  int64  `json:"size_bytes"`
	URL        string `json:"url"`
}

// RunDetailResponse embeds RunListItem so the detail endpoint is a strict superset of the list shape.
type RunDetailResponse struct {
	RunListItem
	Results   []RunResultResponse `json:"results"`
	Artifacts []ArtifactResponse  `json:"artifacts"`
}

type ScriptResponse struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	Filename  string    `json:"filename"`
	Content   string    `json:"content"`
	AgentsMD  string    `json:"agents_md"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Source linkage. Empty/unset for inline scripts (the default).
	SourceKind          string     `json:"source_kind,omitempty"`
	SourceIntegrationID string     `json:"source_integration_id,omitempty"`
	SourceRepo          string     `json:"source_repo,omitempty"`
	SourcePath          string     `json:"source_path,omitempty"`
	SourceRef           string     `json:"source_ref,omitempty"`
	SourceSHA           string     `json:"source_sha,omitempty"`
	SyncedAt            *time.Time `json:"synced_at,omitempty"`
}

type EnvironmentResponse struct {
	ID        string            `json:"id"`
	ProjectID string            `json:"project_id"`
	Name      string            `json:"name"`
	Slug      string            `json:"slug"`
	EnvVars   map[string]string `json:"env_vars"`
	// SecretKeys lists the env_var keys whose values are secret. Their values in
	// EnvVars are masked (secretMask) on read — the real plaintext is never sent
	// to the browser.
	SecretKeys []string  `json:"secret_keys"`
	CreatedAt  time.Time `json:"created_at"`
}
