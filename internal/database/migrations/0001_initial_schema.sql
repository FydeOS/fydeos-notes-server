-- Copyright (c) 2026 Fyde Innovations Limited
-- SPDX-License-Identifier: MIT

-- Initial schema for a fresh FydeOS Notes database. The semantic embedding
-- tables are created only when the pgvector extension is available.

CREATE TYPE "AiPromptRole" AS ENUM (
    'system',
    'assistant',
    'user'
);

CREATE TYPE "BlobStatus" AS ENUM (
    'pending',
    'completed'
);

CREATE TYPE "McpAccessMode" AS ENUM (
    'READ_ONLY',
    'READ_WRITE'
);

CREATE TYPE "NotificationLevel" AS ENUM (
    'High',
    'Default',
    'Low',
    'Min',
    'None'
);

CREATE TYPE "NotificationType" AS ENUM (
    'Mention',
    'Invitation',
    'InvitationAccepted',
    'InvitationBlocked',
    'InvitationRejected',
    'InvitationReviewRequest',
    'InvitationReviewApproved',
    'InvitationReviewDeclined',
    'Comment',
    'CommentMention'
);

CREATE TABLE ai_action_runs (
    id character varying NOT NULL,
    user_id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    doc_id character varying,
    session_id character varying,
    user_message_id character varying,
    compat_submission_id character varying,
    assistant_message_id character varying,
    action_id character varying NOT NULL,
    action_version character varying NOT NULL,
    status character varying NOT NULL,
    attempt integer DEFAULT 1 NOT NULL,
    retry_of character varying,
    input_snapshot json,
    result json,
    artifacts json,
    result_summary text,
    error_code character varying,
    trace json,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

CREATE TABLE ai_message_artifacts (
    message_id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    artifact_id uuid NOT NULL,
    role character varying NOT NULL,
    display_name character varying,
    metadata jsonb,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

CREATE TABLE ai_prompts_messages (
    prompt_id integer NOT NULL,
    idx integer NOT NULL,
    role "AiPromptRole" NOT NULL,
    content text NOT NULL,
    attachments json,
    params json,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

CREATE TABLE ai_prompts_metadata (
    id integer NOT NULL,
    name character varying(32) NOT NULL,
    action character varying,
    model character varying NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    config json,
    modified boolean DEFAULT false NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    optional_models character varying[] DEFAULT ARRAY[]::character varying[]
);

CREATE SEQUENCE ai_prompts_metadata_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE ai_prompts_metadata_id_seq OWNED BY ai_prompts_metadata.id;

CREATE TABLE ai_sessions_messages (
    id character varying NOT NULL,
    session_id character varying NOT NULL,
    role "AiPromptRole" NOT NULL,
    content text NOT NULL,
    attachments json,
    params json,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL,
    "streamObjects" json,
    compat_submission_id character varying,
    scope_snapshot jsonb
);

CREATE TABLE ai_sessions_metadata (
    id character varying NOT NULL,
    user_id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    doc_id character varying,
    prompt_name character varying(32) NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    deleted_at timestamp(3) with time zone,
    "messageCost" integer DEFAULT 0 NOT NULL,
    "tokenCost" integer DEFAULT 0 NOT NULL,
    parent_session_id character varying,
    pinned boolean DEFAULT false NOT NULL,
    prompt_action character varying(32) DEFAULT ''::character varying,
    title character varying,
    focus jsonb
);

CREATE TABLE ai_transcript_tasks (
    id character varying NOT NULL,
    user_id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    blob_id character varying NOT NULL,
    status character varying NOT NULL,
    strategy character varying DEFAULT ''::character varying NOT NULL,
    recipe_id character varying DEFAULT ''::character varying NOT NULL,
    recipe_version character varying DEFAULT ''::character varying NOT NULL,
    action_run_id character varying,
    input_snapshot json,
    public_meta json,
    protected_result json,
    error_code character varying,
    settled_at timestamp with time zone,
    dispatch_generation character varying,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

CREATE TABLE ai_usage_events (
    id character varying NOT NULL,
    workspace_id character varying,
    user_id character varying,
    provider character varying NOT NULL,
    provider_source character varying NOT NULL,
    feature_kind character varying NOT NULL,
    model character varying,
    session_id character varying,
    task_id character varying,
    action_id character varying,
    billing_unit_id character varying,
    prompt_tokens integer DEFAULT 0 NOT NULL,
    completion_tokens integer DEFAULT 0 NOT NULL,
    total_tokens integer DEFAULT 0 NOT NULL,
    cached_tokens integer DEFAULT 0 NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    local_workspace_id character varying,
    CONSTRAINT ai_usage_events_workspace_scope_check CHECK ((((workspace_id IS NOT NULL) AND (local_workspace_id IS NULL)) OR ((workspace_id IS NULL) AND (local_workspace_id IS NOT NULL) AND (user_id IS NOT NULL))))
);

CREATE TABLE ai_workspace_byok_configs (
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    provider character varying NOT NULL,
    name character varying NOT NULL,
    description character varying,
    encrypted_api_key text NOT NULL,
    definition jsonb NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    revision integer DEFAULT 1 NOT NULL,
    credential_generation integer DEFAULT 1 NOT NULL,
    validation jsonb,
    created_by character varying,
    updated_by character varying,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    last_used_at timestamp(3) with time zone,
    last_error_at timestamp(3) with time zone,
    last_error text
);

CREATE TABLE ai_workspace_ignored_docs (
    workspace_id character varying NOT NULL,
    doc_id character varying NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

CREATE TABLE app_configs (
    id character varying NOT NULL,
    value jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    last_updated_by character varying
);

CREATE TABLE auth_refresh_tokens (
    id character varying NOT NULL,
    auth_session_id character varying NOT NULL,
    generation integer NOT NULL,
    secret_hash character varying NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    expires_at timestamp(3) with time zone NOT NULL,
    used_at timestamp(3) with time zone,
    replaced_by_id character varying,
    grace_used_at timestamp(3) with time zone,
    revoked_at timestamp(3) with time zone,
    successor_ciphertext text,
    successor_expires_at timestamp(3) with time zone,
    CONSTRAINT auth_refresh_tokens_successor_material_check CHECK ((((successor_ciphertext IS NULL) AND (successor_expires_at IS NULL)) OR ((successor_ciphertext IS NOT NULL) AND (successor_expires_at IS NOT NULL))))
);

CREATE TABLE auth_sessions (
    id character varying NOT NULL,
    user_session_id character varying NOT NULL,
    installation_id character varying NOT NULL,
    platform character varying NOT NULL,
    device_name character varying,
    app_version character varying,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    last_seen_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    idle_expires_at timestamp(3) with time zone NOT NULL,
    absolute_expires_at timestamp(3) with time zone NOT NULL,
    revoked_at timestamp(3) with time zone,
    revoke_reason character varying,
    last_ip_hash character varying,
    last_user_agent character varying
);

CREATE TABLE blobs (
    workspace_id character varying NOT NULL,
    key character varying NOT NULL,
    size integer NOT NULL,
    mime character varying NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    deleted_at timestamp(3) with time zone,
    status "BlobStatus" DEFAULT 'completed'::"BlobStatus" NOT NULL,
    upload_id character varying,
    reservation_id uuid DEFAULT gen_random_uuid() NOT NULL,
    reservation_expires_at timestamp(3) with time zone
);

CREATE TABLE calendar_accounts (
    id character varying NOT NULL,
    user_id character varying NOT NULL,
    provider character varying NOT NULL,
    provider_account_id character varying NOT NULL,
    display_name character varying,
    email character varying,
    access_token text,
    refresh_token text,
    expires_at timestamp(3) with time zone,
    scope text,
    status character varying DEFAULT 'active'::character varying NOT NULL,
    last_error text,
    refresh_interval_minutes integer DEFAULT 30 NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL,
    auth_type character varying,
    calendar_home_url character varying,
    principal_url character varying,
    provider_preset_id character varying,
    server_url character varying,
    username character varying
);

CREATE TABLE calendar_events (
    id character varying NOT NULL,
    subscription_id character varying NOT NULL,
    external_event_id character varying NOT NULL,
    recurrence_id character varying,
    etag character varying,
    status character varying,
    title character varying,
    description text,
    location character varying,
    start_at_utc timestamp(3) with time zone NOT NULL,
    end_at_utc timestamp(3) with time zone NOT NULL,
    original_timezone character varying,
    all_day boolean DEFAULT false NOT NULL,
    provider_updated_at timestamp(3) with time zone,
    raw jsonb NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL
);

CREATE TABLE calendar_subscriptions (
    id character varying NOT NULL,
    account_id character varying NOT NULL,
    provider character varying NOT NULL,
    external_calendar_id character varying NOT NULL,
    display_name character varying,
    timezone character varying,
    color character varying,
    enabled boolean DEFAULT true NOT NULL,
    sync_token text,
    last_sync_at timestamp(3) with time zone,
    custom_channel_id character varying,
    custom_resource_id character varying,
    channel_expiration timestamp(3) with time zone,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL,
    next_sync_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    sync_retry_count integer DEFAULT 0 NOT NULL,
    sync_claimed_until timestamp(3) with time zone
);

CREATE TABLE comment_attachments (
    sid integer NOT NULL,
    workspace_id character varying NOT NULL,
    doc_id character varying NOT NULL,
    key character varying NOT NULL,
    size integer NOT NULL,
    mime character varying NOT NULL,
    name character varying NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    created_by character varying,
    status "BlobStatus" DEFAULT 'completed'::"BlobStatus" NOT NULL,
    reservation_id uuid DEFAULT gen_random_uuid() NOT NULL,
    reservation_expires_at timestamp(3) with time zone,
    deleted_at timestamp(3) with time zone
);

ALTER TABLE comment_attachments ALTER COLUMN sid ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME comment_attachments_sid_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE comments (
    sid integer NOT NULL,
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    doc_id character varying NOT NULL,
    user_id character varying NOT NULL,
    content jsonb NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    deleted_at timestamp(3) with time zone,
    resolved boolean DEFAULT false NOT NULL
);

ALTER TABLE comments ALTER COLUMN sid ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME comments_sid_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE doc_access_policies (
    workspace_id character varying NOT NULL,
    doc_id character varying NOT NULL,
    visibility text DEFAULT 'private'::text NOT NULL,
    public_role text,
    member_default_role text,
    url_preview_enabled boolean DEFAULT false NOT NULL,
    published_at timestamp(3) with time zone,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT doc_access_policies_check CHECK ((((visibility = 'public'::text) AND (public_role IS NOT NULL)) OR ((visibility = 'private'::text) AND (public_role IS NULL)))),
    CONSTRAINT doc_access_policies_member_default_role_check CHECK (((member_default_role IS NULL) OR (member_default_role = ANY (ARRAY['none'::text, 'reader'::text, 'commenter'::text, 'editor'::text, 'manager'::text])))),
    CONSTRAINT doc_access_policies_public_role_check CHECK (((public_role IS NULL) OR (public_role = 'external'::text))),
    CONSTRAINT doc_access_policies_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'public'::text])))
);

CREATE TABLE doc_blob_ref_projections (
    workspace_id text NOT NULL,
    doc_id text NOT NULL,
    source_revision timestamp(3) with time zone,
    parser_version integer NOT NULL,
    status text NOT NULL,
    indexed_at timestamp(3) with time zone,
    error_code text,
    error_summary text,
    attempt_count integer DEFAULT 0 NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT doc_blob_ref_projections_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT doc_blob_ref_projections_check CHECK (((status <> 'fresh'::text) OR ((source_revision IS NOT NULL) AND (indexed_at IS NOT NULL)))),
    CONSTRAINT doc_blob_ref_projections_error_summary_check CHECK (((error_summary IS NULL) OR (octet_length(error_summary) <= 512))),
    CONSTRAINT doc_blob_ref_projections_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'running'::text, 'fresh'::text, 'failed'::text, 'missing'::text])))
);

