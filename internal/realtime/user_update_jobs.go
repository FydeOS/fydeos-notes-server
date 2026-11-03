// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type UserUpdateJobState struct {
	Grants           bool
	AfterWorkspaceID string
	AfterGrant       *workspaces.DocGrantRef
}

type UserUpdateJob struct {
	UserID     string
	Generation int64
	LeaseToken string
	Attempt    uint
	State      UserUpdateJobState
}

type UserUpdateRefreshJobStore interface {
	MarkDirty(context.Context, string) error
	Claim(context.Context, time.Duration) (UserUpdateJob, bool, error)
	Renew(context.Context, UserUpdateJob, time.Duration) (bool, error)
	Advance(context.Context, UserUpdateJob, UserUpdateJobState) error
	Retry(context.Context, UserUpdateJob, time.Duration) error
	Complete(context.Context, UserUpdateJob) error
}

type userUpdateJobDB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type PostgresUserUpdateRefreshJobStore struct {
	db userUpdateJobDB
}

func NewPostgresUserUpdateRefreshJobStore(db userUpdateJobDB) *PostgresUserUpdateRefreshJobStore {
	return &PostgresUserUpdateRefreshJobStore{db: db}
}

func (s *PostgresUserUpdateRefreshJobStore) MarkDirty(ctx context.Context, userID string) error {
	if s == nil || s.db == nil {
		return errors.New("user update refresh job store is unavailable")
	}
	if userID == "" {
		return errors.New("user update refresh job user ID is required")
	}
	_, err := s.db.Exec(ctx, `SELECT go_enqueue_user_update_refresh($1)`, userID)
	if err != nil {
		return fmt.Errorf("mark user update refresh job dirty: %w", err)
	}
	return nil
}

func (s *PostgresUserUpdateRefreshJobStore) Claim(ctx context.Context, lease time.Duration) (UserUpdateJob, bool, error) {
	if s == nil || s.db == nil {
		return UserUpdateJob{}, false, errors.New("user update refresh job store is unavailable")
	}
	if lease <= 0 {
		return UserUpdateJob{}, false, errors.New("user update refresh job lease must be positive")
	}
	token := uuid.NewString()
	var job UserUpdateJob
	var phase int16
	var afterWorkspace, afterGrantWorkspace, afterGrantDoc pgtype.Text
	var attempt int32
	err := s.db.QueryRow(ctx, `
		WITH candidate AS (
			SELECT user_id
			FROM go_user_update_refresh_jobs
			WHERE available_at <= clock_timestamp()
			  AND (lease_until IS NULL OR lease_until <= clock_timestamp())
			ORDER BY available_at, user_id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE go_user_update_refresh_jobs AS job
		SET lease_token=$1, lease_until=clock_timestamp() + ($2::bigint * interval '1 millisecond')
		FROM candidate
		WHERE job.user_id=candidate.user_id
		RETURNING job.user_id, job.generation, job.phase, job.after_workspace_id,
		          job.after_grant_workspace_id, job.after_grant_doc_id, job.attempt`,
		token, lease.Milliseconds()).Scan(
		&job.UserID, &job.Generation, &phase, &afterWorkspace,
		&afterGrantWorkspace, &afterGrantDoc, &attempt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserUpdateJob{}, false, nil
	}
	if err != nil {
		return UserUpdateJob{}, false, fmt.Errorf("claim user update refresh job: %w", err)
	}
	job.LeaseToken = token
	job.Attempt = uint(max(attempt, 0))
	job.State.Grants = phase == 1
	if afterWorkspace.Valid {
		job.State.AfterWorkspaceID = afterWorkspace.String
	}
	if afterGrantWorkspace.Valid && afterGrantDoc.Valid {
		job.State.AfterGrant = &workspaces.DocGrantRef{WorkspaceID: afterGrantWorkspace.String, DocID: afterGrantDoc.String}
	}
	return job, true, nil
}

func (s *PostgresUserUpdateRefreshJobStore) Advance(ctx context.Context, job UserUpdateJob, state UserUpdateJobState) error {
	var afterGrantWorkspace, afterGrantDoc any
	if state.AfterGrant != nil {
		afterGrantWorkspace = state.AfterGrant.WorkspaceID
		afterGrantDoc = state.AfterGrant.DocID
	}
	phase := 0
	if state.Grants {
		phase = 1
	}
	_, err := s.db.Exec(ctx, `
		UPDATE go_user_update_refresh_jobs
		SET phase=$4, after_workspace_id=NULLIF($5,''),
		    after_grant_workspace_id=$6, after_grant_doc_id=$7,
		    lease_token=NULL, lease_until=NULL, attempt=0,
		    available_at=clock_timestamp(), updated_at=clock_timestamp()
		WHERE user_id=$1 AND generation=$2 AND lease_token=$3`,
		job.UserID, job.Generation, job.LeaseToken, phase, state.AfterWorkspaceID, afterGrantWorkspace, afterGrantDoc)
	if err != nil {
		return fmt.Errorf("advance user update refresh job: %w", err)
	}
	return nil
}

func (s *PostgresUserUpdateRefreshJobStore) Renew(ctx context.Context, job UserUpdateJob, lease time.Duration) (bool, error) {
	if lease <= 0 {
		return false, errors.New("user update refresh job lease must be positive")
	}
	result, err := s.db.Exec(ctx, `
		UPDATE go_user_update_refresh_jobs
		SET lease_until=clock_timestamp() + ($4::bigint * interval '1 millisecond'), updated_at=clock_timestamp()
		WHERE user_id=$1 AND generation=$2 AND lease_token=$3`,
		job.UserID, job.Generation, job.LeaseToken, lease.Milliseconds())
	if err != nil {
		return false, fmt.Errorf("renew user update refresh lease: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *PostgresUserUpdateRefreshJobStore) Retry(ctx context.Context, job UserUpdateJob, delay time.Duration) error {
	if delay < 0 {
		delay = 0
	}
	_, err := s.db.Exec(ctx, `
		UPDATE go_user_update_refresh_jobs
		SET attempt=attempt+1, available_at=clock_timestamp() + ($4::bigint * interval '1 millisecond'),
		    lease_token=NULL, lease_until=NULL, updated_at=clock_timestamp()
		WHERE user_id=$1 AND generation=$2 AND lease_token=$3`,
		job.UserID, job.Generation, job.LeaseToken, delay.Milliseconds())
	if err != nil {
		return fmt.Errorf("schedule user update refresh retry: %w", err)
	}
	return nil
}

func (s *PostgresUserUpdateRefreshJobStore) Complete(ctx context.Context, job UserUpdateJob) error {
	_, err := s.db.Exec(ctx, `DELETE FROM go_user_update_refresh_jobs WHERE user_id=$1 AND generation=$2 AND lease_token=$3`, job.UserID, job.Generation, job.LeaseToken)
	if err != nil {
		return fmt.Errorf("complete user update refresh job: %w", err)
	}
	return nil
}
