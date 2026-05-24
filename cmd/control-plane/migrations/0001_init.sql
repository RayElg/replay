-- Consolidated baseline schema for Replay.
-- Generated from migrations 001-034 via pg_dump --schema-only.
-- Subsequent schema changes should go in new migration files (0002+).

-- +goose Up


-- Name: pgmqtt; Type: EXTENSION; Schema: -; Owner: -

CREATE EXTENSION IF NOT EXISTS pgmqtt WITH SCHEMA public;

-- Name: set_run_result_workspace_id(); Type: FUNCTION; Schema: public; Owner: -

-- +goose StatementBegin
CREATE FUNCTION public.set_run_result_workspace_id() RETURNS trigger
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

-- Name: set_run_root_run_id(); Type: FUNCTION; Schema: public; Owner: -

-- +goose StatementBegin
CREATE FUNCTION public.set_run_root_run_id() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.root_run_id IS NULL THEN
    NEW.root_run_id := NEW.id;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Name: agent_messages; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.agent_messages (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    run_id uuid NOT NULL,
    who text NOT NULL,
    kind text DEFAULT 'chat'::text NOT NULL,
    content text DEFAULT ''::text NOT NULL,
    model text,
    tokens_in integer,
    tokens_out integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    source character varying(64),
    CONSTRAINT agent_messages_kind_check CHECK ((kind = ANY (ARRAY['chat'::text, 'tool_call'::text, 'tool_result'::text]))),
    CONSTRAINT agent_messages_who_check CHECK ((who = ANY (ARRAY['user'::text, 'agent'::text, 'system'::text])))
);

-- Name: api_keys; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.api_keys (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    workspace_id uuid NOT NULL,
    name text NOT NULL,
    key_prefix text NOT NULL,
    key_hash text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_used_at timestamp with time zone,
    scopes text[] DEFAULT ARRAY['admin'::text] NOT NULL,
    expires_at timestamp with time zone
);

-- Name: artifacts; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.artifacts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    run_result_id uuid NOT NULL,
    kind text NOT NULL,
    storage_key text NOT NULL,
    size_bytes bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT artifacts_kind_check CHECK ((kind = ANY (ARRAY['video'::text, 'video_frame'::text, 'trace'::text, 'trace_summary'::text, 'screenshot'::text, 'log'::text])))
);

-- Name: audit_events; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.audit_events (
    id bigint NOT NULL,
    workspace_id uuid NOT NULL,
    actor_id text NOT NULL,
    actor_kind text NOT NULL,
    method text NOT NULL,
    path text NOT NULL,
    status integer NOT NULL,
    ip inet,
    user_agent text,
    request_id text,
    metadata jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

-- Name: audit_events_id_seq; Type: SEQUENCE; Schema: public; Owner: -

CREATE SEQUENCE public.audit_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

-- Name: audit_events_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -

ALTER SEQUENCE public.audit_events_id_seq OWNED BY public.audit_events.id;

-- Name: environments; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.environments (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    env_vars jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    workspace_id uuid NOT NULL
);

-- Name: integrations; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.integrations (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    project_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    provider text NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    encrypted_token text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    workspace_id uuid NOT NULL,
    CONSTRAINT integrations_provider_check CHECK ((provider = ANY (ARRAY['github'::text, 'gha'::text, 'buildkite'::text, 'circleci'::text, 'slack'::text, 'linear'::text])))
);

-- Name: mqtt_credentials; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.mqtt_credentials (
    workspace_id uuid NOT NULL,
    role_name text NOT NULL,
    username text NOT NULL,
    password text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    rotated_at timestamp with time zone DEFAULT now() NOT NULL
);

-- Name: projects; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.projects (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    workspace_id uuid NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    webhook_token text
);

-- Name: run_results; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.run_results (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    run_id uuid NOT NULL,
    test_name text NOT NULL,
    status text NOT NULL,
    duration_ms integer DEFAULT 0 NOT NULL,
    logs text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    workspace_id uuid NOT NULL,
    CONSTRAINT run_results_status_check CHECK ((status = ANY (ARRAY['passed'::text, 'failed'::text, 'skipped'::text, 'timedout'::text])))
);

-- Name: runs; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.runs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    project_id uuid NOT NULL,
    branch text DEFAULT ''::text NOT NULL,
    commit_sha text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'queued'::text NOT NULL,
    test_filter text DEFAULT ''::text NOT NULL,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    script_id uuid,
    env_id uuid,
    root_run_id uuid NOT NULL,
    env_vars jsonb DEFAULT '{}'::jsonb,
    webhook_source text,
    repo text,
    auto_triaged boolean DEFAULT false NOT NULL,
    workspace_id uuid NOT NULL,
    CONSTRAINT runs_status_check CHECK ((status = ANY (ARRAY['queued'::text, 'running'::text, 'passed'::text, 'failed'::text, 'cancelled'::text])))
);