CREATE TABLE doc_grants (
    workspace_id character varying NOT NULL,
    doc_id character varying NOT NULL,
    principal_type text NOT NULL,
    principal_id character varying NOT NULL,
    role text NOT NULL,
    granted_by character varying,
    legacy_workspace_id character varying,
    legacy_doc_id character varying,
    legacy_user_id character varying,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT doc_grants_principal_type_check CHECK ((principal_type = ANY (ARRAY['user'::text, 'group'::text]))),
    CONSTRAINT doc_grants_role_check CHECK ((role = ANY (ARRAY['owner'::text, 'manager'::text, 'editor'::text, 'commenter'::text, 'reader'::text])))
);

CREATE TABLE effective_workspace_quota_states (
    workspace_id character varying NOT NULL,
    plan text NOT NULL,
    source_entitlement_id character varying,
    owner_user_id character varying,
    uses_owner_quota boolean DEFAULT false NOT NULL,
    seat_limit integer NOT NULL,
    member_count integer DEFAULT 0 NOT NULL,
    overcapacity_member_count integer DEFAULT 0 NOT NULL,
    blob_limit bigint NOT NULL,
    storage_quota bigint NOT NULL,
    used_storage_quota bigint DEFAULT 0 NOT NULL,
    history_period_seconds integer NOT NULL,
    readonly boolean DEFAULT false NOT NULL,
    readonly_reasons text[] DEFAULT ARRAY[]::text[] NOT NULL,
    flags jsonb DEFAULT '{}'::jsonb NOT NULL,
    known boolean DEFAULT false NOT NULL,
    stale boolean DEFAULT false NOT NULL,
    last_reconciled_at timestamp(3) with time zone,
    stale_after timestamp(3) with time zone,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT effective_workspace_quota_states_blob_limit_check CHECK ((blob_limit >= 0)),
    CONSTRAINT effective_workspace_quota_states_history_period_check CHECK ((history_period_seconds >= 0)),
    CONSTRAINT effective_workspace_quota_states_member_count_check CHECK ((member_count >= 0)),
    CONSTRAINT effective_workspace_quota_states_overcapacity_check CHECK ((overcapacity_member_count >= 0)),
    CONSTRAINT effective_workspace_quota_states_readonly_reasons_check CHECK ((readonly_reasons <@ ARRAY['member_overflow'::text, 'storage_overflow'::text])),
    CONSTRAINT effective_workspace_quota_states_seat_limit_check CHECK ((seat_limit >= 0)),
    CONSTRAINT effective_workspace_quota_states_storage_quota_check CHECK ((storage_quota >= 0)),
    CONSTRAINT effective_workspace_quota_states_used_storage_quota_check CHECK ((used_storage_quota >= 0))
);

CREATE TABLE entitlements (
    id character varying NOT NULL,
    target_type text NOT NULL,
    target_id character varying,
    source text NOT NULL,
    plan text NOT NULL,
    status text NOT NULL,
    subject_id character varying,
    issuer text,
    quantity integer,
    signed_payload bytea,
    token_hash text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    issued_at timestamp(3) with time zone,
    starts_at timestamp(3) with time zone,
    expires_at timestamp(3) with time zone,
    validated_at timestamp(3) with time zone,
    grace_until timestamp(3) with time zone,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT entitlements_quantity_check CHECK (((quantity IS NULL) OR ((quantity > 0) AND (quantity <= 100000)))),
    CONSTRAINT entitlements_signed_payload_source_check CHECK (((signed_payload IS NULL) OR (source = 'selfhost_license'::text))),
    CONSTRAINT entitlements_source_check CHECK ((source = ANY (ARRAY['builtin'::text, 'cloud_subscription'::text, 'selfhost_license'::text, 'admin_grant'::text]))),
    CONSTRAINT entitlements_status_check CHECK ((status = ANY (ARRAY['active'::text, 'grace'::text, 'expired'::text, 'revoked'::text, 'needs_reupload'::text]))),
    CONSTRAINT entitlements_target_type_check CHECK ((target_type = ANY (ARRAY['user'::text, 'workspace'::text, 'instance'::text])))
);

CREATE TABLE go_user_update_refresh_jobs (
    user_id character varying NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    phase smallint DEFAULT 0 NOT NULL,
    after_workspace_id character varying,
    after_grant_workspace_id character varying,
    after_grant_doc_id character varying,
    available_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    lease_token character varying,
    lease_until timestamp with time zone,
    attempt integer DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT go_user_update_refresh_jobs_attempt_check CHECK ((attempt >= 0)),
    CONSTRAINT go_user_update_refresh_jobs_check CHECK (((after_grant_workspace_id IS NULL) = (after_grant_doc_id IS NULL))),
    CONSTRAINT go_user_update_refresh_jobs_generation_check CHECK ((generation > 0)),
    CONSTRAINT go_user_update_refresh_jobs_phase_check CHECK ((phase = ANY (ARRAY[0, 1])))
);

CREATE TABLE mail_deliveries (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    mail_name text NOT NULL,
    mail_class text NOT NULL,
    priority text DEFAULT 'normal'::text NOT NULL,
    status text NOT NULL,
    dedupe_key text,
    recipient_email text,
    recipient_hash text NOT NULL,
    recipient_domain text NOT NULL,
    recipient_user_id character varying,
    actor_user_id character varying,
    workspace_id character varying,
    notification_id character varying,
    abuse_subject_key text,
    quota_reservation_id uuid,
    quota_decision jsonb,
    payload jsonb,
    send_after timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    expires_at timestamp(3) with time zone,
    attempt_count integer DEFAULT 0 NOT NULL,
    max_attempts integer DEFAULT 3 NOT NULL,
    locked_by text,
    locked_until timestamp(3) with time zone,
    first_attempt_at timestamp(3) with time zone,
    last_attempt_at timestamp(3) with time zone,
    sent_at timestamp(3) with time zone,
    settled_at timestamp(3) with time zone,
    canceled_at timestamp(3) with time zone,
    failed_at timestamp(3) with time zone,
    provider_message_id text,
    provider_response text,
    last_error_code text,
    last_error text,
    retention_state text DEFAULT 'full'::text NOT NULL,
    anonymized_at timestamp(3) with time zone,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT mail_deliveries_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT mail_deliveries_max_attempts_check CHECK ((max_attempts >= 0)),
    CONSTRAINT mail_deliveries_payload_retention_check CHECK ((((status = ANY (ARRAY['queued'::text, 'sending'::text, 'retry_wait'::text])) AND (retention_state = 'full'::text) AND (recipient_email IS NOT NULL) AND (payload IS NOT NULL)) OR ((status = ANY (ARRAY['sent'::text, 'skipped'::text, 'failed'::text, 'canceled'::text])) AND (retention_state = 'anonymized'::text) AND (recipient_email IS NULL) AND (payload IS NULL) AND (anonymized_at IS NOT NULL)))),
    CONSTRAINT mail_deliveries_priority_check CHECK ((priority = ANY (ARRAY['critical'::text, 'high'::text, 'normal'::text, 'low'::text]))),
    CONSTRAINT mail_deliveries_retention_state_check CHECK ((retention_state = ANY (ARRAY['full'::text, 'anonymized'::text]))),
    CONSTRAINT mail_deliveries_status_check CHECK ((status = ANY (ARRAY['queued'::text, 'sending'::text, 'retry_wait'::text, 'sent'::text, 'skipped'::text, 'failed'::text, 'canceled'::text])))
);

CREATE TABLE mcp_credentials (
    id character varying NOT NULL,
    family_id character varying NOT NULL,
    generation integer DEFAULT 0 NOT NULL,
    name character varying NOT NULL,
    secret_hash character varying NOT NULL,
    fingerprint character varying NOT NULL,
    user_id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    access_mode "McpAccessMode" DEFAULT 'READ_ONLY'::"McpAccessMode" NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    expires_at timestamp(3) with time zone NOT NULL,
    last_used_at timestamp(3) with time zone,
    revoked_at timestamp(3) with time zone,
    replaced_by_id character varying,
    grace_ends_at timestamp(3) with time zone
);

CREATE TABLE multiple_users_sessions (
    id character varying NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

CREATE TABLE notifications (
    id character varying NOT NULL,
    user_id character varying NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    level "NotificationLevel" NOT NULL,
    read boolean DEFAULT false NOT NULL,
    type "NotificationType" NOT NULL,
    body jsonb NOT NULL
);

CREATE TABLE replies (
    sid integer NOT NULL,
    id character varying NOT NULL,
    user_id character varying NOT NULL,
    comment_id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    doc_id character varying NOT NULL,
    content jsonb NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    deleted_at timestamp(3) with time zone
);

ALTER TABLE replies ALTER COLUMN sid ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME replies_sid_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE runtime_invite_abuse_actions (
    id bigint NOT NULL,
    subject_key text NOT NULL,
    evidence_id bigint NOT NULL,
    action text NOT NULL,
    status text NOT NULL,
    next_attempt_at timestamp with time zone,
    attempts integer DEFAULT 0 NOT NULL,
    locked_by text,
    locked_until timestamp with time zone,
    last_error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT runtime_invite_abuse_actions_action_check CHECK ((action = ANY (ARRAY['ban_actor'::text, 'quarantine_actor'::text, 'quarantine_workspace'::text, 'quarantine_source_cohort'::text]))),
    CONSTRAINT runtime_invite_abuse_actions_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT runtime_invite_abuse_actions_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'running'::text, 'succeeded'::text, 'retry_wait'::text, 'failed'::text])))
);

CREATE SEQUENCE runtime_invite_abuse_actions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE runtime_invite_abuse_actions_id_seq OWNED BY runtime_invite_abuse_actions.id;

