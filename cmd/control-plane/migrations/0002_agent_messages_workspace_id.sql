-- Adds workspace_id to agent_messages as a defense-in-depth tenant isolation
-- guard. Every other tenant table already carries workspace_id; agent_messages
-- relied on its run_id FK pointing into runs (which is workspace-scoped). All
-- HTTP read paths today go through resolveRootRunIDScoped first, so this is
-- not a known exploit — but if a future caller forgets the scoped resolver,
-- the agent conversation would leak cross-tenant. Closing the gap.

-- +goose Up

ALTER TABLE public.agent_messages
    ADD COLUMN workspace_id uuid;

UPDATE public.agent_messages am
SET workspace_id = r.workspace_id
FROM public.runs r
WHERE am.run_id = r.id;

ALTER TABLE public.agent_messages
    ALTER COLUMN workspace_id SET NOT NULL,
    ADD CONSTRAINT agent_messages_workspace_id_fkey
        FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;

CREATE INDEX idx_agent_messages_workspace_run
    ON public.agent_messages USING btree (workspace_id, run_id, created_at);

-- Trigger fills workspace_id from runs on insert when omitted — same pattern
-- as set_run_result_workspace_id. Lets existing INSERTs keep working without
-- threading workspace_id through every call site.
-- +goose StatementBegin
CREATE FUNCTION public.set_agent_message_workspace_id() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.workspace_id IS NULL THEN
    SELECT workspace_id INTO NEW.workspace_id FROM runs WHERE id = NEW.run_id;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER trg_agent_messages_set_workspace
    BEFORE INSERT ON public.agent_messages
    FOR EACH ROW EXECUTE FUNCTION public.set_agent_message_workspace_id();

-- +goose Down

DROP TRIGGER IF EXISTS trg_agent_messages_set_workspace ON public.agent_messages;
DROP FUNCTION IF EXISTS public.set_agent_message_workspace_id();
DROP INDEX IF EXISTS public.idx_agent_messages_workspace_run;
ALTER TABLE public.agent_messages
    DROP CONSTRAINT IF EXISTS agent_messages_workspace_id_fkey,
    DROP COLUMN IF EXISTS workspace_id;
