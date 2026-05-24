-- Structured triage verdict on runs. The auto-triage agent (and interactive
-- chat) emits a machine-readable classification via the submit_triage_verdict
-- tool so a failure's nature is visible at a glance and queryable, rather than
-- buried in free-form chat. All columns are nullable: a run carries a verdict
-- only once the agent has triaged it.

-- +goose Up

ALTER TABLE public.runs
    ADD COLUMN triage_classification text,
    ADD COLUMN triage_confidence text,
    ADD COLUMN triage_summary text,
    ADD COLUMN triaged_at timestamp with time zone;

-- +goose Down

ALTER TABLE public.runs
    DROP COLUMN IF EXISTS triage_classification,
    DROP COLUMN IF EXISTS triage_confidence,
    DROP COLUMN IF EXISTS triage_summary,
    DROP COLUMN IF EXISTS triaged_at;