-- Name: script_patches; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.script_patches (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    script_id uuid NOT NULL,
    proposed_by_run_id uuid,
    proposed_by text DEFAULT 'agent'::text NOT NULL,
    summary text NOT NULL,
    rationale text DEFAULT ''::text NOT NULL,
    old_content text NOT NULL,
    new_content text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    decided_by text,
    decided_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT script_patches_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'applied'::text, 'rejected'::text, 'stale'::text])))
);

-- Name: scripts; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.scripts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL,
    filename text NOT NULL,
    content text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    workspace_id uuid NOT NULL,
    agents_md text,
    source_kind text DEFAULT 'inline'::text NOT NULL,
    source_integration_id uuid,
    source_repo text,
    source_path text,
    source_ref text,
    source_sha text,
    synced_at timestamp with time zone
);

-- Name: steps; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.steps (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    run_result_id uuid NOT NULL,
    idx integer NOT NULL,
    api_name text NOT NULL,
    selector text DEFAULT ''::text NOT NULL,
    url text DEFAULT ''::text NOT NULL,
    status text NOT NULL,
    start_ms integer DEFAULT 0 NOT NULL,
    duration_ms integer DEFAULT 0 NOT NULL,
    error text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT steps_status_check CHECK ((status = ANY (ARRAY['passed'::text, 'failed'::text])))
);

-- Name: user_invites; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.user_invites (
    token text NOT NULL,
    workspace_id uuid NOT NULL,
    email text NOT NULL,
    invited_by uuid,
    expires_at timestamp with time zone NOT NULL,
    accepted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

-- Name: user_password_resets; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.user_password_resets (
    token text NOT NULL,
    user_id uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

-- Name: user_sessions; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.user_sessions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    user_agent text,
    ip inet
);

-- Name: users; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.users (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    workspace_id uuid NOT NULL,
    email text NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    password_hash text,
    last_login_at timestamp with time zone
);

-- Name: workspaces; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.workspaces (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);

-- Name: audit_events id; Type: DEFAULT; Schema: public; Owner: -

ALTER TABLE ONLY public.audit_events ALTER COLUMN id SET DEFAULT nextval('public.audit_events_id_seq'::regclass);

-- Name: agent_messages agent_messages_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.agent_messages
    ADD CONSTRAINT agent_messages_pkey PRIMARY KEY (id);

-- Name: api_keys api_keys_key_hash_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_key_hash_key UNIQUE (key_hash);

-- Name: api_keys api_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);

-- Name: artifacts artifacts_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.artifacts
    ADD CONSTRAINT artifacts_pkey PRIMARY KEY (id);

-- Name: audit_events audit_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_pkey PRIMARY KEY (id);

-- Name: environments environments_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.environments
    ADD CONSTRAINT environments_pkey PRIMARY KEY (id);

-- Name: environments environments_project_id_slug_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.environments
    ADD CONSTRAINT environments_project_id_slug_key UNIQUE (project_id, slug);

-- Name: integrations integrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.integrations
    ADD CONSTRAINT integrations_pkey PRIMARY KEY (id);

-- Name: integrations integrations_workspace_id_provider_name_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.integrations
    ADD CONSTRAINT integrations_workspace_id_provider_name_key UNIQUE (workspace_id, provider, name);

-- Name: mqtt_credentials mqtt_credentials_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.mqtt_credentials
    ADD CONSTRAINT mqtt_credentials_pkey PRIMARY KEY (workspace_id);

-- Name: mqtt_credentials mqtt_credentials_role_name_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.mqtt_credentials
    ADD CONSTRAINT mqtt_credentials_role_name_key UNIQUE (role_name);

-- Name: mqtt_credentials mqtt_credentials_username_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.mqtt_credentials
    ADD CONSTRAINT mqtt_credentials_username_key UNIQUE (username);

-- Name: projects projects_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_pkey PRIMARY KEY (id);

-- Name: run_results run_results_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.run_results
    ADD CONSTRAINT run_results_pkey PRIMARY KEY (id);

-- Name: runs runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_pkey PRIMARY KEY (id);

-- Name: script_patches script_patches_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.script_patches
    ADD CONSTRAINT script_patches_pkey PRIMARY KEY (id);