CREATE TABLE runtime_invite_abuse_evidence (
    id bigint NOT NULL,
    subject_key text NOT NULL,
    request_id text,
    workspace_id text,
    user_id text,
    actor_email_hash text,
    source_prefix_hash text,
    source_asn bigint,
    target_domains jsonb DEFAULT '[]'::jsonb NOT NULL,
    counters jsonb DEFAULT '{}'::jsonb NOT NULL,
    decision text NOT NULL,
    reason text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE runtime_invite_abuse_evidence_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE runtime_invite_abuse_evidence_id_seq OWNED BY runtime_invite_abuse_evidence.id;

CREATE TABLE runtime_invite_abuse_subjects (
    subject_key text NOT NULL,
    kind text NOT NULL,
    user_id text,
    actor_email_hash text,
    email_domain text,
    first_seen_at timestamp with time zone NOT NULL,
    last_seen_at timestamp with time zone NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    action text,
    action_reason text,
    action_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE runtime_rolling_quota_counters (
    scope_key text NOT NULL,
    window_seconds integer NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    count integer NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT runtime_rolling_quota_counters_count_check CHECK ((count >= 0))
);

CREATE TABLE runtime_rolling_quota_reservations (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    scope_key text NOT NULL,
    window_seconds integer NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    count integer NOT NULL,
    status text NOT NULL,
    purpose text NOT NULL,
    request_id text,
    expires_at timestamp with time zone NOT NULL,
    committed_at timestamp with time zone,
    released_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT runtime_rolling_quota_reservations_count_check CHECK ((count >= 0)),
    CONSTRAINT runtime_rolling_quota_reservations_status_check CHECK ((status = ANY (ARRAY['reserved'::text, 'committed'::text, 'released'::text, 'expired'::text])))
);

CREATE TABLE runtime_states (
    purpose text NOT NULL,
    token_hash text NOT NULL,
    lookup_key text,
    payload jsonb NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    consumed_at timestamp with time zone,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE search_projection_jobs (
    workspace_id text NOT NULL,
    doc_id text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    available_at timestamp with time zone DEFAULT statement_timestamp() NOT NULL,
    claim_token uuid,
    claimed_until timestamp with time zone,
    last_error text,
    created_at timestamp with time zone DEFAULT statement_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT statement_timestamp() NOT NULL,
    CONSTRAINT search_projection_jobs_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT search_projection_jobs_check CHECK (((claim_token IS NULL) = (claimed_until IS NULL)))
);

CREATE TABLE search_projection_units (
    workspace_id text NOT NULL,
    doc_id text NOT NULL,
    block_id text NOT NULL,
    unit_id text NOT NULL,
    flavour text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    summary text DEFAULT ''::text NOT NULL,
    content text NOT NULL,
    blob text[] DEFAULT '{}'::text[] NOT NULL,
    ref_doc_id text[] DEFAULT '{}'::text[] NOT NULL,
    ref text[] DEFAULT '{}'::text[] NOT NULL,
    parent_flavour text DEFAULT ''::text NOT NULL,
    parent_block_id text DEFAULT ''::text NOT NULL,
    additional text DEFAULT '{}'::text NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    content_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple'::regconfig, ((((COALESCE(title, ''::text) || ' '::text) || COALESCE(summary, ''::text)) || ' '::text) || COALESCE(content, ''::text)))) STORED,
    markdown_preview text DEFAULT ''::text NOT NULL
);

CREATE TABLE snapshot_histories (
    workspace_id character varying NOT NULL,
    guid character varying NOT NULL,
    "timestamp" timestamp(3) with time zone NOT NULL,
    blob bytea NOT NULL,
    state bytea,
    expired_at timestamp(3) with time zone NOT NULL,
    created_by character varying
);

CREATE TABLE snapshots (
    workspace_id character varying NOT NULL,
    guid character varying NOT NULL,
    blob bytea NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL,
    state bytea,
    created_by character varying,
    updated_by character varying,
    size bigint
);

CREATE TABLE sync_active_users_minutely (
    minute_ts timestamp(3) with time zone NOT NULL,
    active_users integer DEFAULT 0 NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT now() NOT NULL
);

CREATE TABLE updates (
    workspace_id character varying NOT NULL,
    guid character varying NOT NULL,
    blob bytea NOT NULL,
    created_at timestamp(3) with time zone NOT NULL,
    created_by character varying
);

CREATE TABLE user_connected_accounts (
    id character varying NOT NULL,
    user_id character varying NOT NULL,
    provider character varying NOT NULL,
    provider_account_id character varying NOT NULL,
    scope text,
    access_token text,
    refresh_token text,
    expires_at timestamp(3) with time zone,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL,
    provider_namespace text
);

CREATE TABLE user_features (
    id integer NOT NULL,
    user_id character varying NOT NULL,
    reason character varying NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    expired_at timestamp(3) with time zone,
    activated boolean DEFAULT false NOT NULL,
    name character varying DEFAULT ''::character varying NOT NULL,
    type integer DEFAULT 0 NOT NULL
);

CREATE SEQUENCE user_features_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE user_features_id_seq OWNED BY user_features.id;

CREATE TABLE user_sessions (
    id character varying NOT NULL,
    session_id character varying NOT NULL,
    user_id character varying NOT NULL,
    expires_at timestamp(3) with time zone,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    sign_in_client_version character varying,
    refresh_client_version character varying
);

CREATE TABLE user_settings (
    user_id character varying NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL,
    payload jsonb NOT NULL
);

CREATE TABLE user_snapshots (
    user_id character varying NOT NULL,
    id character varying NOT NULL,
    blob bytea NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL
);

CREATE TABLE users (
    id character varying NOT NULL,
    name character varying NOT NULL,
    email character varying NOT NULL,
    password character varying,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    email_verified timestamp(3) with time zone,
    avatar_url character varying,
    registered boolean DEFAULT true NOT NULL,
    disabled boolean DEFAULT false NOT NULL,
    auth_epoch integer DEFAULT 0 NOT NULL
);

CREATE TABLE workspace_access_policies (
    workspace_id character varying NOT NULL,
    visibility text DEFAULT 'private'::text NOT NULL,
    sharing_enabled boolean DEFAULT true NOT NULL,
    url_preview_enabled boolean DEFAULT false NOT NULL,
    member_default_doc_role text DEFAULT 'manager'::text NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT workspace_access_policies_member_default_doc_role_check CHECK ((member_default_doc_role = ANY (ARRAY['none'::text, 'reader'::text, 'commenter'::text, 'editor'::text, 'manager'::text]))),
    CONSTRAINT workspace_access_policies_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'public'::text])))
);

CREATE TABLE workspace_admin_stats (
    workspace_id character varying NOT NULL,
    snapshot_count bigint DEFAULT 0 NOT NULL,
    snapshot_size bigint DEFAULT 0 NOT NULL,
    blob_count bigint DEFAULT 0 NOT NULL,
    blob_size bigint DEFAULT 0 NOT NULL,
    member_count bigint DEFAULT 0 NOT NULL,
    public_page_count bigint DEFAULT 0 NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT now() NOT NULL
);

CREATE TABLE workspace_admin_stats_daily (
    workspace_id character varying NOT NULL,
    date date NOT NULL,
    snapshot_size bigint DEFAULT 0 NOT NULL,
    blob_size bigint DEFAULT 0 NOT NULL,
    member_count bigint DEFAULT 0 NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT now() NOT NULL
);

CREATE TABLE workspace_artifacts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    workspace_id character varying NOT NULL,
    content_hash character varying NOT NULL,
    display_name character varying,
    file_name character varying,
    canonical_media_type character varying NOT NULL,
    size_bytes bigint NOT NULL,
    storage_scope character varying NOT NULL,
    storage_key text NOT NULL,
    status character varying NOT NULL,
    library_owned boolean DEFAULT false NOT NULL,
    reservation_expires_at timestamp(3) with time zone,
    ready_at timestamp(3) with time zone,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    extracted_text_runes bigint,
    CONSTRAINT workspace_artifacts_extracted_text_runes_check CHECK (((extracted_text_runes IS NULL) OR (extracted_text_runes >= 0))),
    CONSTRAINT workspace_artifacts_library_display_name_check CHECK (((NOT library_owned) OR (NULLIF(btrim((display_name)::text), ''::text) IS NOT NULL)))
);

CREATE TABLE workspace_calendar_items (
    id character varying NOT NULL,
    workspace_calendar_id character varying NOT NULL,
    subscription_id character varying NOT NULL,
    sort_order integer,
    color_override character varying,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL
);

CREATE TABLE workspace_calendars (
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    created_by_user_id character varying NOT NULL,
    display_name_override character varying,
    color_override character varying,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL
);

CREATE TABLE workspace_doc_view_daily (
    workspace_id character varying NOT NULL,
    doc_id character varying NOT NULL,
    date date NOT NULL,
    total_views bigint DEFAULT 0 NOT NULL,
    unique_views bigint DEFAULT 0 NOT NULL,
    guest_views bigint DEFAULT 0 NOT NULL,
    last_accessed_at timestamp(3) with time zone,
    updated_at timestamp(3) with time zone DEFAULT now() NOT NULL
);

CREATE TABLE workspace_invitations (
    id character varying DEFAULT ('perm_'::text || md5(((random())::text || (clock_timestamp())::text))) NOT NULL,
    workspace_id character varying NOT NULL,
    invitee_user_id character varying,
    normalized_email character varying,
    inviter_user_id character varying,
    requested_role text DEFAULT 'member'::text NOT NULL,
    status text NOT NULL,
    kind text DEFAULT 'email'::text NOT NULL,
    token_hash text,
    legacy_permission_id character varying,
    expires_at timestamp(3) with time zone,
    accepted_at timestamp(3) with time zone,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT workspace_invitations_check CHECK (((invitee_user_id IS NOT NULL) OR (normalized_email IS NOT NULL) OR (kind = 'link'::text))),
    CONSTRAINT workspace_invitations_kind_check CHECK ((kind = ANY (ARRAY['email'::text, 'link'::text]))),
    CONSTRAINT workspace_invitations_requested_role_check CHECK ((requested_role = ANY (ARRAY['admin'::text, 'member'::text]))),
    CONSTRAINT workspace_invitations_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'waiting_review'::text, 'waiting_seat'::text, 'accepted'::text, 'declined'::text, 'revoked'::text, 'expired'::text])))
);

CREATE TABLE workspace_member_last_access (
    workspace_id character varying NOT NULL,
    user_id character varying NOT NULL,
    last_accessed_at timestamp(3) with time zone NOT NULL,
    last_doc_id character varying,
    updated_at timestamp(3) with time zone DEFAULT now() NOT NULL
);

CREATE TABLE workspace_members (
    id character varying DEFAULT ('perm_'::text || md5(((random())::text || (clock_timestamp())::text))) NOT NULL,
    workspace_id character varying NOT NULL,
    user_id character varying NOT NULL,
    role text NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    source text DEFAULT 'legacy'::text NOT NULL,
    legacy_permission_id character varying,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT workspace_members_role_check CHECK ((role = ANY (ARRAY['owner'::text, 'admin'::text, 'member'::text]))),
    CONSTRAINT workspace_members_source_check CHECK ((source = ANY (ARRAY['legacy'::text, 'email'::text, 'link'::text, 'system'::text]))),
    CONSTRAINT workspace_members_state_check CHECK ((state = ANY (ARRAY['active'::text, 'suspended'::text, 'left'::text])))
);

CREATE TABLE workspace_pages (
    workspace_id character varying NOT NULL,
    page_id character varying NOT NULL,
    mode smallint DEFAULT 0 NOT NULL,
    summary character varying,
    title character varying,
    blocked boolean DEFAULT false NOT NULL,
    published_at timestamp(3) with time zone
);

CREATE TABLE workspace_storage_cleanup_jobs (
    workspace_id text NOT NULL,
    user_ids text[] DEFAULT ARRAY[]::text[] NOT NULL,
    attempt integer DEFAULT 0 NOT NULL,
    available_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    lease_token uuid,
    lease_until timestamp with time zone,
    last_error text,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT workspace_storage_cleanup_jobs_attempt_check CHECK ((attempt >= 0)),
    CONSTRAINT workspace_storage_cleanup_jobs_check CHECK (((lease_token IS NULL) = (lease_until IS NULL)))
);

CREATE TABLE workspace_sync_permission_generations (
    workspace_id character varying NOT NULL,
    generation bigint DEFAULT 0 NOT NULL
);

CREATE TABLE workspaces (
    id character varying NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    enable_ai boolean DEFAULT true NOT NULL,
    avatar_key character varying,
    name character varying,
    enable_doc_embedding boolean DEFAULT true NOT NULL,
    sid integer NOT NULL,
    indexed boolean DEFAULT false NOT NULL,
    last_check_embeddings timestamp(3) with time zone DEFAULT '1970-01-01 00:00:00+00'::timestamp with time zone NOT NULL
);

