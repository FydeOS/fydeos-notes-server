// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type SMTPTestError struct {
	Phase string
	Cause error
}

func (e *SMTPTestError) Error() string {
	if e == nil || e.Cause == nil {
		return "SMTP test email failed"
	}
	return e.Cause.Error()
}

func (e *SMTPTestError) Unwrap() error {
	return e.Cause
}

type TestEmailSender struct {
	timeout time.Duration
}

func NewTestEmailSender() *TestEmailSender {
	return &TestEmailSender{timeout: 30 * time.Second}
}

func (s *TestEmailSender) SendTestEmail(ctx context.Context, settings SMTPConfig, recipient string) error {
	if settings.Host == "" {
		settings.Host = "localhost"
	}
	if settings.Port == 0 {
		settings.Port = 587
	}
	timeout := 30 * time.Second
	if s != nil && s.timeout > 0 {
		timeout = s.timeout
	}
	if err := verifySMTP(ctx, settings, timeout); err != nil {
		return &SMTPTestError{Phase: "verify", Cause: err}
	}

	result := NewSMTPSender(Settings{SMTP: settings, Lease: timeout + time.Second}).Send(ctx, Message{
		Name:    "TestMail",
		To:      recipient,
		Subject: "Test Email from FydeOS Notes",
		HTML:    `<h1>Test Email from FydeOS Notes</h1><p>This is a test email from your FydeOS Notes instance.</p>`,
	})
	if result.Status != SendAccepted {
		message := result.Error
		if message == "" {
			message = result.ErrorCode
		}
		if message == "" {
			message = "SMTP server did not accept the test email"
		}
		return &SMTPTestError{Phase: "send", Cause: errors.New(message)}
	}
	return nil
}

func verifySMTP(ctx context.Context, settings SMTPConfig, timeout time.Duration) error {
	if settings.Host == "" {
		settings.Host = "localhost"
	}
	if settings.Port == 0 {
		settings.Port = 587
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client, err := newSMTPClient(ctx, settings, timeout)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Noop(); err != nil {
		return fmt.Errorf("SMTP verification failed: %w", err)
	}
	return nil
}