-- Name: scripts scripts_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.scripts
    ADD CONSTRAINT scripts_pkey PRIMARY KEY (id);

-- Name: steps steps_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.steps
    ADD CONSTRAINT steps_pkey PRIMARY KEY (id);

-- Name: user_invites user_invites_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.user_invites
    ADD CONSTRAINT user_invites_pkey PRIMARY KEY (token);

-- Name: user_password_resets user_password_resets_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.user_password_resets
    ADD CONSTRAINT user_password_resets_pkey PRIMARY KEY (token);

-- Name: user_sessions user_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.user_sessions
    ADD CONSTRAINT user_sessions_pkey PRIMARY KEY (id);

-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);

-- Name: users users_workspace_email_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_workspace_email_key UNIQUE (workspace_id, email);

-- Name: workspaces workspaces_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_pkey PRIMARY KEY (id);

-- Name: workspaces workspaces_slug_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_slug_key UNIQUE (slug);

-- Name: idx_agent_messages_run_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_agent_messages_run_id ON public.agent_messages USING btree (run_id, created_at);

-- Name: idx_api_keys_expires_at; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_api_keys_expires_at ON public.api_keys USING btree (expires_at) WHERE (expires_at IS NOT NULL);

-- Name: idx_artifacts_run_result_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_artifacts_run_result_id ON public.artifacts USING btree (run_result_id);

-- Name: idx_audit_actor_created; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_audit_actor_created ON public.audit_events USING btree (actor_id, created_at DESC);

-- Name: idx_audit_workspace_created; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_audit_workspace_created ON public.audit_events USING btree (workspace_id, created_at DESC);

-- Name: idx_environments_project_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_environments_project_id ON public.environments USING btree (project_id);

-- Name: idx_environments_workspace; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_environments_workspace ON public.environments USING btree (workspace_id);

-- Name: idx_integrations_project; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_integrations_project ON public.integrations USING btree (project_id);

-- Name: idx_integrations_workspace; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_integrations_workspace ON public.integrations USING btree (workspace_id);

-- Name: idx_projects_webhook_token; Type: INDEX; Schema: public; Owner: -

CREATE UNIQUE INDEX idx_projects_webhook_token ON public.projects USING btree (webhook_token);

-- Name: idx_run_results_run_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_run_results_run_id ON public.run_results USING btree (run_id);

-- Name: idx_run_results_workspace; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_run_results_workspace ON public.run_results USING btree (workspace_id);

-- Name: idx_runs_auto_triage; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_runs_auto_triage ON public.runs USING btree (auto_triaged, status, finished_at) WHERE ((status = 'failed'::text) AND (auto_triaged = false));

-- Name: idx_runs_branch_created; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_runs_branch_created ON public.runs USING btree (branch, created_at DESC);

-- Name: idx_runs_commit_sha; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_runs_commit_sha ON public.runs USING btree (commit_sha);

-- Name: idx_runs_created_at; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_runs_created_at ON public.runs USING btree (created_at DESC);

-- Name: idx_runs_project_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_runs_project_id ON public.runs USING btree (project_id);

-- Name: idx_runs_repo; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_runs_repo ON public.runs USING btree (repo) WHERE (repo IS NOT NULL);

-- Name: idx_runs_root_run_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_runs_root_run_id ON public.runs USING btree (root_run_id, created_at);

-- Name: idx_runs_status; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_runs_status ON public.runs USING btree (status);

-- Name: idx_runs_workspace; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_runs_workspace ON public.runs USING btree (workspace_id, created_at DESC);

-- Name: idx_script_patches_script_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_script_patches_script_id ON public.script_patches USING btree (script_id, created_at DESC);

-- Name: idx_script_patches_status; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_script_patches_status ON public.script_patches USING btree (status, created_at DESC);

-- Name: idx_scripts_project_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_scripts_project_id ON public.scripts USING btree (project_id);

-- Name: idx_scripts_source_lookup; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_scripts_source_lookup ON public.scripts USING btree (source_integration_id, source_path) WHERE (source_kind = 'github'::text);

-- Name: idx_scripts_workspace; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_scripts_workspace ON public.scripts USING btree (workspace_id);

-- Name: idx_steps_run_result; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_steps_run_result ON public.steps USING btree (run_result_id, idx);

-- Name: idx_user_invites_expires; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_user_invites_expires ON public.user_invites USING btree (expires_at) WHERE (accepted_at IS NULL);

-- Name: idx_user_invites_workspace; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_user_invites_workspace ON public.user_invites USING btree (workspace_id);