ALTER TABLE workspaces ALTER COLUMN sid ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME workspaces_sid_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

ALTER TABLE ONLY ai_prompts_metadata ALTER COLUMN id SET DEFAULT nextval('ai_prompts_metadata_id_seq'::regclass);

ALTER TABLE ONLY runtime_invite_abuse_actions ALTER COLUMN id SET DEFAULT nextval('runtime_invite_abuse_actions_id_seq'::regclass);

ALTER TABLE ONLY runtime_invite_abuse_evidence ALTER COLUMN id SET DEFAULT nextval('runtime_invite_abuse_evidence_id_seq'::regclass);

ALTER TABLE ONLY user_features ALTER COLUMN id SET DEFAULT nextval('user_features_id_seq'::regclass);

ALTER TABLE ONLY ai_action_runs
    ADD CONSTRAINT ai_action_runs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY ai_message_artifacts
    ADD CONSTRAINT ai_message_artifacts_pkey PRIMARY KEY (message_id, artifact_id, role);

ALTER TABLE ONLY ai_prompts_metadata
    ADD CONSTRAINT ai_prompts_metadata_pkey PRIMARY KEY (id);

ALTER TABLE ONLY ai_sessions_messages
    ADD CONSTRAINT ai_sessions_messages_pkey PRIMARY KEY (id);

ALTER TABLE ONLY ai_sessions_metadata
    ADD CONSTRAINT ai_sessions_metadata_pkey PRIMARY KEY (id);

ALTER TABLE ONLY ai_transcript_tasks
    ADD CONSTRAINT ai_transcript_tasks_pkey PRIMARY KEY (id);

ALTER TABLE ONLY ai_usage_events
    ADD CONSTRAINT ai_usage_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY ai_workspace_byok_configs
    ADD CONSTRAINT ai_workspace_byok_configs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY ai_workspace_ignored_docs
    ADD CONSTRAINT ai_workspace_ignored_docs_pkey PRIMARY KEY (workspace_id, doc_id);

ALTER TABLE ONLY app_configs
    ADD CONSTRAINT app_configs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY auth_refresh_tokens
    ADD CONSTRAINT auth_refresh_tokens_pkey PRIMARY KEY (id);

ALTER TABLE ONLY auth_sessions
    ADD CONSTRAINT auth_sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY blobs
    ADD CONSTRAINT blobs_pkey PRIMARY KEY (workspace_id, key);

ALTER TABLE ONLY calendar_accounts
    ADD CONSTRAINT calendar_accounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY calendar_events
    ADD CONSTRAINT calendar_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY calendar_subscriptions
    ADD CONSTRAINT calendar_subscriptions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY comment_attachments
    ADD CONSTRAINT comment_attachments_pkey PRIMARY KEY (workspace_id, doc_id, key);

ALTER TABLE ONLY comments
    ADD CONSTRAINT comments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY doc_access_policies
    ADD CONSTRAINT doc_access_policies_pkey PRIMARY KEY (workspace_id, doc_id);

ALTER TABLE ONLY doc_blob_ref_projections
    ADD CONSTRAINT doc_blob_ref_projections_pkey PRIMARY KEY (workspace_id, doc_id);

ALTER TABLE ONLY doc_grants
    ADD CONSTRAINT doc_grants_pkey PRIMARY KEY (workspace_id, doc_id, principal_type, principal_id);

ALTER TABLE ONLY effective_workspace_quota_states
    ADD CONSTRAINT effective_workspace_quota_states_pkey PRIMARY KEY (workspace_id);

ALTER TABLE ONLY entitlements
    ADD CONSTRAINT entitlements_pkey PRIMARY KEY (id);

ALTER TABLE ONLY go_user_update_refresh_jobs
    ADD CONSTRAINT go_user_update_refresh_jobs_pkey PRIMARY KEY (user_id);

ALTER TABLE ONLY mail_deliveries
    ADD CONSTRAINT mail_deliveries_pkey PRIMARY KEY (id);

ALTER TABLE ONLY mcp_credentials
    ADD CONSTRAINT mcp_credentials_pkey PRIMARY KEY (id);

ALTER TABLE ONLY multiple_users_sessions
    ADD CONSTRAINT multiple_users_sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY notifications
    ADD CONSTRAINT notifications_pkey PRIMARY KEY (id);

ALTER TABLE ONLY replies
    ADD CONSTRAINT replies_pkey PRIMARY KEY (id);

ALTER TABLE ONLY runtime_invite_abuse_actions
    ADD CONSTRAINT runtime_invite_abuse_actions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY runtime_invite_abuse_evidence
    ADD CONSTRAINT runtime_invite_abuse_evidence_pkey PRIMARY KEY (id);

ALTER TABLE ONLY runtime_invite_abuse_subjects
    ADD CONSTRAINT runtime_invite_abuse_subjects_pkey PRIMARY KEY (subject_key);

ALTER TABLE ONLY runtime_rolling_quota_counters
    ADD CONSTRAINT runtime_rolling_quota_counters_pkey PRIMARY KEY (scope_key, window_seconds, bucket_start);

ALTER TABLE ONLY runtime_rolling_quota_reservations
    ADD CONSTRAINT runtime_rolling_quota_reservations_pkey PRIMARY KEY (id, scope_key, window_seconds, bucket_start);

ALTER TABLE ONLY runtime_states
    ADD CONSTRAINT runtime_states_pkey PRIMARY KEY (purpose, token_hash);

ALTER TABLE ONLY search_projection_jobs
    ADD CONSTRAINT search_projection_jobs_pkey PRIMARY KEY (workspace_id, doc_id);

ALTER TABLE ONLY search_projection_units
    ADD CONSTRAINT search_projection_units_pkey PRIMARY KEY (workspace_id, doc_id, unit_id);

ALTER TABLE ONLY snapshot_histories
    ADD CONSTRAINT snapshot_histories_pkey PRIMARY KEY (workspace_id, guid, "timestamp");

ALTER TABLE ONLY snapshots
    ADD CONSTRAINT snapshots_pkey PRIMARY KEY (workspace_id, guid);

ALTER TABLE ONLY sync_active_users_minutely
    ADD CONSTRAINT sync_active_users_minutely_pkey PRIMARY KEY (minute_ts);

ALTER TABLE ONLY updates
    ADD CONSTRAINT updates_pkey PRIMARY KEY (workspace_id, guid, created_at);

ALTER TABLE ONLY user_connected_accounts
    ADD CONSTRAINT user_connected_accounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY user_features
    ADD CONSTRAINT user_features_pkey PRIMARY KEY (id);

ALTER TABLE ONLY user_sessions
    ADD CONSTRAINT user_sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY user_settings
    ADD CONSTRAINT user_settings_pkey PRIMARY KEY (user_id);

ALTER TABLE ONLY user_snapshots
    ADD CONSTRAINT user_snapshots_pkey PRIMARY KEY (user_id, id);

ALTER TABLE ONLY users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);

ALTER TABLE ONLY workspace_access_policies
    ADD CONSTRAINT workspace_access_policies_pkey PRIMARY KEY (workspace_id);

ALTER TABLE ONLY workspace_admin_stats_daily
    ADD CONSTRAINT workspace_admin_stats_daily_pkey PRIMARY KEY (workspace_id, date);

ALTER TABLE ONLY workspace_admin_stats
    ADD CONSTRAINT workspace_admin_stats_pkey PRIMARY KEY (workspace_id);

ALTER TABLE ONLY workspace_artifacts
    ADD CONSTRAINT workspace_artifacts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY workspace_calendar_items
    ADD CONSTRAINT workspace_calendar_items_pkey PRIMARY KEY (id);

ALTER TABLE ONLY workspace_calendars
    ADD CONSTRAINT workspace_calendars_pkey PRIMARY KEY (id);

ALTER TABLE ONLY workspace_doc_view_daily
    ADD CONSTRAINT workspace_doc_view_daily_pkey PRIMARY KEY (workspace_id, doc_id, date);

ALTER TABLE ONLY workspace_invitations
    ADD CONSTRAINT workspace_invitations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY workspace_member_last_access
    ADD CONSTRAINT workspace_member_last_access_pkey PRIMARY KEY (workspace_id, user_id);

ALTER TABLE ONLY workspace_members
    ADD CONSTRAINT workspace_members_pkey PRIMARY KEY (id);

ALTER TABLE ONLY workspace_pages
    ADD CONSTRAINT workspace_pages_pkey PRIMARY KEY (workspace_id, page_id);

ALTER TABLE ONLY workspace_storage_cleanup_jobs
    ADD CONSTRAINT workspace_storage_cleanup_jobs_pkey PRIMARY KEY (workspace_id);

ALTER TABLE ONLY workspace_sync_permission_generations
    ADD CONSTRAINT workspace_sync_permission_generations_pkey PRIMARY KEY (workspace_id);

ALTER TABLE ONLY workspaces
    ADD CONSTRAINT workspaces_pkey PRIMARY KEY (id);

CREATE INDEX ai_action_runs_action_id_action_version_idx ON ai_action_runs USING btree (action_id, action_version);

CREATE INDEX ai_action_runs_retry_of_idx ON ai_action_runs USING btree (retry_of);

CREATE INDEX ai_action_runs_session_id_idx ON ai_action_runs USING btree (session_id);

CREATE INDEX ai_action_runs_status_idx ON ai_action_runs USING btree (status);

CREATE INDEX ai_action_runs_user_id_workspace_id_idx ON ai_action_runs USING btree (user_id, workspace_id);

CREATE INDEX ai_message_artifacts_workspace_id_artifact_id_idx ON ai_message_artifacts USING btree (workspace_id, artifact_id);

CREATE UNIQUE INDEX ai_prompts_messages_prompt_id_idx_key ON ai_prompts_messages USING btree (prompt_id, idx);

CREATE UNIQUE INDEX ai_prompts_metadata_name_key ON ai_prompts_metadata USING btree (name);

CREATE UNIQUE INDEX ai_session_unique_pinned_idx ON ai_sessions_metadata USING btree (user_id, workspace_id) WHERE ((pinned = true) AND (deleted_at IS NULL));

CREATE INDEX ai_sessions_messages_created_at_role_idx ON ai_sessions_messages USING btree (created_at, role);

CREATE INDEX ai_sessions_messages_session_id_compat_submission_id_idx ON ai_sessions_messages USING btree (session_id, compat_submission_id);

CREATE INDEX ai_sessions_messages_session_id_idx ON ai_sessions_messages USING btree (session_id);

CREATE INDEX ai_sessions_metadata_missing_title_idx ON ai_sessions_metadata USING btree (updated_at) WHERE ((title IS NULL) AND (deleted_at IS NULL) AND ((COALESCE(prompt_action, ''::character varying))::text = ''::text));

CREATE INDEX ai_sessions_metadata_prompt_name_idx ON ai_sessions_metadata USING btree (prompt_name);

CREATE INDEX ai_sessions_metadata_user_id_idx ON ai_sessions_metadata USING btree (user_id);

CREATE INDEX ai_sessions_metadata_user_id_workspace_id_doc_id_idx ON ai_sessions_metadata USING btree (user_id, workspace_id, doc_id);

CREATE INDEX ai_transcript_tasks_action_run_id_idx ON ai_transcript_tasks USING btree (action_run_id);

CREATE INDEX ai_transcript_tasks_status_idx ON ai_transcript_tasks USING btree (status);

