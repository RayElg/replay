-- Switches projects.webhook_token from plaintext storage to a SHA-256 hash
-- (matches the api_keys pattern). The plaintext only ever exists for the
-- moment of generation: rotate returns it once, then it's unrecoverable.
-- Display in the UI uses the prefix.

-- +goose Up

ALTER TABLE public.projects
    ADD COLUMN webhook_token_hash text,
    ADD COLUMN webhook_token_prefix text;

-- No backfill: the baseline ships with NULL webhook_token on the seed project,
-- and the only writer was the legacy GET endpoint which auto-minted on first
-- read. Pre-baseline deployments don't exist — this migration ships with the
-- first cut. If somehow a deployment has plaintext tokens, the columns stay
-- NULL and the GET endpoint will route through the rotate flow on next read.

DROP INDEX IF EXISTS public.idx_projects_webhook_token;

ALTER TABLE public.projects
    DROP COLUMN webhook_token;

CREATE UNIQUE INDEX idx_projects_webhook_token_hash
    ON public.projects USING btree (webhook_token_hash)
    WHERE webhook_token_hash IS NOT NULL;

-- +goose Down

ALTER TABLE public.projects
    ADD COLUMN webhook_token text;
DROP INDEX IF EXISTS public.idx_projects_webhook_token_hash;
ALTER TABLE public.projects
    DROP COLUMN IF EXISTS webhook_token_hash,
    DROP COLUMN IF EXISTS webhook_token_prefix;
CREATE UNIQUE INDEX idx_projects_webhook_token
    ON public.projects USING btree (webhook_token)
    WHERE webhook_token IS NOT NULL;