-- Name: idx_user_password_resets_expires; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_user_password_resets_expires ON public.user_password_resets USING btree (expires_at) WHERE (used_at IS NULL);

-- Name: idx_user_password_resets_user; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_user_password_resets_user ON public.user_password_resets USING btree (user_id);

-- Name: idx_user_sessions_expires; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_user_sessions_expires ON public.user_sessions USING btree (expires_at);

-- Name: idx_user_sessions_user; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_user_sessions_user ON public.user_sessions USING btree (user_id);

-- Name: idx_workspaces_deleted_at; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_workspaces_deleted_at ON public.workspaces USING btree (deleted_at) WHERE (deleted_at IS NOT NULL);

-- Name: run_results trg_run_results_set_workspace; Type: TRIGGER; Schema: public; Owner: -

CREATE TRIGGER trg_run_results_set_workspace BEFORE INSERT ON public.run_results FOR EACH ROW EXECUTE FUNCTION public.set_run_result_workspace_id();

-- Name: runs trg_runs_set_root; Type: TRIGGER; Schema: public; Owner: -

CREATE TRIGGER trg_runs_set_root BEFORE INSERT ON public.runs FOR EACH ROW EXECUTE FUNCTION public.set_run_root_run_id();

-- Name: agent_messages agent_messages_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.agent_messages
    ADD CONSTRAINT agent_messages_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE CASCADE;

-- Name: api_keys api_keys_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;

-- Name: artifacts artifacts_run_result_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.artifacts
    ADD CONSTRAINT artifacts_run_result_id_fkey FOREIGN KEY (run_result_id) REFERENCES public.run_results(id) ON DELETE CASCADE;

-- Name: audit_events audit_events_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;

-- Name: environments environments_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.environments
    ADD CONSTRAINT environments_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;

-- Name: environments environments_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.environments
    ADD CONSTRAINT environments_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

-- Name: integrations integrations_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.integrations
    ADD CONSTRAINT integrations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

-- Name: mqtt_credentials mqtt_credentials_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.mqtt_credentials
    ADD CONSTRAINT mqtt_credentials_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;

-- Name: projects projects_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;

-- Name: run_results run_results_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.run_results
    ADD CONSTRAINT run_results_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE CASCADE;

-- Name: runs runs_env_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_env_id_fkey FOREIGN KEY (env_id) REFERENCES public.environments(id) ON DELETE SET NULL;

-- Name: runs runs_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;

-- Name: runs runs_root_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_root_run_id_fkey FOREIGN KEY (root_run_id) REFERENCES public.runs(id);

-- Name: runs runs_script_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_script_id_fkey FOREIGN KEY (script_id) REFERENCES public.scripts(id) ON DELETE SET NULL;

-- Name: runs runs_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);


-- Name: script_patches script_patches_proposed_by_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.script_patches
    ADD CONSTRAINT script_patches_proposed_by_run_id_fkey FOREIGN KEY (proposed_by_run_id) REFERENCES public.runs(id) ON DELETE SET NULL;

-- Name: script_patches script_patches_script_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.script_patches
    ADD CONSTRAINT script_patches_script_id_fkey FOREIGN KEY (script_id) REFERENCES public.scripts(id) ON DELETE CASCADE;

-- Name: scripts scripts_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.scripts
    ADD CONSTRAINT scripts_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;

-- Name: scripts scripts_source_integration_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.scripts
    ADD CONSTRAINT scripts_source_integration_id_fkey FOREIGN KEY (source_integration_id) REFERENCES public.integrations(id) ON DELETE SET NULL;

-- Name: scripts scripts_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.scripts
    ADD CONSTRAINT scripts_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

-- Name: steps steps_run_result_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.steps
    ADD CONSTRAINT steps_run_result_id_fkey FOREIGN KEY (run_result_id) REFERENCES public.run_results(id) ON DELETE CASCADE;

-- Name: user_invites user_invites_invited_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.user_invites
    ADD CONSTRAINT user_invites_invited_by_fkey FOREIGN KEY (invited_by) REFERENCES public.users(id) ON DELETE SET NULL;

-- Name: user_invites user_invites_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.user_invites
    ADD CONSTRAINT user_invites_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;

-- Name: user_password_resets user_password_resets_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.user_password_resets
    ADD CONSTRAINT user_password_resets_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

-- Name: user_sessions user_sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.user_sessions
    ADD CONSTRAINT user_sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

-- Name: user_sessions user_sessions_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.user_sessions
    ADD CONSTRAINT user_sessions_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;