CREATE INDEX ai_transcript_tasks_user_id_workspace_id_idx ON ai_transcript_tasks USING btree (user_id, workspace_id);

CREATE INDEX ai_transcript_tasks_workspace_id_blob_id_idx ON ai_transcript_tasks USING btree (workspace_id, blob_id);

CREATE INDEX ai_usage_events_feature_kind_created_at_idx ON ai_usage_events USING btree (feature_kind, created_at);

CREATE INDEX ai_usage_events_quota_exempt_idx ON ai_usage_events USING btree (user_id, provider_source, feature_kind, billing_unit_id);

CREATE INDEX ai_usage_events_workspace_id_created_at_idx ON ai_usage_events USING btree (workspace_id, created_at);

CREATE INDEX ai_usage_events_workspace_id_provider_source_created_at_idx ON ai_usage_events USING btree (workspace_id, provider_source, created_at);

CREATE INDEX ai_workspace_byok_configs_workspace_id_idx ON ai_workspace_byok_configs USING btree (workspace_id);

CREATE INDEX ai_workspace_byok_configs_workspace_id_provider_enabled_sor_idx ON ai_workspace_byok_configs USING btree (workspace_id, provider, enabled, sort_order);

CREATE INDEX ai_workspace_byok_configs_workspace_id_provider_enabled_sort_or ON ai_workspace_byok_configs USING btree (workspace_id, provider, enabled, sort_order);

CREATE UNIQUE INDEX ai_workspace_byok_configs_workspace_id_provider_name_key ON ai_workspace_byok_configs USING btree (workspace_id, provider, name);

CREATE UNIQUE INDEX ai_workspace_byok_configs_workspace_provider_name_key ON ai_workspace_byok_configs USING btree (workspace_id, provider, name);

CREATE UNIQUE INDEX auth_refresh_tokens_auth_session_id_generation_key ON auth_refresh_tokens USING btree (auth_session_id, generation);

CREATE INDEX auth_refresh_tokens_auth_session_id_used_at_idx ON auth_refresh_tokens USING btree (auth_session_id, used_at);

CREATE INDEX auth_refresh_tokens_expires_at_idx ON auth_refresh_tokens USING btree (expires_at);

CREATE UNIQUE INDEX auth_refresh_tokens_replaced_by_id_key ON auth_refresh_tokens USING btree (replaced_by_id);

CREATE INDEX auth_sessions_absolute_expires_at_idx ON auth_sessions USING btree (absolute_expires_at);

CREATE INDEX auth_sessions_idle_expires_at_idx ON auth_sessions USING btree (idle_expires_at);

CREATE INDEX auth_sessions_installation_id_idx ON auth_sessions USING btree (installation_id);

CREATE UNIQUE INDEX auth_sessions_user_session_id_key ON auth_sessions USING btree (user_session_id);

CREATE INDEX blobs_workspace_id_status_deleted_at_idx ON blobs USING btree (workspace_id, status, deleted_at);

CREATE INDEX blobs_workspace_id_status_deleted_at_reservation_expires_at_idx ON blobs USING btree (workspace_id, status, deleted_at, reservation_expires_at);

CREATE INDEX calendar_accounts_provider_provider_account_id_idx ON calendar_accounts USING btree (provider, provider_account_id);

CREATE INDEX calendar_accounts_user_id_idx ON calendar_accounts USING btree (user_id);

CREATE UNIQUE INDEX calendar_accounts_user_id_provider_provider_account_id_key ON calendar_accounts USING btree (user_id, provider, provider_account_id);

CREATE INDEX calendar_events_subscription_id_end_at_utc_idx ON calendar_events USING btree (subscription_id, end_at_utc);

CREATE UNIQUE INDEX calendar_events_subscription_id_external_event_id_recurrenc_key ON calendar_events USING btree (subscription_id, external_event_id, recurrence_id);

CREATE INDEX calendar_events_subscription_id_start_at_utc_idx ON calendar_events USING btree (subscription_id, start_at_utc);

CREATE UNIQUE INDEX calendar_subscriptions_account_id_external_calendar_id_key ON calendar_subscriptions USING btree (account_id, external_calendar_id);

CREATE INDEX calendar_subscriptions_account_id_idx ON calendar_subscriptions USING btree (account_id);

CREATE INDEX calendar_subscriptions_custom_channel_id_idx ON calendar_subscriptions USING btree (custom_channel_id);

CREATE INDEX calendar_subscriptions_enabled_next_sync_at_idx ON calendar_subscriptions USING btree (enabled, next_sync_at);

CREATE INDEX calendar_subscriptions_provider_external_calendar_id_idx ON calendar_subscriptions USING btree (provider, external_calendar_id);

CREATE INDEX calendar_subscriptions_sync_claim_idx ON calendar_subscriptions USING btree (enabled, next_sync_at, sync_claimed_until);

CREATE UNIQUE INDEX comment_attachments_sid_key ON comment_attachments USING btree (sid);

CREATE INDEX comment_attachments_workspace_id_status_deleted_at_reservation_ ON comment_attachments USING btree (workspace_id, status, deleted_at, reservation_expires_at);

CREATE UNIQUE INDEX comments_sid_key ON comments USING btree (sid);

CREATE INDEX comments_user_id_idx ON comments USING btree (user_id);

CREATE INDEX comments_workspace_id_doc_id_sid_idx ON comments USING btree (workspace_id, doc_id, sid);

CREATE INDEX comments_workspace_id_doc_id_updated_at_idx ON comments USING btree (workspace_id, doc_id, updated_at);

CREATE INDEX doc_access_policies_public_idx ON doc_access_policies USING btree (workspace_id, visibility, published_at) WHERE (visibility = 'public'::text);

CREATE INDEX doc_access_policies_workspace_id_doc_id_idx ON doc_access_policies USING btree (workspace_id, doc_id);

CREATE INDEX doc_blob_ref_projections_workspace_revision_idx ON doc_blob_ref_projections USING btree (workspace_id, source_revision);

CREATE INDEX doc_blob_ref_projections_workspace_status_idx ON doc_blob_ref_projections USING btree (workspace_id, status, updated_at DESC);

CREATE UNIQUE INDEX doc_grants_legacy_key ON doc_grants USING btree (legacy_workspace_id, legacy_doc_id, legacy_user_id) WHERE ((legacy_workspace_id IS NOT NULL) AND (legacy_doc_id IS NOT NULL) AND (legacy_user_id IS NOT NULL));

CREATE UNIQUE INDEX doc_grants_owner_key ON doc_grants USING btree (workspace_id, doc_id) WHERE ((principal_type = 'user'::text) AND (role = 'owner'::text));

CREATE INDEX doc_grants_principal_type_principal_id_role_idx ON doc_grants USING btree (principal_type, principal_id, role);

CREATE INDEX doc_grants_workspace_id_doc_id_role_idx ON doc_grants USING btree (workspace_id, doc_id, role);

CREATE INDEX effective_workspace_quota_states_known_stale_idx ON effective_workspace_quota_states USING btree (known, stale);

CREATE INDEX effective_workspace_quota_states_owner_user_id_idx ON effective_workspace_quota_states USING btree (owner_user_id);

CREATE INDEX effective_workspace_quota_states_readonly_stale_idx ON effective_workspace_quota_states USING btree (readonly, stale);

CREATE INDEX effective_workspace_quota_states_stale_after_idx ON effective_workspace_quota_states USING btree (stale_after);

CREATE UNIQUE INDEX entitlements_active_subject_key ON entitlements USING btree (source, subject_id) WHERE ((subject_id IS NOT NULL) AND (status = ANY (ARRAY['active'::text, 'grace'::text])));

CREATE INDEX entitlements_status_expires_at_idx ON entitlements USING btree (status, expires_at);

CREATE INDEX entitlements_target_type_target_id_status_idx ON entitlements USING btree (target_type, target_id, status);

CREATE INDEX go_user_update_refresh_jobs_claim_idx ON go_user_update_refresh_jobs USING btree (available_at, lease_until, user_id);

CREATE UNIQUE INDEX go_users_email_lower_unique_idx ON users USING btree (lower((email)::text));

CREATE INDEX idx_blobs_active ON blobs USING btree (workspace_id) WHERE ((deleted_at IS NULL) AND (status = 'completed'::"BlobStatus"));

CREATE INDEX mail_deliveries_abuse_subject_key_created_at_idx ON mail_deliveries USING btree (abuse_subject_key, created_at);

CREATE INDEX mail_deliveries_actor_user_id_created_at_idx ON mail_deliveries USING btree (actor_user_id, created_at);

CREATE INDEX mail_deliveries_created_at_idx ON mail_deliveries USING btree (created_at);

CREATE UNIQUE INDEX mail_deliveries_dedupe_key_idx ON mail_deliveries USING btree (dedupe_key) WHERE (dedupe_key IS NOT NULL);

CREATE INDEX mail_deliveries_mail_class_status_created_at_idx ON mail_deliveries USING btree (mail_class, status, created_at);

CREATE INDEX mail_deliveries_mail_name_status_created_at_idx ON mail_deliveries USING btree (mail_name, status, created_at);

CREATE INDEX mail_deliveries_pending_expires_at_idx ON mail_deliveries USING btree (expires_at) WHERE ((status = ANY (ARRAY['queued'::text, 'sending'::text, 'retry_wait'::text])) AND (expires_at IS NOT NULL));

CREATE INDEX mail_deliveries_ready_idx ON mail_deliveries USING btree (status, send_after, locked_until);

CREATE INDEX mail_deliveries_recipient_domain_created_at_idx ON mail_deliveries USING btree (recipient_domain, created_at);

CREATE INDEX mail_deliveries_settled_at_idx ON mail_deliveries USING btree (settled_at);

CREATE INDEX mail_deliveries_settled_name_status_idx ON mail_deliveries USING btree (settled_at, mail_name, status) WHERE (settled_at IS NOT NULL);

CREATE INDEX mail_deliveries_status_created_at_idx ON mail_deliveries USING btree (status, created_at);

CREATE INDEX mail_deliveries_workspace_id_created_at_idx ON mail_deliveries USING btree (workspace_id, created_at);

CREATE INDEX mcp_credentials_expires_at_idx ON mcp_credentials USING btree (expires_at);

CREATE UNIQUE INDEX mcp_credentials_family_id_generation_key ON mcp_credentials USING btree (family_id, generation);

CREATE UNIQUE INDEX mcp_credentials_secret_hash_key ON mcp_credentials USING btree (secret_hash);

CREATE INDEX mcp_credentials_user_id_workspace_id_idx ON mcp_credentials USING btree (user_id, workspace_id);

CREATE INDEX notifications_user_id_created_at_read_idx ON notifications USING btree (user_id, created_at, read);

CREATE INDEX replies_comment_id_sid_idx ON replies USING btree (comment_id, sid);

CREATE UNIQUE INDEX replies_sid_key ON replies USING btree (sid);

CREATE INDEX replies_user_id_idx ON replies USING btree (user_id);

CREATE INDEX replies_workspace_id_doc_id_updated_at_idx ON replies USING btree (workspace_id, doc_id, updated_at);

CREATE INDEX runtime_invite_abuse_actions_due_idx ON runtime_invite_abuse_actions USING btree (COALESCE(next_attempt_at, created_at), id) WHERE (status = ANY (ARRAY['pending'::text, 'retry_wait'::text]));

CREATE INDEX runtime_invite_abuse_actions_expired_lease_idx ON runtime_invite_abuse_actions USING btree (locked_until, id) WHERE (status = 'running'::text);

