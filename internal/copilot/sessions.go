// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrSessionNotFound      = errors.New("copilot session not found")
	ErrMessageNotFound      = errors.New("Copilot message not found.")
	ErrPersonalScope        = errors.New("personal copilot scope is not available")
	ErrManagedQuotaExceeded = errors.New("the daily Copilot request limit has been reached")
)

type Message struct {
	ID            string          `json:"id"`
	Role          string          `json:"role"`
	Content       string          `json:"content"`
	Attachments   json.RawMessage `json:"attachments"`
	StreamObjects json.RawMessage `json:"streamObjects"`
	ScopeSnapshot json.RawMessage `json:"scopeSnapshot"`
	CreatedAt     time.Time       `json:"createdAt"`
}

type Session struct {
	ID              string    `json:"sessionId"`
	UserID          string    `json:"-"`
	WorkspaceID     string    `json:"workspaceId"`
	DocID           *string   `json:"docId"`
	ParentSessionID *string   `json:"parentSessionId"`
	PromptName      string    `json:"promptName"`
	Action          *string   `json:"action"`
	Pinned          bool      `json:"pinned"`
	Title           *string   `json:"title"`
	Messages        []Message `json:"messages"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type CreateSessionInput struct {
	UserID          string
	WorkspaceID     string
	DocID           *string
	PromptName      string
	Pinned          bool
	ReuseLatestChat bool
	ManagedQuota    bool
}

var ErrCopilotActionPromptInvalid = errors.New("Copilot action prompts require a document session")

type ListSessionsInput struct {
	UserID       string
	WorkspaceID  string
	DocID        *string
	SessionID    *string
	Pinned       *bool
	Action       *bool
	Fork         *bool
	WithMessages bool
	MessageOrder string
	Limit        int
	Offset       int
	Order        string
}

type SessionPage struct {
	Sessions   []Session
	TotalCount int
}

type UpdateSessionInput struct {
	UserID      string
	SessionID   string
	WorkspaceID string
	Personal    bool
	DocID       *string
	SetDocID    bool
	Pinned      *bool
	PromptName  *string
}

type ForkSessionInput struct {
	UserID          string
	WorkspaceID     string
	DocID           string
	SessionID       string
	LatestMessageID *string
	Personal        bool
}

type CreateUserMessageInput struct {
	UserID              string
	SessionID           string
	Content             *string
	Attachments         []string
	ArtifactAttachments []MessageArtifactAttachment
	Params              json.RawMessage
}

type DB interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Begin(context.Context) (pgx.Tx, error)
}

type Store struct{ db DB }

func NewStore(db DB) *Store { return &Store{db: db} }

func (s *Store) WorkspaceExists(ctx context.Context, workspaceID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, workspaceID).Scan(&exists)
	return exists, err
}

func (s *Store) UserMessageCost(ctx context.Context, userID string) (int64, error) {
	return countUserQuotaCost(ctx, s.db, userID)
}

const FydeosManagedQuotaLimit int64 = 100

func (s *Store) CountFydeosManagedRequests(ctx context.Context, userID string, at time.Time) (int64, error) {
	return countFydeosManagedRequests(ctx, s.db, userID, at)
}

type quotaQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func countFydeosManagedRequests(ctx context.Context, db quotaQueryer, userID string, at time.Time) (int64, error) {
	utc := at.UTC()
	dayStart := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	var used int64
	err := db.QueryRow(ctx, `
		WITH user_sessions AS (
		  SELECT id, prompt_action, created_at FROM ai_sessions_metadata WHERE user_id=$1
		), legacy_chat_units AS (
		  SELECT 'chat:' || message.id AS unit_id
		  FROM user_sessions AS session
		  JOIN ai_sessions_messages AS message ON message.session_id=session.id
		  WHERE message.role='user' AND message.created_at >= $2
		    AND (session.prompt_action IS NULL OR session.prompt_action='')
		    AND NOT EXISTS (SELECT 1 FROM ai_action_runs run WHERE run.user_message_id=message.id)
		    AND NOT EXISTS (
		      SELECT 1 FROM ai_usage_events usage
		      WHERE usage.user_id=$1
		        AND usage.provider_source IN ('byok_server','byok_local','fydeos_byok_selection','fydeos_unsupported_request')
		        AND usage.billing_unit_id=message.id AND usage.created_at >= $2
		    )
		    AND NOT EXISTS (
		      SELECT 1 FROM ai_usage_events usage
		      WHERE usage.user_id=$1 AND usage.provider_source='fydeos_managed_quota_denied'
		        AND (usage.billing_unit_id=message.id OR usage.task_id=message.id) AND usage.created_at >= $2
		    )
		    AND NOT EXISTS (
		      SELECT 1 FROM ai_usage_events usage
		      WHERE usage.user_id=$1 AND usage.provider_source IN ('fydeos_managed_quota','fydeos_managed_quota_released')
		        AND usage.billing_unit_id=message.id AND usage.created_at >= $2
		    )
		), legacy_action_session_units AS (
		  SELECT 'action-session:' || session.id AS unit_id
		  FROM user_sessions AS session
		  WHERE session.prompt_action IS NOT NULL AND session.prompt_action <> '' AND session.created_at >= $2
		    AND NOT EXISTS (
		      SELECT 1 FROM ai_usage_events usage
		      WHERE usage.user_id=$1 AND usage.session_id=session.id
		        AND usage.provider_source IN ('fydeos_managed_quota','fydeos_managed_quota_denied','fydeos_managed_quota_released',
		          'byok_server','byok_local','fydeos_byok_selection','fydeos_unsupported_request')
		    )
		), legacy_action_run_units AS (
		  SELECT CASE WHEN run.session_id IS NULL
		    THEN 'action-run:' || COALESCE(run.user_message_id,run.id)
		    ELSE 'action-session:' || run.session_id END AS unit_id
		  FROM ai_action_runs AS run
		  LEFT JOIN user_sessions AS session ON session.id=run.session_id
		  WHERE run.user_id=$1 AND run.status='succeeded' AND run.action_id NOT LIKE 'transcript.audio.%'
		    AND COALESCE(session.created_at,run.created_at) >= $2
		    AND NOT EXISTS (
		      SELECT 1 FROM ai_usage_events usage
		      WHERE usage.user_id=$1
		        AND usage.provider_source IN ('byok_server','byok_local','fydeos_byok_selection','fydeos_unsupported_request')
		        AND usage.billing_unit_id IN (run.id,run.user_message_id) AND usage.created_at >= $2
		    )
		    AND NOT EXISTS (
		      SELECT 1 FROM ai_usage_events usage
		      WHERE usage.user_id=$1 AND usage.provider_source='fydeos_managed_quota_denied'
		        AND usage.billing_unit_id IN (run.id,COALESCE(run.user_message_id,run.id)) AND usage.created_at >= $2
		    )
		    AND NOT EXISTS (
		      SELECT 1 FROM ai_usage_events usage
		      WHERE usage.user_id=$1 AND usage.provider_source IN ('fydeos_managed_quota','fydeos_managed_quota_denied')
		        AND (usage.billing_unit_id=COALESCE(run.user_message_id,run.id)
		          OR (run.session_id IS NOT NULL AND usage.session_id=run.session_id))
		    )
		), managed_units AS (
		  SELECT DISTINCT 'managed:' || usage.task_id AS unit_id
		  FROM ai_usage_events usage
		  WHERE usage.user_id=$1 AND usage.provider_source='fydeos_managed_quota'
		    AND usage.feature_kind IN ('chat','action') AND usage.task_id IS NOT NULL AND usage.created_at >= $2
		), all_units AS (
		  SELECT unit_id FROM legacy_chat_units
		  UNION SELECT unit_id FROM legacy_action_session_units
		  UNION SELECT unit_id FROM legacy_action_run_units
		  UNION SELECT unit_id FROM managed_units
		)
		SELECT COUNT(*)::bigint FROM all_units`, userID, dayStart).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("count FydeOS managed Copilot requests: %w", err)
	}
	return used, nil
}

func (s *Store) ReserveFydeosManagedRequest(ctx context.Context, userID, workspaceID, sessionID, billingUnitID, provider, model string, personal bool, at time.Time) (string, error) {
	return s.ReserveFydeosManagedRequestForFeature(ctx, userID, workspaceID, sessionID, billingUnitID, provider, model, "chat", personal, at)
}

func (s *Store) ReserveFydeosManagedRequestForFeature(ctx context.Context, userID, workspaceID, sessionID, billingUnitID, provider, model, featureKind string, personal bool, at time.Time) (string, error) {
	if s == nil || s.db == nil || userID == "" || sessionID == "" || billingUnitID == "" || provider == "" || model == "" {
		return "", errors.New("managed Copilot quota reservation is invalid")
	}
	if featureKind != "chat" && featureKind != "action" {
		return "", errors.New("managed Copilot feature kind is invalid")
	}
	utc := at.UTC()
	dayStart := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin managed Copilot quota reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockKey := "managed-copilot-quota:" + userID + ":" + dayStart.Format("2006-01-02")
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return "", fmt.Errorf("lock managed Copilot quota: %w", err)
	}

	var eventID, source string
	err = tx.QueryRow(ctx, `SELECT id,provider_source FROM ai_usage_events
		WHERE user_id=$1 AND billing_unit_id=$2 AND created_at >= $3
		  AND provider_source IN ('fydeos_managed_quota','fydeos_managed_quota_released','fydeos_managed_quota_denied')
		ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, userID, billingUnitID, dayStart).Scan(&eventID, &source)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("read managed Copilot quota unit: %w", err)
	}
	if source == "fydeos_managed_quota" {
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit managed Copilot quota reservation: %w", err)
		}
		return eventID, nil
	}

	used, err := countFydeosManagedRequests(ctx, tx, userID, at)
	if err != nil {
		return "", err
	}
	if used >= FydeosManagedQuotaLimit {
		if eventID == "" {
			eventID = uuid.NewString()
			if _, err := tx.Exec(ctx, `INSERT INTO ai_usage_events(
				id,workspace_id,local_workspace_id,user_id,provider,provider_source,feature_kind,model,session_id,task_id,billing_unit_id,created_at)
				VALUES($1,$2,$3,$4,$5,'fydeos_managed_quota_denied',$6,$7,$8,$9,$9,$10)`, eventID, quotaWorkspaceID(workspaceID, personal), quotaLocalWorkspaceID(workspaceID, personal), userID, provider, featureKind, model, sessionID, billingUnitID, at); err != nil {
				return "", fmt.Errorf("record managed Copilot quota denial: %w", err)
			}
		} else if _, err := tx.Exec(ctx, `UPDATE ai_usage_events SET provider_source='fydeos_managed_quota_denied',provider=$2,feature_kind=$3,model=$4,session_id=$5,created_at=$6 WHERE id=$1`, eventID, provider, featureKind, model, sessionID, at); err != nil {
			return "", fmt.Errorf("record managed Copilot quota denial: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit managed Copilot quota denial: %w", err)
		}
		return "", ErrManagedQuotaExceeded
	}
	if eventID == "" {
		eventID = uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO ai_usage_events(
			id,workspace_id,local_workspace_id,user_id,provider,provider_source,feature_kind,model,session_id,task_id,billing_unit_id,created_at)
			VALUES($1,$2,$3,$4,$5,'fydeos_managed_quota',$6,$7,$8,$9,$9,$10)`, eventID, quotaWorkspaceID(workspaceID, personal), quotaLocalWorkspaceID(workspaceID, personal), userID, provider, featureKind, model, sessionID, billingUnitID, at); err != nil {
			return "", fmt.Errorf("reserve managed Copilot quota: %w", err)
		}
	} else if _, err := tx.Exec(ctx, `UPDATE ai_usage_events SET provider_source='fydeos_managed_quota',provider=$2,feature_kind=$3,model=$4,session_id=$5,task_id=$6,billing_unit_id=$6,created_at=$7 WHERE id=$1`, eventID, provider, featureKind, model, sessionID, billingUnitID, at); err != nil {
		return "", fmt.Errorf("reserve managed Copilot quota: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit managed Copilot quota reservation: %w", err)
	}
	return eventID, nil
}

func quotaWorkspaceID(workspaceID string, personal bool) any {
	if personal {
		return nil
	}
	return workspaceID
}

func quotaLocalWorkspaceID(workspaceID string, personal bool) any {
	if !personal {
		return nil
	}
	return workspaceID
}

func (s *Store) ReleaseFydeosManagedRequest(ctx context.Context, reservationID string) error {
	if s == nil || s.db == nil || reservationID == "" {
		return errors.New("managed Copilot quota reservation is invalid")
	}
	if _, err := s.db.Exec(ctx, `UPDATE ai_usage_events SET provider_source='fydeos_managed_quota_released'
		WHERE id=$1 AND provider_source='fydeos_managed_quota'`, reservationID); err != nil {
		return fmt.Errorf("release managed Copilot quota: %w", err)
	}
	return nil
}

func (s *Store) guardPersonalScope(ctx context.Context, tx pgx.Tx, userID, workspaceID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "copilot-personal:"+workspaceID); err != nil {
		return err
	}
	var canonical bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, workspaceID).Scan(&canonical); err != nil {
		return err
	}
	if canonical {
		return ErrPersonalScope
	}
	var foreign bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ai_sessions_metadata WHERE workspace_id=$1 AND user_id<>$2)`, workspaceID, userID).Scan(&foreign); err != nil {
		return err
	}
	if foreign {
		return ErrPersonalScope
	}
	return nil
}