-- Name: users users_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;


-- ─── pgmqtt CDC topic mappings ────────────────────────────────────
-- These configure the embedded MQTT broker to publish row-change events
-- as MQTT messages. Stored in pgmqtt_topic_mappings; recreated on each
-- baseline replay so the broker config travels with the schema.

-- +goose StatementBegin
SELECT pgmqtt_add_outbound_mapping(
    'public',
    'runs',
    'runs/{{ columns.project_id }}/{{ columns.env_id }}/queue',
    '{"run_id":"{{ columns.id }}","project_id":"{{ columns.project_id }}","branch":"{{ columns.branch }}","commit_sha":"{{ columns.commit_sha }}","status":"{{ columns.status }}","test_filter":"{{ columns.test_filter }}","script_id":"{{ columns.script_id }}","env_id":"{{ columns.env_id }}"}',
    1
);
-- +goose StatementEnd

-- +goose StatementBegin
SELECT pgmqtt_add_outbound_mapping(
    'public',
    'runs',
    'runs/{{ columns.workspace_id }}/{{ columns.id }}/changed',
    $tmpl${
      "op":"{{ op | lower }}",
      "id":"{{ columns.id }}",
      "workspace_id":"{{ columns.workspace_id }}",
      "project_id":"{{ columns.project_id }}",
      "root_run_id":"{{ columns.root_run_id }}",
      "branch":"{{ columns.branch }}",
      "commit_sha":"{{ columns.commit_sha }}",
      "repo":{% if columns.repo and columns.repo != 'NULL' %}"{{ columns.repo }}"{% else %}null{% endif %},
      "status":"{{ columns.status }}",
      "auto_triaged":{% if columns.auto_triaged == 't' %}true{% else %}false{% endif %},
      "test_filter":"{{ columns.test_filter }}",
      "script_id":{% if columns.script_id and columns.script_id != 'NULL' %}"{{ columns.script_id }}"{% else %}null{% endif %},
      "env_id":{% if columns.env_id and columns.env_id != 'NULL' %}"{{ columns.env_id }}"{% else %}null{% endif %},
      "webhook_source":{% if columns.webhook_source and columns.webhook_source != 'NULL' %}"{{ columns.webhook_source }}"{% else %}null{% endif %},
      "started_at":{% if columns.started_at and columns.started_at != 'NULL' %}"{{ columns.started_at }}"{% else %}null{% endif %},
      "finished_at":{% if columns.finished_at and columns.finished_at != 'NULL' %}"{{ columns.finished_at }}"{% else %}null{% endif %},
      "created_at":"{{ columns.created_at }}"
    }$tmpl$,
    1,
    'ui_runs_changed'
);
-- +goose StatementEnd

-- +goose StatementBegin
SELECT pgmqtt_add_outbound_mapping(
    'public',
    'run_results',
    'runs/{{ columns.workspace_id }}/{{ columns.run_id }}/result/changed',
    $tmpl${
      "op":"{{ op | lower }}",
      "id":"{{ columns.id }}",
      "workspace_id":"{{ columns.workspace_id }}",
      "run_id":"{{ columns.run_id }}",
      "test_name":"{{ columns.test_name }}",
      "status":"{{ columns.status }}",
      "duration_ms":{% if columns.duration_ms and columns.duration_ms != 'NULL' %}{{ columns.duration_ms }}{% else %}null{% endif %},
      "error_message":{% if columns.error_message and columns.error_message != 'NULL' %}"{{ columns.error_message }}"{% else %}null{% endif %},
      "created_at":"{{ columns.created_at }}"
    }$tmpl$,
    1,
    'ui_run_results_changed'
);
-- +goose StatementEnd

-- ─── Bootstrap seeds ──────────────────────────────────────────────
-- Minimum rows the app needs to function on first boot. The default
-- workspace id is hardcoded in cmd/control-plane/auth.go for
-- single-tenant / self-hosted deployments.

-- +goose StatementBegin
INSERT INTO workspaces (id, name, slug) VALUES
    ('00000000-0000-0000-0000-000000000001', 'Default Workspace', 'default')
ON CONFLICT (id) DO NOTHING;
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO projects (id, workspace_id, name) VALUES
    ('00000000-0000-0000-0000-000000000001',
     '00000000-0000-0000-0000-000000000001',
     'Default Project')
ON CONFLICT (id) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- Baseline is intentionally non-reversible — there is no meaningful
-- prior state to roll back to. Drop the database to start over.
SELECT 1;