CREATE INDEX runtime_invite_abuse_actions_status_next_attempt_idx ON runtime_invite_abuse_actions USING btree (status, next_attempt_at);

CREATE INDEX runtime_invite_abuse_evidence_subject_id_idx ON runtime_invite_abuse_evidence USING btree (subject_key, id DESC);

CREATE INDEX runtime_invite_abuse_evidence_workspace_idx ON runtime_invite_abuse_evidence USING btree (workspace_id, subject_key) WHERE (workspace_id IS NOT NULL);

CREATE INDEX runtime_invite_abuse_subjects_user_status_idx ON runtime_invite_abuse_subjects USING btree (user_id, status) WHERE (user_id IS NOT NULL);

CREATE INDEX runtime_rolling_quota_counters_expires_at_idx ON runtime_rolling_quota_counters USING btree (expires_at);

CREATE INDEX runtime_rolling_quota_reservations_scope_idx ON runtime_rolling_quota_reservations USING btree (scope_key, window_seconds, bucket_start) WHERE (status = 'reserved'::text);

CREATE INDEX runtime_rolling_quota_reservations_status_expires_at_idx ON runtime_rolling_quota_reservations USING btree (status, expires_at);

CREATE INDEX runtime_states_expires_at_idx ON runtime_states USING btree (expires_at);

CREATE INDEX runtime_states_lookup_idx ON runtime_states USING btree (purpose, lookup_key) WHERE ((lookup_key IS NOT NULL) AND (consumed_at IS NULL));

CREATE INDEX search_projection_jobs_available ON search_projection_jobs USING btree (available_at, workspace_id, doc_id) WHERE (claim_token IS NULL);

CREATE INDEX search_projection_jobs_expired_claims ON search_projection_jobs USING btree (claimed_until) WHERE (claim_token IS NOT NULL);

CREATE INDEX search_projection_units_fts ON search_projection_units USING gin (content_vector);

CREATE INDEX search_projection_units_workspace_doc ON search_projection_units USING btree (workspace_id, doc_id, updated_at DESC);

CREATE INDEX snapshots_workspace_id_updated_at_idx ON snapshots USING btree (workspace_id, updated_at);

CREATE INDEX user_connected_accounts_provider_account_id_idx ON user_connected_accounts USING btree (provider_account_id);

CREATE UNIQUE INDEX user_connected_accounts_provider_namespace_subject_key ON user_connected_accounts USING btree (provider_namespace, provider_account_id) WHERE (provider_namespace IS NOT NULL);

CREATE INDEX user_connected_accounts_user_id_idx ON user_connected_accounts USING btree (user_id);

CREATE INDEX user_features_name_idx ON user_features USING btree (name);

CREATE INDEX user_features_user_id_idx ON user_features USING btree (user_id);

CREATE UNIQUE INDEX user_sessions_session_id_user_id_key ON user_sessions USING btree (session_id, user_id);

CREATE INDEX users_email_idx ON users USING btree (email);

CREATE UNIQUE INDEX users_email_key ON users USING btree (email);

CREATE INDEX users_email_lowercase_idx ON users USING btree (lower((email)::text));

CREATE INDEX workspace_access_policies_url_preview_enabled_sharing_enabl_idx ON workspace_access_policies USING btree (url_preview_enabled, sharing_enabled);

CREATE INDEX workspace_access_policies_visibility_idx ON workspace_access_policies USING btree (visibility);

CREATE INDEX workspace_admin_stats_blob_count_idx ON workspace_admin_stats USING btree (blob_count DESC);

CREATE INDEX workspace_admin_stats_blob_size_idx ON workspace_admin_stats USING btree (blob_size DESC);

CREATE INDEX workspace_admin_stats_daily_date_idx ON workspace_admin_stats_daily USING btree (date);

CREATE INDEX workspace_admin_stats_member_count_idx ON workspace_admin_stats USING btree (member_count DESC);

CREATE INDEX workspace_admin_stats_public_page_count_idx ON workspace_admin_stats USING btree (public_page_count DESC);

CREATE INDEX workspace_admin_stats_snapshot_count_idx ON workspace_admin_stats USING btree (snapshot_count DESC);

CREATE INDEX workspace_admin_stats_snapshot_size_idx ON workspace_admin_stats USING btree (snapshot_size DESC);

CREATE INDEX workspace_artifacts_status_reservation_expires_at_idx ON workspace_artifacts USING btree (status, reservation_expires_at);

CREATE UNIQUE INDEX workspace_artifacts_workspace_id_content_hash_key ON workspace_artifacts USING btree (workspace_id, content_hash);

CREATE UNIQUE INDEX workspace_artifacts_workspace_id_id_key ON workspace_artifacts USING btree (workspace_id, id);

CREATE INDEX workspace_artifacts_workspace_id_status_idx ON workspace_artifacts USING btree (workspace_id, status);

CREATE INDEX workspace_calendar_items_subscription_id_idx ON workspace_calendar_items USING btree (subscription_id);

CREATE UNIQUE INDEX workspace_calendar_items_workspace_calendar_id_subscription_key ON workspace_calendar_items USING btree (workspace_calendar_id, subscription_id);

CREATE INDEX workspace_calendars_workspace_id_idx ON workspace_calendars USING btree (workspace_id);

CREATE INDEX workspace_doc_view_daily_workspace_id_date_idx ON workspace_doc_view_daily USING btree (workspace_id, date);

CREATE INDEX workspace_invitations_invitee_user_id_status_idx ON workspace_invitations USING btree (invitee_user_id, status);

CREATE INDEX workspace_invitations_inviter_created_at_idx ON workspace_invitations USING btree (inviter_user_id, created_at);

CREATE INDEX workspace_invitations_inviter_status_created_at_idx ON workspace_invitations USING btree (inviter_user_id, status, created_at);

CREATE UNIQUE INDEX workspace_invitations_legacy_permission_id_key ON workspace_invitations USING btree (legacy_permission_id) WHERE (legacy_permission_id IS NOT NULL);

CREATE UNIQUE INDEX workspace_invitations_token_hash_key ON workspace_invitations USING btree (token_hash) WHERE (token_hash IS NOT NULL);

CREATE INDEX workspace_invitations_workspace_accepted_at_idx ON workspace_invitations USING btree (workspace_id, accepted_at);

CREATE UNIQUE INDEX workspace_invitations_workspace_id_invitee_user_id_key ON workspace_invitations USING btree (workspace_id, invitee_user_id);

CREATE INDEX workspace_invitations_workspace_id_normalized_email_status_idx ON workspace_invitations USING btree (workspace_id, normalized_email, status);

CREATE INDEX workspace_invitations_workspace_id_status_idx ON workspace_invitations USING btree (workspace_id, status);

CREATE INDEX workspace_invitations_workspace_inviter_created_at_idx ON workspace_invitations USING btree (workspace_id, inviter_user_id, created_at);

CREATE INDEX workspace_invitations_workspace_status_created_at_idx ON workspace_invitations USING btree (workspace_id, status, created_at);

CREATE INDEX workspace_member_last_access_workspace_id_last_accessed_at_idx ON workspace_member_last_access USING btree (workspace_id, last_accessed_at DESC);

CREATE INDEX workspace_member_last_access_workspace_id_last_doc_id_idx ON workspace_member_last_access USING btree (workspace_id, last_doc_id);

CREATE UNIQUE INDEX workspace_members_active_owner_key ON workspace_members USING btree (workspace_id) WHERE ((role = 'owner'::text) AND (state = 'active'::text));

CREATE UNIQUE INDEX workspace_members_active_user_key ON workspace_members USING btree (workspace_id, user_id) WHERE (state = 'active'::text);

CREATE UNIQUE INDEX workspace_members_legacy_permission_id_key ON workspace_members USING btree (legacy_permission_id) WHERE (legacy_permission_id IS NOT NULL);

CREATE INDEX workspace_members_user_id_state_idx ON workspace_members USING btree (user_id, state);

CREATE INDEX workspace_members_workspace_id_role_state_idx ON workspace_members USING btree (workspace_id, role, state);

CREATE UNIQUE INDEX workspace_members_workspace_id_user_id_state_key ON workspace_members USING btree (workspace_id, user_id, state);

CREATE INDEX workspace_members_workspace_state_created_at_idx ON workspace_members USING btree (workspace_id, state, created_at);

CREATE INDEX workspace_storage_cleanup_due_idx ON workspace_storage_cleanup_jobs USING btree (available_at, workspace_id) WHERE (lease_until IS NULL);

CREATE INDEX workspaces_created_at_idx ON workspaces USING btree (created_at);

CREATE INDEX workspaces_last_check_embeddings_idx ON workspaces USING btree (last_check_embeddings);

CREATE UNIQUE INDEX workspaces_sid_key ON workspaces USING btree (sid);

CREATE FUNCTION bump_workspace_sync_permission_generation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE affected_workspace_id varchar;
BEGIN
  affected_workspace_id := COALESCE(NEW.workspace_id, OLD.workspace_id);
  IF NOT EXISTS (SELECT 1 FROM workspaces WHERE id=affected_workspace_id) THEN
    RETURN COALESCE(NEW, OLD);
  END IF;
  INSERT INTO workspace_sync_permission_generations(workspace_id,generation)
  VALUES (affected_workspace_id,1)
  ON CONFLICT (workspace_id) DO UPDATE
    SET generation=workspace_sync_permission_generations.generation+1;
  RETURN COALESCE(NEW, OLD);
END;
$$;

CREATE FUNCTION enqueue_search_projection_for_source_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    target_workspace text;
    target_doc text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        target_workspace := OLD.workspace_id;
        target_doc := OLD.guid;
    ELSE
        target_workspace := NEW.workspace_id;
        target_doc := NEW.guid;
    END IF;
    IF target_workspace IS NULL OR target_doc IS NULL OR target_workspace = target_doc THEN
        IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
    END IF;

    INSERT INTO search_projection_jobs(workspace_id, doc_id)
    VALUES (target_workspace, target_doc)
    ON CONFLICT (workspace_id, doc_id) DO UPDATE SET
        available_at = statement_timestamp(),
        claim_token = NULL,
        claimed_until = NULL,
        last_error = NULL,
        updated_at = statement_timestamp();
    IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END;
$$;

CREATE FUNCTION go_enqueue_user_feature_refresh() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    target_user_id varchar;
    old_admin boolean := false;
    new_admin boolean := false;
