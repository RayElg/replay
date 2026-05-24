-- Per-environment list of env_var keys whose values are secret. Every value is
-- already encrypted at rest (replaycrypto under REPLAY_ENCRYPT_KEY); secret_keys
-- additionally marks which ones must never be returned to the browser. The API
-- masks them on read and preserves the stored ciphertext on write when the mask
-- is sent back unchanged. The runner reads straight from the DB and decrypts, so
-- masking the API response doesn't affect test execution.
--
-- This replaces the old client-side heuristic that guessed secrecy from key
-- substrings ("token"/"key"/…) with an explicit, user-controlled designation.

-- +goose Up

ALTER TABLE public.environments
    ADD COLUMN secret_keys text[] NOT NULL DEFAULT '{}';

-- +goose Down

ALTER TABLE public.environments
    DROP COLUMN IF EXISTS secret_keys;
