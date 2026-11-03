// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/maildelivery"
)

var ErrSMTPTestEmailUnavailable = errors.New("SMTP test email service is unavailable")

func (r *Resolver) sendTestEmail(ctx context.Context, input model.JSONObject) (bool, error) {
	if err := requireAdministrator(ctx); err != nil {
		return false, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return false, ErrAdminAccessDenied
	}
	if r.SMTPTestEmailSender == nil {
		return false, ErrSMTPTestEmailUnavailable
	}

	settings, err := smtpTestConfig(input)
	if err != nil {
		return false, &maildelivery.SMTPTestError{Phase: "verify", Cause: err}
	}
	if err := r.SMTPTestEmailSender.SendTestEmail(ctx, settings, principal.User.Email); err != nil {
		return false, err
	}
	return true, nil
}

func smtpTestConfig(input model.JSONObject) (maildelivery.SMTPConfig, error) {
	var values struct {
		Name      string `json:"name"`
		Host      string `json:"host"`
		Port      int    `json:"port"`
		Sender    string `json:"sender"`
		Username  string `json:"username"`
		Password  string `json:"password"`
		Secure    bool   `json:"secure"`
		IgnoreTLS bool   `json:"ignoreTLS"`
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return maildelivery.SMTPConfig{}, errors.New("invalid SMTP configuration")
	}
	if err := json.Unmarshal(encoded, &values); err != nil {
		return maildelivery.SMTPConfig{}, errors.New("invalid SMTP configuration")
	}
	return maildelivery.SMTPConfig{
		Name:      values.Name,
		Host:      values.Host,
		Port:      values.Port,
		Sender:    values.Sender,
		Username:  values.Username,
		Password:  values.Password,
		Secure:    values.Secure,
		IgnoreTLS: values.IgnoreTLS,
	}, nil
}
