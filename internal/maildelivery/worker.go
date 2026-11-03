// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/google/uuid"
)

type Store interface {
	ClaimReady(context.Context, string, int, time.Duration) ([]Delivery, error)
	StartAttempt(context.Context, string, string) (Attempt, bool, error)
	MarkSent(context.Context, string, string, DeliveryResult) error
	MarkSkipped(context.Context, string, string, string) error
	MarkFailed(context.Context, string, string, string, string) error
	MarkRetry(context.Context, string, string, time.Time, DeliveryResult) (bool, error)
	WorkspaceName(context.Context, string) (string, bool, error)
	UserEmail(context.Context, string) (string, bool, error)
	RecipientDisabled(context.Context, string) (bool, error)
	DeleteAnonymizedBefore(context.Context, time.Time, int) (int64, error)
}

type Worker struct {
	store     Store
	sender    Sender
	workerID  string
	batchSize int
	lease     time.Duration
	interval  time.Duration
	retention time.Duration
	now       func() time.Time
}

func NewWorker(store Store, sender Sender, settings Settings) (*Worker, error) {
	if store == nil || sender == nil || settings.BatchSize < 1 || settings.BatchSize > 1000 || settings.Lease < time.Second || settings.Retention < time.Second {
		return nil, errors.New("valid mail delivery store, sender, and settings are required")
	}
	return &Worker{store: store, sender: sender, workerID: "mail-delivery-" + uuid.NewString(), batchSize: settings.BatchSize,
		lease: settings.Lease, interval: time.Minute, retention: settings.Retention, now: time.Now}, nil
}

func (w *Worker) ProcessReady(ctx context.Context) (int, error) {
	deliveries, err := w.store.ClaimReady(ctx, w.workerID, w.batchSize, w.lease)
	if err != nil {
		return 0, err
	}
	var failures []error
	for _, delivery := range deliveries {
		if err := w.process(ctx, delivery); err != nil {
			failures = append(failures, fmt.Errorf("process mail delivery %s: %w", delivery.ID, err))
		}
	}
	if _, err := w.store.DeleteAnonymizedBefore(ctx, w.now().Add(-w.retention), w.batchSize); err != nil {
		failures = append(failures, fmt.Errorf("clean retained mail deliveries: %w", err))
	}
	return len(deliveries), errors.Join(failures...)
}

func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return errors.New("mail delivery worker is required")
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if _, err := w.ProcessReady(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.ErrorContext(ctx, "mail delivery batch failed", "error", redact(err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *Worker) process(ctx context.Context, delivery Delivery) error {
	if delivery.Recipient == nil || *delivery.Recipient == "" || len(delivery.Payload) == 0 {
		return w.store.MarkSkipped(ctx, delivery.ID, w.workerID, "missing_payload")
	}
	now := w.now()
	if delivery.ExpiresAt != nil && !delivery.ExpiresAt.After(now) {
		return w.store.MarkFailed(ctx, delivery.ID, w.workerID, "expired", "")
	}
	disabled, err := w.store.RecipientDisabled(ctx, *delivery.Recipient)
	if err != nil {
		return err
	}
	if disabled {
		return w.store.MarkSkipped(ctx, delivery.ID, w.workerID, "disabled_recipient")
	}
	var payload struct {
		Name  string         `json:"name"`
		Props map[string]any `json:"props"`
	}
	if err := json.Unmarshal(delivery.Payload, &payload); err != nil || payload.Name == "" || payload.Name != delivery.MailName {
		return w.store.MarkSkipped(ctx, delivery.ID, w.workerID, "invalid_payload")
	}
	workspaceName := ""
	actorEmail := ""
	actorName := ""
	if payload.Name == "MemberRemoved" || payload.Name == "MemberLeave" || payload.Name == "MemberInvitation" || payload.Name == "LinkInvitationApprove" || payload.Name == "LinkInvitationReviewRequest" || payload.Name == "LinkInvitationDecline" {
		workspace, ok := payload.Props["workspace"].(map[string]any)
		workspaceID, valid := workspace["$$workspaceId"].(string)
		if !ok || !valid || workspaceID == "" {
			return w.store.MarkSkipped(ctx, delivery.ID, w.workerID, "dynamic_props_missing")
		}
		var exists bool
		workspaceName, exists, err = w.store.WorkspaceName(ctx, workspaceID)
		if err != nil {
			return err
		}
		if !exists {
			return w.store.MarkSkipped(ctx, delivery.ID, w.workerID, "dynamic_props_missing")
		}
		if payload.Name == "MemberInvitation" && invitelinks.ContainsURLOrDomain(workspaceName) {
			return w.store.MarkSkipped(ctx, delivery.ID, w.workerID, "workspace_name_contains_domain")
		}
	}
	if payload.Name == "MemberLeave" || payload.Name == "LinkInvitationReviewRequest" || payload.Name == "MemberInvitation" {
		user, ok := payload.Props["user"].(map[string]any)
		userID, valid := user["$$userId"].(string)
		if !ok || !valid || userID == "" {
			return w.store.MarkSkipped(ctx, delivery.ID, w.workerID, "dynamic_props_missing")
		}
		var exists bool
		if payload.Name == "MemberInvitation" {
			reader, ok := w.store.(interface {
				UserDisplayName(context.Context, string) (string, bool, error)
			})
			if !ok {
				return w.store.MarkSkipped(ctx, delivery.ID, w.workerID, "dynamic_props_missing")
			}
			actorName, exists, err = reader.UserDisplayName(ctx, userID)
		} else {
			actorEmail, exists, err = w.store.UserEmail(ctx, userID)
		}
		if err != nil {
			return err
		}
		if !exists {
			return w.store.MarkSkipped(ctx, delivery.ID, w.workerID, "dynamic_props_missing")
		}
	}
	if payload.Name == "MemberInvitation" {
		if url, _ := payload.Props["url"].(string); url == "" {
			return w.store.MarkSkipped(ctx, delivery.ID, w.workerID, "dynamic_props_missing")
		}
	}
	message, ok, err := renderMessage(payload.Name, *delivery.Recipient, payload.Props, workspaceName, actorEmail, actorName)
	if err != nil {
		return w.store.MarkFailed(ctx, delivery.ID, w.workerID, "render_failed", err.Error())
	}
	if !ok {
		return w.store.MarkSkipped(ctx, delivery.ID, w.workerID, "dynamic_props_missing")
	}
	attempt, started, err := w.store.StartAttempt(ctx, delivery.ID, w.workerID)
	if err != nil || !started {
		return err
	}
	result := w.sender.Send(ctx, message)
	if result.Status == SendAccepted {
		return w.store.MarkSent(ctx, delivery.ID, w.workerID, DeliveryResult{
			ProviderMessageID: result.ProviderMessageID, ProviderResponse: result.ProviderResponse,
		})
	}
	deliveryResult := DeliveryResult{ErrorCode: result.ErrorCode, Error: result.Error}
	if result.Retryable && attempt.Count < attempt.Max {
		delay := time.Duration(math.Min(float64(30*time.Minute), math.Pow(2, float64(attempt.Count))*float64(30*time.Second)))
		next := w.now().Add(delay)
		if attempt.ExpiresAt == nil || next.Before(*attempt.ExpiresAt) {
			_, retryErr := w.store.MarkRetry(ctx, delivery.ID, w.workerID, next, deliveryResult)
			return retryErr
		}
	}
	reason := result.ErrorCode
	if reason == "" {
		reason = string(result.Status)
	}
	return w.store.MarkFailed(ctx, delivery.ID, w.workerID, reason, result.Error)
}