func (s *Store) Create(ctx context.Context, input CreateSessionInput, personal bool) (Session, error) {
	if strings.TrimSpace(input.UserID) == "" || strings.TrimSpace(input.WorkspaceID) == "" || strings.TrimSpace(input.PromptName) == "" || len(input.PromptName) > 32 {
		return Session{}, errors.New("invalid copilot session input")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Session{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if personal {
		if err := s.guardPersonalScope(ctx, tx, input.UserID, input.WorkspaceID); err != nil {
			return Session{}, err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "copilot-session:"+input.UserID+":"+input.WorkspaceID); err != nil {
		return Session{}, err
	}
	promptAction, knownPrompt := BuiltinPromptAction(input.PromptName)
	if !knownPrompt {
		return Session{}, fmt.Errorf("Copilot prompt %q is not available", input.PromptName)
	}
	if promptAction != "" && (input.DocID == nil || *input.DocID == "" || input.Pinned) {
		return Session{}, ErrCopilotActionPromptInvalid
	}
	if input.ReuseLatestChat && promptAction == "" {
		var id string
		err := tx.QueryRow(ctx, `SELECT id FROM ai_sessions_metadata
			WHERE user_id=$1 AND workspace_id=$2 AND doc_id IS NOT DISTINCT FROM $3
			  AND parent_session_id IS NULL AND (prompt_action IS NULL OR prompt_action='')
			  AND deleted_at IS NULL ORDER BY updated_at DESC LIMIT 1`, input.UserID, input.WorkspaceID, input.DocID).Scan(&id)
		if err == nil {
			if err := tx.Commit(ctx); err != nil {
				return Session{}, err
			}
			return s.Get(ctx, input.UserID, input.WorkspaceID, id, personal)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Session{}, err
		}
	}
	if input.ManagedQuota && promptAction != "" {
		utc := time.Now().UTC()
		dayStart := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
		lockKey := "managed-copilot-quota:" + input.UserID + ":" + dayStart.Format("2006-01-02")
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
			return Session{}, fmt.Errorf("lock managed Copilot action quota: %w", err)
		}
		used, err := countFydeosManagedRequests(ctx, tx, input.UserID, utc)
		if err != nil {
			return Session{}, err
		}
		if used >= FydeosManagedQuotaLimit {
			return Session{}, ErrManagedQuotaExceeded
		}
	}
	if input.Pinned {
		if _, err := tx.Exec(ctx, `UPDATE ai_sessions_metadata SET pinned=false WHERE user_id=$1 AND workspace_id=$2 AND pinned=true AND deleted_at IS NULL`, input.UserID, input.WorkspaceID); err != nil {
			return Session{}, err
		}
	}
	id := uuid.NewString()
	var session Session
	err = tx.QueryRow(ctx, `INSERT INTO ai_sessions_metadata(id,user_id,workspace_id,doc_id,prompt_name,prompt_action,pinned)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		RETURNING id,user_id,workspace_id,doc_id,parent_session_id,prompt_name,NULLIF(prompt_action,''),pinned,title,created_at,updated_at`,
		id, input.UserID, input.WorkspaceID, input.DocID, input.PromptName, promptAction, input.Pinned).Scan(
		&session.ID, &session.UserID, &session.WorkspaceID, &session.DocID, &session.ParentSessionID,
		&session.PromptName, &session.Action, &session.Pinned, &session.Title, &session.CreatedAt, &session.UpdatedAt)
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, err
	}
	session.Messages = []Message{}
	return session, nil
}

func (s *Store) Fork(ctx context.Context, input ForkSessionInput) (string, error) {
	if strings.TrimSpace(input.UserID) == "" || strings.TrimSpace(input.WorkspaceID) == "" ||
		strings.TrimSpace(input.DocID) == "" || strings.TrimSpace(input.SessionID) == "" || input.DocID == input.WorkspaceID {
		return "", errors.New("invalid copilot fork input")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if input.Personal {
		if err := s.guardPersonalScope(ctx, tx, input.UserID, input.WorkspaceID); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "copilot-session:"+input.UserID+":"+input.WorkspaceID); err != nil {
		return "", err
	}
	var promptName, promptAction string
	var pinned bool
	var title *string
	var focus json.RawMessage
	err = tx.QueryRow(ctx, `SELECT prompt_name,COALESCE(prompt_action,''),pinned,title,focus
		FROM ai_sessions_metadata WHERE id=$1 AND user_id=$2 AND workspace_id=$3 AND deleted_at IS NULL FOR UPDATE`,
		input.SessionID, input.UserID, input.WorkspaceID).Scan(&promptName, &promptAction, &pinned, &title, &focus)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSessionNotFound
	}
	if err != nil {
		return "", err
	}
	rows, err := tx.Query(ctx, `SELECT id,role::text,content,attachments,params,scope_snapshot,"streamObjects",created_at
		FROM ai_sessions_messages WHERE session_id=$1 ORDER BY created_at ASC,id ASC`, input.SessionID)
	if err != nil {
		return "", err
	}
	type forkMessage struct {
		id, role, content                                 string
		attachments, params, scopeSnapshot, streamObjects json.RawMessage
		createdAt                                         time.Time
	}
	messages := make([]forkMessage, 0)
	hasCutoff := input.LatestMessageID != nil && *input.LatestMessageID != ""
	reachedCutoff := false
	foundCutoff := !hasCutoff
	userMessageCount := 0
	for rows.Next() {
		var message forkMessage
		if err := rows.Scan(&message.id, &message.role, &message.content, &message.attachments, &message.params, &message.scopeSnapshot, &message.streamObjects, &message.createdAt); err != nil {
			rows.Close()
			return "", err
		}
		if reachedCutoff {
			continue
		}
		messages = append(messages, message)
		if message.role == "user" {
			userMessageCount++
		}
		if hasCutoff && message.id == *input.LatestMessageID {
			if message.role != "assistant" {
				rows.Close()
				return "", ErrMessageNotFound
			}
			reachedCutoff = true
			foundCutoff = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	if !foundCutoff {
		return "", ErrMessageNotFound
	}
	if pinned {
		if _, err := tx.Exec(ctx, `UPDATE ai_sessions_metadata SET pinned=false WHERE user_id=$1 AND workspace_id=$2 AND pinned=true AND deleted_at IS NULL`, input.UserID, input.WorkspaceID); err != nil {
			return "", err
		}
	}
	forkID := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO ai_sessions_metadata
		(id,user_id,workspace_id,doc_id,prompt_name,prompt_action,pinned,title,parent_session_id,focus,"messageCost")
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		forkID, input.UserID, input.WorkspaceID, input.DocID, promptName, promptAction, pinned, title, input.SessionID, focus, userMessageCount); err != nil {
		return "", err
	}
	for _, message := range messages {
		if _, err := tx.Exec(ctx, `INSERT INTO ai_sessions_messages
			(id,session_id,role,content,attachments,params,scope_snapshot,"streamObjects",created_at,updated_at)
			VALUES($1,$2,$3::"AiPromptRole",$4,$5,$6,$7,$8,$9,now())`,
			uuid.NewString(), forkID, message.role, message.content, nullableJSON(message.attachments), nullableJSON(message.params), nullableJSON(message.scopeSnapshot), nullableJSON(message.streamObjects), message.createdAt); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return forkID, nil
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 || string(value) == "null" {
		return nil
	}
	return json.RawMessage(value)
}

func (s *Store) Get(ctx context.Context, userID, workspaceID, sessionID string, personal bool) (Session, error) {
	if personal {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return Session{}, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := s.guardPersonalScope(ctx, tx, userID, workspaceID); err != nil {
			return Session{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Session{}, err
		}
	}
	session, err := scanSession(s.db.QueryRow(ctx, `SELECT id,user_id,workspace_id,doc_id,parent_session_id,prompt_name,NULLIF(prompt_action,''),pinned,title,created_at,updated_at
		FROM ai_sessions_metadata WHERE id=$1 AND user_id=$2 AND workspace_id=$3 AND deleted_at IS NULL`, sessionID, userID, workspaceID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, err
	}
	messages, err := s.messages(ctx, []string{sessionID}, "asc")
	if err != nil {
		return Session{}, err
	}
	session.Messages = messages[sessionID]
	if session.Messages == nil {
		session.Messages = []Message{}
	}
	return session, nil
}

func (s *Store) GetScope(ctx context.Context, userID, sessionID string) (string, *string, error) {
	var workspaceID string
	var docID *string
	err := s.db.QueryRow(ctx, `SELECT workspace_id,doc_id FROM ai_sessions_metadata WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL`, sessionID, userID).Scan(&workspaceID, &docID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrSessionNotFound
	}
	return workspaceID, docID, err
}

func (s *Store) CreateUserMessage(ctx context.Context, input CreateUserMessageInput) (string, error) {
	if strings.TrimSpace(input.UserID) == "" || strings.TrimSpace(input.SessionID) == "" || len(input.Attachments) > maxCopilotAttachmentsPerMessage || len(input.ArtifactAttachments) > len(input.Attachments) {
		return "", errors.New("invalid copilot message input")
	}
	content := ""
	if input.Content != nil {
		content = *input.Content
	}
	if len(content) > 1<<20 {
		return "", errors.New("copilot message is too large")
	}
	attachments, err := json.Marshal(input.Attachments)
	if err != nil {
		return "", err
	}
	var params any
	if len(input.Params) > 0 && string(input.Params) != "null" {
		if !json.Valid(input.Params) {
			return "", errors.New("invalid copilot message params")
		}
		params = input.Params
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "copilot-message:"+input.UserID+":"+input.SessionID); err != nil {
		return "", err
	}
	var workspaceID string
	var action string
	err = tx.QueryRow(ctx, `SELECT workspace_id,COALESCE(prompt_action,'') FROM ai_sessions_metadata
		WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL FOR UPDATE`, input.SessionID, input.UserID).Scan(&workspaceID, &action)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSessionNotFound
	}
	if err != nil {
		return "", err
	}
	if action != "" {
		var hasUserMessage bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ai_sessions_messages WHERE session_id=$1 AND role='user')`, input.SessionID).Scan(&hasUserMessage); err != nil {
			return "", err
		}
		if hasUserMessage {
			return "", errors.New("copilot action has already been used")
		}
	}
	messageID := uuid.NewString()
	artifactIDs := make([]string, 0, len(input.ArtifactAttachments))
	seenArtifactIDs := make(map[string]struct{}, len(input.ArtifactAttachments))
	for _, attachment := range input.ArtifactAttachments {
		if !validAttachmentKey(attachment.ContentHash) || attachment.SizeBytes < 1 || attachment.SizeBytes > maxSessionAttachmentBytes || attachment.MediaType == "" {
			return "", errors.New("invalid Copilot message artifact")
		}
		objectKey := input.UserID + "/" + workspaceID + "/" + attachment.ContentHash
		var artifactID string
		err := tx.QueryRow(ctx, `INSERT INTO workspace_artifacts(
			id,workspace_id,content_hash,file_name,canonical_media_type,size_bytes,storage_scope,storage_key,status,
			library_owned,extracted_text_runes,ready_at,created_at,updated_at)
			VALUES(gen_random_uuid(),$1,$2,NULLIF($3,''),$4,$5,'copilot',$6,'ready',false,$7,clock_timestamp(),clock_timestamp(),clock_timestamp())
			ON CONFLICT(workspace_id,content_hash) DO UPDATE SET
				file_name=coalesce(workspace_artifacts.file_name,EXCLUDED.file_name),
				canonical_media_type=EXCLUDED.canonical_media_type,size_bytes=EXCLUDED.size_bytes,
				storage_scope=EXCLUDED.storage_scope,storage_key=EXCLUDED.storage_key,status='ready',
				extracted_text_runes=coalesce(EXCLUDED.extracted_text_runes,workspace_artifacts.extracted_text_runes),
				ready_at=coalesce(workspace_artifacts.ready_at,clock_timestamp()),reservation_expires_at=NULL,updated_at=clock_timestamp()
			WHERE workspace_artifacts.status<>'deleting'
			RETURNING id::text`, workspaceID, attachment.ContentHash, attachment.FileName, attachment.MediaType,
			attachment.SizeBytes, objectKey, attachment.ExtractedTextRunes).Scan(&artifactID)
		if err != nil {
			return "", fmt.Errorf("register Copilot message artifact: %w", err)
		}
		if _, seen := seenArtifactIDs[artifactID]; !seen {
			seenArtifactIDs[artifactID] = struct{}{}
			artifactIDs = append(artifactIDs, artifactID)
		}
	}
	var scopeSnapshot any
	if len(artifactIDs) > 0 {
		scopeSnapshot, err = json.Marshal(map[string]any{
			"version":   1,
			"retrieval": map[string]any{"mode": "required", "requiredArtifactIds": artifactIDs},
		})
		if err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ai_sessions_messages(id,session_id,role,content,attachments,params,scope_snapshot,created_at,updated_at)
		VALUES($1,$2,'user',$3,$4,$5,$6,now(),now())`, messageID, input.SessionID, content, attachments, params, scopeSnapshot); err != nil {
		return "", err
	}
	for _, artifactID := range artifactIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO ai_message_artifacts(message_id,workspace_id,artifact_id,role,created_at)
			VALUES($1,$2,$3,'attachment',clock_timestamp())`, messageID, workspaceID, artifactID); err != nil {
			return "", fmt.Errorf("link Copilot message artifact: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE ai_sessions_metadata SET "messageCost"=COALESCE("messageCost",0)+1,updated_at=now()
		WHERE id=$1 AND user_id=$2`, input.SessionID, input.UserID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return messageID, nil
}

func scanSession(row pgx.Row) (Session, error) {
	var session Session
	err := row.Scan(&session.ID, &session.UserID, &session.WorkspaceID, &session.DocID,
		&session.ParentSessionID, &session.PromptName, &session.Action, &session.Pinned,
		&session.Title, &session.CreatedAt, &session.UpdatedAt)
	return session, err
}

func (s *Store) List(ctx context.Context, input ListSessionsInput, personal bool) (SessionPage, error) {
	if input.Limit < 1 {
		input.Limit = 10
	}
	if input.Limit > 100 {
		input.Limit = 100
	}
	if input.Offset < 0 {
		input.Offset = 0
	}
	if input.Order != "asc" {
		input.Order = "desc"
	}
	if input.MessageOrder != "desc" {
		input.MessageOrder = "asc"
	}
	if personal {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return SessionPage{}, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := s.guardPersonalScope(ctx, tx, input.UserID, input.WorkspaceID); err != nil {
			return SessionPage{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return SessionPage{}, err
		}
	}
	args := []any{input.UserID, input.WorkspaceID, input.DocID != nil, input.DocID, input.SessionID != nil, input.SessionID, input.Pinned != nil, input.Pinned, input.Action != nil, input.Action, input.Fork != nil, input.Fork}
	where := `user_id=$1 AND workspace_id=$2 AND deleted_at IS NULL
		AND (NOT $3 OR doc_id IS NOT DISTINCT FROM $4)
		AND (NOT $5 OR id=$6)
		AND (NOT $7 OR pinned=$8)
		AND (NOT $9 OR (CASE WHEN $10 THEN prompt_action IS NOT NULL AND prompt_action<>'' ELSE prompt_action IS NULL OR prompt_action='' END))
		AND (NOT $11 OR (CASE WHEN $12 THEN parent_session_id IS NOT NULL ELSE parent_session_id IS NULL END))`
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM ai_sessions_metadata WHERE `+where, args...).Scan(&total); err != nil {
		return SessionPage{}, err
	}
	args = append(args, input.Limit, input.Offset)
	query := `SELECT id,user_id,workspace_id,doc_id,parent_session_id,prompt_name,NULLIF(prompt_action,''),pinned,title,created_at,updated_at FROM ai_sessions_metadata WHERE ` + where + ` ORDER BY updated_at ` + input.Order + `, id ` + input.Order + ` LIMIT $13 OFFSET $14`
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return SessionPage{}, err
	}
	sessions := []Session{}
	ids := []string{}
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			rows.Close()
			return SessionPage{}, err
		}
		sessions = append(sessions, session)
		ids = append(ids, session.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SessionPage{}, err
	}
	rows.Close()
	if input.WithMessages && len(ids) > 0 {
		messageMap, err := s.messages(ctx, ids, input.MessageOrder)
		if err != nil {
			return SessionPage{}, err
		}
		for i := range sessions {
			sessions[i].Messages = messageMap[sessions[i].ID]
			if sessions[i].Messages == nil {
				sessions[i].Messages = []Message{}
			}
		}
	} else {
		for i := range sessions {
			sessions[i].Messages = []Message{}
		}
	}
	return SessionPage{Sessions: sessions, TotalCount: total}, nil
}

func (s *Store) messages(ctx context.Context, sessionIDs []string, order string) (map[string][]Message, error) {
	if order != "desc" {
		order = "asc"
	}
	rows, err := s.db.Query(ctx, `SELECT id,session_id,role::text,content,attachments,"streamObjects",scope_snapshot,created_at
		FROM ai_sessions_messages WHERE session_id=ANY($1::text[]) ORDER BY created_at `+order+`,id `+order, sessionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string][]Message{}
	for rows.Next() {
		var message Message
		var sessionID string
		if err := rows.Scan(&message.ID, &sessionID, &message.Role, &message.Content, &message.Attachments, &message.StreamObjects, &message.ScopeSnapshot, &message.CreatedAt); err != nil {
			return nil, err
		}
		if len(message.Attachments) == 0 {
			message.Attachments = json.RawMessage("[]")
		}
		if len(message.StreamObjects) == 0 {
			message.StreamObjects = json.RawMessage("[]")
		}
		result[sessionID] = append(result[sessionID], message)
	}
	return result, rows.Err()
}

func (s *Store) Update(ctx context.Context, input UpdateSessionInput) (string, error) {
	if input.PromptName != nil && (strings.TrimSpace(*input.PromptName) == "" || len(*input.PromptName) > 32) {
		return "", errors.New("invalid prompt name")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if input.Personal {
		if err := s.guardPersonalScope(ctx, tx, input.UserID, input.WorkspaceID); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "copilot-session:"+input.UserID+":"+input.WorkspaceID); err != nil {
		return "", err
	}
	if input.Pinned != nil && *input.Pinned {
		if _, err := tx.Exec(ctx, `UPDATE ai_sessions_metadata SET pinned=false WHERE user_id=$1 AND workspace_id=$2 AND pinned=true AND deleted_at IS NULL`, input.UserID, input.WorkspaceID); err != nil {
			return "", err
		}
	}
	var docID any
	if input.DocID != nil {
		docID = *input.DocID
	}
	var pinned any
	if input.Pinned != nil {
		pinned = *input.Pinned
	}
	var promptName any
	if input.PromptName != nil {
		promptName = *input.PromptName
	}
	var promptAction any
	if input.PromptName != nil {
		action, known := BuiltinPromptAction(*input.PromptName)
		if !known {
			return "", fmt.Errorf("Copilot prompt %q is not available", *input.PromptName)
		}
		var docID string
		var pinned bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(doc_id,''),pinned FROM ai_sessions_metadata WHERE id=$1 AND user_id=$2 AND workspace_id=$3 AND deleted_at IS NULL FOR UPDATE`, input.SessionID, input.UserID, input.WorkspaceID).Scan(&docID, &pinned); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", ErrSessionNotFound
			}
			return "", err
		}
		if input.SetDocID {
			docID = ""
			if input.DocID != nil {
				docID = *input.DocID
			}
		}
		if input.Pinned != nil {
			pinned = *input.Pinned
		}
		if action != "" && (docID == "" || pinned) {
			return "", ErrCopilotActionPromptInvalid
		}
		promptAction = action
	}
	var id string
	err = tx.QueryRow(ctx, `UPDATE ai_sessions_metadata SET
		doc_id=CASE WHEN $4 THEN $5 ELSE doc_id END,
		pinned=coalesce($6::boolean,pinned),
		prompt_name=coalesce($7::varchar,prompt_name),
		prompt_action=coalesce($8::varchar,prompt_action),
		updated_at=now()
		WHERE id=$1 AND user_id=$2 AND workspace_id=$3 AND deleted_at IS NULL
		RETURNING id`, input.SessionID, input.UserID, input.WorkspaceID, input.SetDocID, docID, pinned, promptName, promptAction).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSessionNotFound
	}
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) Cleanup(ctx context.Context, userID, workspaceID string, sessionIDs []string, personal bool) ([]string, error) {
	if len(sessionIDs) == 0 {
		return nil, ErrSessionNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if personal {
		if err := s.guardPersonalScope(ctx, tx, userID, workspaceID); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `UPDATE ai_sessions_metadata SET deleted_at=now(),updated_at=now()
		WHERE user_id=$1 AND workspace_id=$2 AND id=ANY($3::text[]) AND deleted_at IS NULL RETURNING id`, userID, workspaceID, sessionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deleted := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		deleted = append(deleted, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(deleted) == 0 {
		return nil, ErrSessionNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return deleted, nil
}
