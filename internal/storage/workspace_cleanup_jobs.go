// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const workspaceCleanupLease = 5 * time.Minute

var errWorkspaceCleanupLeaseLost = errors.New("workspace storage cleanup lease lost")

type workspaceCleanupJob struct {
	workspaceID string
	userIDs     []string
	leaseToken  string
}

func (r *Runtime) CleanupPendingWorkspaceStorage(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("workspace storage cleanup limit must be between 1 and 1000")
	}
	var attempted int
	var failures []error
	for attempted < limit {
		jobs, err := r.claimWorkspaceCleanupJobs(ctx, 1)
		if err != nil {
			failures = append(failures, err)
			break
		}
		if len(jobs) == 0 {
			break
		}
		job := jobs[0]
		attempted++
		if _, err := r.deleteWorkspaceObjectsWithLease(ctx, job); err != nil {
			failures = append(failures, fmt.Errorf("clean workspace %s objects: %w", job.workspaceID, err))
			if updateErr := r.retryWorkspaceCleanupJob(ctx, job, "storage_cleanup_failed"); updateErr != nil {
				failures = append(failures, fmt.Errorf("schedule workspace %s cleanup retry: %w", job.workspaceID, updateErr))
			}
			continue
		}
		var stillQueued bool
		if err := r.db.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM workspace_storage_cleanup_jobs WHERE workspace_id=$1 AND lease_token=$2
		)`, job.workspaceID, job.leaseToken).Scan(&stillQueued); err != nil {
			failures = append(failures, fmt.Errorf("verify workspace %s cleanup task: %w", job.workspaceID, err))
			continue
		}
		if stillQueued {
			if err := r.deferWorkspaceCleanupJob(ctx, job); err != nil {
				failures = append(failures, fmt.Errorf("defer workspace %s cleanup task: %w", job.workspaceID, err))
			}
		}
	}
	return attempted, errors.Join(failures...)
}

func (r *Runtime) deleteWorkspaceObjectsWithLease(ctx context.Context, job workspaceCleanupJob) (int64, error) {
	leaseCtx, cancel := context.WithCancel(ctx)
	stopRenewal := make(chan struct{})
	renewalDone := make(chan struct{})
	var renewalErr error
	go func() {
		defer close(renewalDone)
		interval := r.cleanupLeaseDuration() / 3
		if interval <= 0 {
			interval = workspaceCleanupLease / 3
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopRenewal:
				return
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				if err := r.renewWorkspaceCleanupLease(leaseCtx, job); err != nil {
					renewalErr = err
					cancel()
					return
				}
			}
		}
	}()

	deleted, deleteErr := r.deleteWorkspaceObjects(leaseCtx, job.workspaceID, job.userIDs, false)
	close(stopRenewal)
	<-renewalDone
	cancel()
	if deleteErr != nil || renewalErr != nil {
		return deleted, errors.Join(deleteErr, renewalErr)
	}
	if err := r.completeWorkspaceCleanupJob(ctx, job); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func (r *Runtime) cleanupLeaseDuration() time.Duration {
	if r.workspaceCleanupLeaseDuration > 0 {
		return r.workspaceCleanupLeaseDuration
	}
	return workspaceCleanupLease
}

func (r *Runtime) renewWorkspaceCleanupLease(ctx context.Context, job workspaceCleanupJob) error {
	tag, err := r.db.Exec(ctx, `UPDATE workspace_storage_cleanup_jobs
		SET lease_until=clock_timestamp()+make_interval(secs=>$3),updated_at=clock_timestamp()
		WHERE workspace_id=$1 AND lease_token=$2 AND lease_until>clock_timestamp()`,
		job.workspaceID, job.leaseToken, r.cleanupLeaseDuration().Seconds())
	if err != nil {
		return fmt.Errorf("renew workspace cleanup lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errWorkspaceCleanupLeaseLost
	}
	return nil
}

func (r *Runtime) completeWorkspaceCleanupJob(ctx context.Context, job workspaceCleanupJob) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM workspace_storage_cleanup_jobs
		WHERE workspace_id=$1 AND lease_token=$2`, job.workspaceID, job.leaseToken)
	if err != nil {
		return fmt.Errorf("complete workspace storage cleanup task: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errWorkspaceCleanupLeaseLost
	}
	return nil
}

func (r *Runtime) claimWorkspaceCleanupJobs(ctx context.Context, limit int) ([]workspaceCleanupJob, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin workspace cleanup claim: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `
		SELECT workspace_id,user_ids FROM workspace_storage_cleanup_jobs
		WHERE available_at<=clock_timestamp()
		  AND (lease_until IS NULL OR lease_until<=clock_timestamp())
		ORDER BY available_at,workspace_id
		LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("select workspace cleanup tasks: %w", err)
	}
	jobs := make([]workspaceCleanupJob, 0, limit)
	for rows.Next() {
		var job workspaceCleanupJob
		if err := rows.Scan(&job.workspaceID, &job.userIDs); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan workspace cleanup task: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read workspace cleanup tasks: %w", err)
	}
	rows.Close()
	for i := range jobs {
		jobs[i].leaseToken = uuid.NewString()
		if _, err := tx.Exec(ctx, `UPDATE workspace_storage_cleanup_jobs
			SET lease_token=$2,lease_until=clock_timestamp()+make_interval(secs=>$3),updated_at=clock_timestamp()
			WHERE workspace_id=$1`, jobs[i].workspaceID, jobs[i].leaseToken, r.cleanupLeaseDuration().Seconds()); err != nil {
			return nil, fmt.Errorf("lease workspace cleanup task: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit workspace cleanup claim: %w", err)
	}
	return jobs, nil
}

func (r *Runtime) retryWorkspaceCleanupJob(ctx context.Context, job workspaceCleanupJob, reason string) error {
	_, err := r.db.Exec(ctx, `UPDATE workspace_storage_cleanup_jobs
		SET attempt=attempt+1,
		    available_at=clock_timestamp()+make_interval(secs=>CASE WHEN attempt>=11 THEN 3600 ELSE (1 << attempt) END),
		    lease_token=NULL,lease_until=NULL,last_error=$3,updated_at=clock_timestamp()
		WHERE workspace_id=$1 AND lease_token=$2`, job.workspaceID, job.leaseToken, reason)
	return err
}

func (r *Runtime) deferWorkspaceCleanupJob(ctx context.Context, job workspaceCleanupJob) error {
	_, err := r.db.Exec(ctx, `UPDATE workspace_storage_cleanup_jobs
		SET available_at=clock_timestamp()+interval '5 minutes',lease_token=NULL,lease_until=NULL,
		    updated_at=clock_timestamp()
		WHERE workspace_id=$1 AND lease_token=$2`, job.workspaceID, job.leaseToken)
	return err
}