BEGIN
    IF TG_OP = 'INSERT' THEN
        target_user_id := NEW.user_id;
        new_admin := NEW.name = 'administrator' AND NEW.activated AND (NEW.expired_at IS NULL OR NEW.expired_at > clock_timestamp());
    ELSIF TG_OP = 'DELETE' THEN
        target_user_id := OLD.user_id;
        old_admin := OLD.name = 'administrator' AND OLD.activated AND (OLD.expired_at IS NULL OR OLD.expired_at > clock_timestamp());
    ELSE
        target_user_id := NEW.user_id;
        old_admin := OLD.name = 'administrator' AND OLD.activated AND (OLD.expired_at IS NULL OR OLD.expired_at > clock_timestamp());
        new_admin := NEW.name = 'administrator' AND NEW.activated AND (NEW.expired_at IS NULL OR NEW.expired_at > clock_timestamp());
    END IF;

    IF TG_OP = 'UPDATE' AND OLD.user_id IS DISTINCT FROM NEW.user_id THEN
        IF old_admin AND EXISTS (SELECT 1 FROM users WHERE id = OLD.user_id AND disabled = false) THEN
            PERFORM go_enqueue_user_update_refresh(OLD.user_id);
        END IF;
        IF new_admin AND EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id AND disabled = false) THEN
            PERFORM go_enqueue_user_update_refresh(NEW.user_id);
        END IF;
        RETURN NEW;
    END IF;

    IF old_admin IS DISTINCT FROM new_admin
       AND EXISTS (SELECT 1 FROM users WHERE id = target_user_id AND disabled = false) THEN
        PERFORM go_enqueue_user_update_refresh(target_user_id);
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION go_enqueue_user_profile_refresh() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.name IS DISTINCT FROM NEW.name OR OLD.avatar_url IS DISTINCT FROM NEW.avatar_url THEN
        PERFORM go_enqueue_user_update_refresh(NEW.id);
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION go_enqueue_user_update_refresh(target_user_id character varying) RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO go_user_update_refresh_jobs(user_id)
    VALUES (target_user_id)
    ON CONFLICT (user_id) DO UPDATE SET
        generation = go_user_update_refresh_jobs.generation + 1,
        phase = 0,
        after_workspace_id = NULL,
        after_grant_workspace_id = NULL,
        after_grant_doc_id = NULL,
        available_at = clock_timestamp(),
        lease_token = NULL,
        lease_until = NULL,
        attempt = 0,
        updated_at = clock_timestamp()
$$;

CREATE FUNCTION guard_workspace_storage_lifecycle_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NOT pg_try_advisory_xact_lock(
    hashtextextended('storage-workspace:' || NEW.id, 0)
  ) THEN
    RAISE EXCEPTION 'workspace storage lifecycle is busy'
      USING ERRCODE = '40001';
  END IF;
  RETURN NEW;
END;
$$;

CREATE FUNCTION lock_personal_copilot_workspace_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(
    hashtextextended('copilot-personal:' || NEW.id, 0)
  );
  RETURN NEW;
END;
$$;

CREATE FUNCTION preserve_blob_source_timestamps() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW."blob" IS DISTINCT FROM OLD."blob"
    AND NEW."updated_at" <= OLD."updated_at" THEN
    NEW."updated_at" := OLD."updated_at" + INTERVAL '1 millisecond';
  END IF;
  RETURN NEW;
END;
$$;

CREATE FUNCTION reject_immutable_blob_source_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  RAISE EXCEPTION '% source content is immutable', TG_TABLE_NAME
    USING ERRCODE = '55000';
END;
$$;

CREATE FUNCTION snapshots_set_size() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  NEW."size" := octet_length(NEW."blob");
  RETURN NEW;
END;
$$;

CREATE TRIGGER doc_access_policies_sync_permission_generation AFTER INSERT OR DELETE OR UPDATE ON doc_access_policies FOR EACH ROW EXECUTE FUNCTION bump_workspace_sync_permission_generation();

CREATE TRIGGER doc_grants_sync_permission_generation AFTER INSERT OR DELETE OR UPDATE ON doc_grants FOR EACH ROW EXECUTE FUNCTION bump_workspace_sync_permission_generation();

CREATE TRIGGER go_user_feature_refresh_after_change AFTER INSERT OR DELETE OR UPDATE ON user_features FOR EACH ROW EXECUTE FUNCTION go_enqueue_user_feature_refresh();

CREATE TRIGGER go_user_profile_refresh_after_update AFTER UPDATE OF name, avatar_url ON users FOR EACH ROW EXECUTE FUNCTION go_enqueue_user_profile_refresh();

CREATE TRIGGER guard_workspace_storage_lifecycle_insert BEFORE INSERT ON workspaces FOR EACH ROW EXECUTE FUNCTION guard_workspace_storage_lifecycle_insert();

CREATE TRIGGER lock_personal_copilot_workspace_insert BEFORE INSERT ON workspaces FOR EACH ROW EXECUTE FUNCTION lock_personal_copilot_workspace_insert();

CREATE TRIGGER preserve_snapshot_blob_source_timestamp BEFORE UPDATE OF blob ON snapshots FOR EACH ROW EXECUTE FUNCTION preserve_blob_source_timestamps();

CREATE TRIGGER reject_snapshot_history_source_change BEFORE UPDATE OF workspace_id, guid, "timestamp", blob ON snapshot_histories FOR EACH ROW EXECUTE FUNCTION reject_immutable_blob_source_change();

CREATE TRIGGER reject_update_source_change BEFORE UPDATE OF workspace_id, guid, blob, created_at ON updates FOR EACH ROW EXECUTE FUNCTION reject_immutable_blob_source_change();

CREATE TRIGGER snapshots_search_projection_enqueue AFTER INSERT OR DELETE OR UPDATE ON snapshots FOR EACH ROW EXECUTE FUNCTION enqueue_search_projection_for_source_change();

CREATE TRIGGER snapshots_set_size_before_write BEFORE INSERT OR UPDATE OF blob ON snapshots FOR EACH ROW EXECUTE FUNCTION snapshots_set_size();

CREATE TRIGGER updates_search_projection_enqueue AFTER INSERT OR DELETE OR UPDATE ON updates FOR EACH ROW EXECUTE FUNCTION enqueue_search_projection_for_source_change();

CREATE TRIGGER workspace_access_policies_sync_permission_generation AFTER INSERT OR DELETE OR UPDATE ON workspace_access_policies FOR EACH ROW EXECUTE FUNCTION bump_workspace_sync_permission_generation();

CREATE TRIGGER workspace_members_sync_permission_generation AFTER INSERT OR DELETE OR UPDATE ON workspace_members FOR EACH ROW EXECUTE FUNCTION bump_workspace_sync_permission_generation();

ALTER TABLE ONLY ai_action_runs
    ADD CONSTRAINT ai_action_runs_session_id_fkey FOREIGN KEY (session_id) REFERENCES ai_sessions_metadata(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY ai_message_artifacts
    ADD CONSTRAINT ai_message_artifacts_message_id_fkey FOREIGN KEY (message_id) REFERENCES ai_sessions_messages(id) ON DELETE CASCADE;

ALTER TABLE ONLY ai_message_artifacts
    ADD CONSTRAINT ai_message_artifacts_workspace_id_artifact_id_fkey FOREIGN KEY (workspace_id, artifact_id) REFERENCES workspace_artifacts(workspace_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY ai_prompts_messages
    ADD CONSTRAINT ai_prompts_messages_prompt_id_fkey FOREIGN KEY (prompt_id) REFERENCES ai_prompts_metadata(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY ai_sessions_messages
    ADD CONSTRAINT ai_sessions_messages_session_id_fkey FOREIGN KEY (session_id) REFERENCES ai_sessions_metadata(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY ai_sessions_metadata
    ADD CONSTRAINT ai_sessions_metadata_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY ai_usage_events
    ADD CONSTRAINT ai_usage_events_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY ai_workspace_byok_configs
    ADD CONSTRAINT ai_workspace_byok_configs_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY ai_workspace_ignored_docs
    ADD CONSTRAINT ai_workspace_ignored_docs_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY app_configs
    ADD CONSTRAINT app_configs_last_updated_by_fkey FOREIGN KEY (last_updated_by) REFERENCES users(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY auth_refresh_tokens
    ADD CONSTRAINT auth_refresh_tokens_auth_session_id_fkey FOREIGN KEY (auth_session_id) REFERENCES auth_sessions(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY auth_refresh_tokens
    ADD CONSTRAINT auth_refresh_tokens_replaced_by_id_fkey FOREIGN KEY (replaced_by_id) REFERENCES auth_refresh_tokens(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY auth_sessions
    ADD CONSTRAINT auth_sessions_user_session_id_fkey FOREIGN KEY (user_session_id) REFERENCES user_sessions(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY blobs
    ADD CONSTRAINT blobs_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY calendar_accounts
    ADD CONSTRAINT calendar_accounts_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY calendar_events
    ADD CONSTRAINT calendar_events_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES calendar_subscriptions(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY calendar_subscriptions
    ADD CONSTRAINT calendar_subscriptions_account_id_fkey FOREIGN KEY (account_id) REFERENCES calendar_accounts(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY comment_attachments
    ADD CONSTRAINT comment_attachments_created_by_fkey FOREIGN KEY (created_by) REFERENCES users(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY comment_attachments
    ADD CONSTRAINT comment_attachments_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY comments
    ADD CONSTRAINT comments_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY comments
    ADD CONSTRAINT comments_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY doc_access_policies
    ADD CONSTRAINT doc_access_policies_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY doc_grants
    ADD CONSTRAINT doc_grants_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY effective_workspace_quota_states
    ADD CONSTRAINT effective_workspace_quota_states_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY effective_workspace_quota_states
    ADD CONSTRAINT effective_workspace_quota_states_source_entitlement_id_fkey FOREIGN KEY (source_entitlement_id) REFERENCES entitlements(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY effective_workspace_quota_states
    ADD CONSTRAINT effective_workspace_quota_states_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY go_user_update_refresh_jobs
    ADD CONSTRAINT go_user_update_refresh_jobs_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;

ALTER TABLE ONLY mcp_credentials
    ADD CONSTRAINT mcp_credentials_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY mcp_credentials
    ADD CONSTRAINT mcp_credentials_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY notifications
    ADD CONSTRAINT notifications_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY replies
    ADD CONSTRAINT replies_comment_id_fkey FOREIGN KEY (comment_id) REFERENCES comments(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY replies
    ADD CONSTRAINT replies_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY runtime_invite_abuse_actions
    ADD CONSTRAINT runtime_invite_abuse_actions_evidence_id_fkey FOREIGN KEY (evidence_id) REFERENCES runtime_invite_abuse_evidence(id);

ALTER TABLE ONLY runtime_invite_abuse_actions
    ADD CONSTRAINT runtime_invite_abuse_actions_subject_key_fkey FOREIGN KEY (subject_key) REFERENCES runtime_invite_abuse_subjects(subject_key);

ALTER TABLE ONLY runtime_invite_abuse_evidence
    ADD CONSTRAINT runtime_invite_abuse_evidence_subject_key_fkey FOREIGN KEY (subject_key) REFERENCES runtime_invite_abuse_subjects(subject_key);

ALTER TABLE ONLY snapshot_histories
    ADD CONSTRAINT snapshot_histories_created_by_fkey FOREIGN KEY (created_by) REFERENCES users(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY snapshots
    ADD CONSTRAINT snapshots_created_by_fkey FOREIGN KEY (created_by) REFERENCES users(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY snapshots
    ADD CONSTRAINT snapshots_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES users(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY updates
    ADD CONSTRAINT updates_created_by_fkey FOREIGN KEY (created_by) REFERENCES users(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY user_connected_accounts
    ADD CONSTRAINT user_connected_accounts_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY user_features
    ADD CONSTRAINT user_features_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY user_sessions
    ADD CONSTRAINT user_sessions_session_id_fkey FOREIGN KEY (session_id) REFERENCES multiple_users_sessions(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY user_sessions
    ADD CONSTRAINT user_sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY user_settings
    ADD CONSTRAINT user_settings_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY user_snapshots
    ADD CONSTRAINT user_snapshots_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_access_policies
    ADD CONSTRAINT workspace_access_policies_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_admin_stats_daily
    ADD CONSTRAINT workspace_admin_stats_daily_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_admin_stats
    ADD CONSTRAINT workspace_admin_stats_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_artifacts
    ADD CONSTRAINT workspace_artifacts_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE;

ALTER TABLE ONLY workspace_calendar_items
    ADD CONSTRAINT workspace_calendar_items_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES calendar_subscriptions(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_calendar_items
    ADD CONSTRAINT workspace_calendar_items_workspace_calendar_id_fkey FOREIGN KEY (workspace_calendar_id) REFERENCES workspace_calendars(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_calendars
    ADD CONSTRAINT workspace_calendars_created_by_user_id_fkey FOREIGN KEY (created_by_user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_calendars
    ADD CONSTRAINT workspace_calendars_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_doc_view_daily
    ADD CONSTRAINT workspace_doc_view_daily_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_invitations
    ADD CONSTRAINT workspace_invitations_invitee_user_id_fkey FOREIGN KEY (invitee_user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY workspace_invitations
    ADD CONSTRAINT workspace_invitations_inviter_user_id_fkey FOREIGN KEY (inviter_user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE ONLY workspace_invitations
    ADD CONSTRAINT workspace_invitations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_member_last_access
    ADD CONSTRAINT workspace_member_last_access_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_member_last_access
    ADD CONSTRAINT workspace_member_last_access_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_members
    ADD CONSTRAINT workspace_members_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_members
    ADD CONSTRAINT workspace_members_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_pages
    ADD CONSTRAINT workspace_pages_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON UPDATE CASCADE ON DELETE CASCADE;

ALTER TABLE ONLY workspace_sync_permission_generations
    ADD CONSTRAINT workspace_sync_permission_generations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE;


--

DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;
EXCEPTION
    WHEN undefined_file OR insufficient_privilege THEN
        RAISE NOTICE 'pgvector is unavailable; semantic embedding runtime remains disabled';
END;
$$;

DO $embedding$
BEGIN
    IF to_regtype('public.vector') IS NULL THEN
        RETURN;
    END IF;

    EXECUTE $schema$
        CREATE FUNCTION enqueue_changed_embedding_source() RETURNS trigger
            LANGUAGE plpgsql
            AS $$
                BEGIN
                    UPDATE embedding_projections SET status='pending', next_attempt_at=NULL, updated_at=now()
                    WHERE source_id=NEW.id AND status='ready';
                    RETURN NEW;
                END;
                $$
    $schema$;
    EXECUTE $schema$
        CREATE TABLE embedding_chunks (
            generation_token uuid NOT NULL,
            workspace_id text NOT NULL,
            index_id uuid NOT NULL,
            source_id uuid NOT NULL,
            chunk_index integer NOT NULL,
            content text NOT NULL,
            embedding public.vector(1024) NOT NULL,
            source_kind text NOT NULL,
            doc_id text,
            artifact_id uuid,
            unit_id text,
            visibility text,
            block_id text,
            element_id text,
            frame_id text,
            created_at timestamp with time zone DEFAULT now() NOT NULL,
            CONSTRAINT embedding_chunks_check CHECK ((((source_kind = 'document'::text) AND (doc_id IS NOT NULL) AND (artifact_id IS NULL)) OR ((source_kind = 'artifact'::text) AND (artifact_id IS NOT NULL) AND (doc_id IS NULL)))),
            CONSTRAINT embedding_chunks_chunk_index_check CHECK ((chunk_index >= 0)),
            CONSTRAINT embedding_chunks_source_kind_check CHECK ((source_kind = ANY (ARRAY['document'::text, 'artifact'::text])))
        )
    $schema$;
    EXECUTE $schema$
        CREATE TABLE embedding_indexes (
            id uuid NOT NULL,
            workspace_id text NOT NULL,
            fingerprint text NOT NULL,
            route_source text NOT NULL,
            provider text NOT NULL,
            model_id text NOT NULL,
            endpoint_fingerprint text NOT NULL,
            dimensions integer DEFAULT 1024 NOT NULL,
            distance_metric text DEFAULT 'cosine'::text NOT NULL,
            contract_version integer NOT NULL,
            health_status text NOT NULL,
            failure_count integer DEFAULT 0 NOT NULL,
            next_probe_at timestamp with time zone,
            probe_lease_owner text,
            probe_lease_until timestamp with time zone,
            last_error_code text,
            activated_at timestamp with time zone DEFAULT now() NOT NULL,
            inactive_at timestamp with time zone,
            created_at timestamp with time zone DEFAULT now() NOT NULL,
            updated_at timestamp with time zone DEFAULT now() NOT NULL,
            CONSTRAINT embedding_indexes_dimensions_check CHECK ((dimensions = 1024)),
            CONSTRAINT embedding_indexes_distance_metric_check CHECK ((distance_metric = 'cosine'::text)),
            CONSTRAINT embedding_indexes_health_status_check CHECK ((health_status = ANY (ARRAY['pending'::text, 'ready'::text, 'retry_wait'::text, 'incompatible'::text]))),
            CONSTRAINT embedding_indexes_route_source_check CHECK ((route_source = ANY (ARRAY['byok'::text, 'managed'::text])))
        )
    $schema$;
    EXECUTE $schema$
        CREATE TABLE embedding_projections (
            source_id uuid NOT NULL,
            index_id uuid NOT NULL,
            status text NOT NULL,
            applied_content_revision text,
            applied_descriptor_revision text,
            applied_recipe_revision text,
            active_generation_token uuid,
            priority integer DEFAULT 0 NOT NULL,
            attempt_count integer DEFAULT 0 NOT NULL,
            next_attempt_at timestamp with time zone,
            lease_owner text,
            lease_token bigint DEFAULT 0 NOT NULL,
            lease_until timestamp with time zone,
            last_error_code text,
            last_error_detail text,
            created_at timestamp with time zone DEFAULT now() NOT NULL,
            updated_at timestamp with time zone DEFAULT now() NOT NULL,
            CONSTRAINT embedding_projections_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'running'::text, 'retry_wait'::text, 'ready'::text, 'failed'::text])))
        )
    $schema$;
    EXECUTE $schema$
        CREATE TABLE embedding_sources (
            id uuid NOT NULL,
            workspace_id text NOT NULL,
            source_kind text NOT NULL,
            source_key text NOT NULL,
            content_revision text NOT NULL,
            descriptor_revision text NOT NULL,
            recipe_revision text NOT NULL,
            storage_scope text,
            storage_key text,
            file_name text,
            mime_type text,
            document_projection jsonb,
            size_bytes bigint,
            deleted_at timestamp with time zone,
            created_at timestamp with time zone DEFAULT now() NOT NULL,
            updated_at timestamp with time zone DEFAULT now() NOT NULL,
            CONSTRAINT embedding_sources_check CHECK ((((source_kind = 'document'::text) AND (storage_scope IS NULL) AND (storage_key IS NULL) AND (document_projection IS NOT NULL)) OR ((source_kind = 'artifact'::text) AND (storage_scope IS NOT NULL) AND (storage_key IS NOT NULL) AND (document_projection IS NULL)))),
            CONSTRAINT embedding_sources_source_kind_check CHECK ((source_kind = ANY (ARRAY['document'::text, 'artifact'::text]))),
            CONSTRAINT embedding_sources_storage_scope_check CHECK ((storage_scope = ANY (ARRAY['blob'::text, 'copilot'::text])))
        )
    $schema$;
    EXECUTE $schema$
        CREATE TABLE embedding_workspace_states (
            workspace_id text NOT NULL,
            active_index_id uuid,
            index_epoch bigint DEFAULT 0 NOT NULL,
            runtime_state text NOT NULL,
            reason_code text,
            changed_at timestamp with time zone DEFAULT now() NOT NULL,
            CONSTRAINT embedding_workspace_states_runtime_state_check CHECK ((runtime_state = ANY (ARRAY['active'::text, 'disabled'::text, 'unavailable'::text])))
        )
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_chunks
            ADD CONSTRAINT embedding_chunks_pkey PRIMARY KEY (generation_token, chunk_index)
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_indexes
            ADD CONSTRAINT embedding_indexes_pkey PRIMARY KEY (id)
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_indexes
            ADD CONSTRAINT embedding_indexes_workspace_id_fingerprint_key UNIQUE (workspace_id, fingerprint)
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_projections
            ADD CONSTRAINT embedding_projections_pkey PRIMARY KEY (source_id, index_id)
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_sources
            ADD CONSTRAINT embedding_sources_pkey PRIMARY KEY (id)
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_sources
            ADD CONSTRAINT embedding_sources_workspace_id_source_kind_source_key_key UNIQUE (workspace_id, source_kind, source_key)
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_workspace_states
            ADD CONSTRAINT embedding_workspace_states_pkey PRIMARY KEY (workspace_id)
    $schema$;
    EXECUTE $schema$
        CREATE INDEX embedding_chunks_artifact_idx ON embedding_chunks USING btree (workspace_id, artifact_id) WHERE (artifact_id IS NOT NULL)
    $schema$;
    EXECUTE $schema$
        CREATE INDEX embedding_chunks_hnsw ON embedding_chunks USING hnsw (embedding public.vector_cosine_ops) WITH (m='32', ef_construction='200')
    $schema$;
    EXECUTE $schema$
        CREATE INDEX embedding_chunks_scope_idx ON embedding_chunks USING btree (workspace_id, index_id, source_id)
    $schema$;
    EXECUTE $schema$
        CREATE INDEX embedding_indexes_inactive_idx ON embedding_indexes USING btree (inactive_at) WHERE (inactive_at IS NOT NULL)
    $schema$;
    EXECUTE $schema$
        CREATE INDEX embedding_projection_claim_idx ON embedding_projections USING btree (priority DESC, next_attempt_at NULLS FIRST, updated_at) WHERE (status = ANY (ARRAY['pending'::text, 'retry_wait'::text, 'running'::text]))
    $schema$;
    EXECUTE $schema$
        CREATE INDEX embedding_sources_workspace_idx ON embedding_sources USING btree (workspace_id, source_kind) WHERE (deleted_at IS NULL)
    $schema$;
    EXECUTE $schema$
        CREATE TRIGGER embedding_source_revision_changed AFTER UPDATE OF content_revision, descriptor_revision, recipe_revision ON embedding_sources FOR EACH ROW WHEN (((old.content_revision IS DISTINCT FROM new.content_revision) OR (old.descriptor_revision IS DISTINCT FROM new.descriptor_revision) OR (old.recipe_revision IS DISTINCT FROM new.recipe_revision))) EXECUTE FUNCTION enqueue_changed_embedding_source()
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_chunks
            ADD CONSTRAINT embedding_chunks_index_id_fkey FOREIGN KEY (index_id) REFERENCES embedding_indexes(id) ON DELETE CASCADE
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_chunks
            ADD CONSTRAINT embedding_chunks_source_id_fkey FOREIGN KEY (source_id) REFERENCES embedding_sources(id) ON DELETE CASCADE
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_projections
            ADD CONSTRAINT embedding_projections_index_id_fkey FOREIGN KEY (index_id) REFERENCES embedding_indexes(id) ON DELETE CASCADE
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_projections
            ADD CONSTRAINT embedding_projections_source_id_fkey FOREIGN KEY (source_id) REFERENCES embedding_sources(id) ON DELETE CASCADE
    $schema$;
    EXECUTE $schema$
        ALTER TABLE ONLY embedding_workspace_states
            ADD CONSTRAINT embedding_workspace_states_active_index_fkey FOREIGN KEY (active_index_id) REFERENCES embedding_indexes(id) ON DELETE SET NULL
    $schema$;
END;
$embedding$;
