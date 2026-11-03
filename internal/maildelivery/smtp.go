// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"
)

type Message struct {
	Name    string
	To      string
	Subject string
	HTML    string
}

type SendStatus string

const (
	SendAccepted SendStatus = "accepted"
	SendRejected SendStatus = "rejected"
	SendFailed   SendStatus = "failed"
)

type SendResult struct {
	Status            SendStatus
	Retryable         bool
	ErrorCode         string
	Error             string
	ProviderMessageID string
	ProviderResponse  string
}

type Sender interface {
	Send(context.Context, Message) SendResult
}

type SMTPSender struct {
	settings Settings
	timeout  time.Duration
}

func NewSMTPSender(settings Settings) *SMTPSender {
	timeout := settings.Lease - time.Second
	if timeout < time.Second {
		timeout = time.Second
	}
	if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	return &SMTPSender{settings: settings, timeout: timeout}
}

func (s *SMTPSender) Send(ctx context.Context, message Message) SendResult {
	if s == nil {
		return SendResult{Status: SendFailed, Retryable: true, ErrorCode: "smtp_not_configured"}
	}
	settings := s.settings.SMTP
	_, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(message.To)), "@")
	if !ok || domain == "" || strings.Contains(domain, "@") {
		return SendResult{Status: SendRejected, ErrorCode: "invalid_recipient"}
	}
	for _, fallbackDomain := range s.settings.FallbackDomains {
		if strings.EqualFold(strings.TrimSpace(fallbackDomain), domain) && s.settings.FallbackSMTP.Host != "" {
			settings = s.settings.FallbackSMTP
			break
		}
	}
	if settings.Host == "" {
		return SendResult{Status: SendFailed, Retryable: true, ErrorCode: "smtp_not_configured"}
	}
	result := sendSMTP(ctx, settings, message, s.timeout)
	return result
}

func sendSMTP(ctx context.Context, settings SMTPConfig, message Message, timeout time.Duration) SendResult {
	to, err := mail.ParseAddress(message.To)
	if err != nil || to.Address == "" {
		return SendResult{Status: SendRejected, ErrorCode: "invalid_recipient"}
	}
	from, err := mail.ParseAddress(settings.Sender)
	if err != nil || from.Address == "" {
		return SendResult{Status: SendFailed, Retryable: false, ErrorCode: "invalid_sender"}
	}
	if strings.ContainsAny(message.Subject, "\r\n") {
		return SendResult{Status: SendRejected, ErrorCode: "invalid_subject"}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client, err := newSMTPClient(ctx, settings, timeout)
	if err != nil {
		return classifySMTPError(err)
	}
	defer client.Close()
	if err := client.Mail(from.Address); err != nil {
		return classifySMTPError(err)
	}
	if err := client.Rcpt(to.Address); err != nil {
		return classifySMTPError(err)
	}
	data, err := client.Data()
	if err != nil {
		return classifySMTPError(err)
	}
	encodedSubject := mime.QEncoding.Encode("UTF-8", message.Subject)
	headers := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", smtpAddress(from), smtpAddress(to), encodedSubject)
	if _, err := io.WriteString(data, headers); err != nil {
		_ = data.Close()
		return classifySMTPError(err)
	}
	body := quotedprintable.NewWriter(data)
	if _, err := io.WriteString(body, message.HTML); err != nil {
		_ = body.Close()
		_ = data.Close()
		return classifySMTPError(err)
	}
	if err := body.Close(); err != nil {
		_ = data.Close()
		return classifySMTPError(err)
	}
	if err := data.Close(); err != nil {
		return classifySMTPError(err)
	}
	// DATA accepted the message; a later QUIT failure must not cause a duplicate retry.
	_ = client.Quit()
	return SendResult{Status: SendAccepted}
}

func newSMTPClient(ctx context.Context, settings SMTPConfig, timeout time.Duration) (*smtp.Client, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	address := net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port))
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if settings.Secure {
		secureConn := tls.Client(conn, smtpTLSConfig(settings))
		if err := secureConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = secureConn
	}
	client, err := smtp.NewClient(conn, smtpHeloHostname(settings.Name))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !settings.Secure {
		if supported, _ := client.Extension("STARTTLS"); supported {
			if err := client.StartTLS(smtpTLSConfig(settings)); err != nil {
				_ = client.Close()
				return nil, err
			}
		}
	}
	if settings.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", settings.Username, settings.Password, settings.Host)); err != nil {
			_ = client.Close()
			return nil, err
		}
	}
	return client, nil
}

func smtpAddress(address *mail.Address) string {
	if address.Name == "" {
		return "<" + address.Address + ">"
	}
	return mime.QEncoding.Encode("UTF-8", address.Name) + " <" + address.Address + ">"
}

func smtpHeloHostname(configured string) string {
	for _, candidate := range []string{configured, hostname()} {
		if normalized := normalizeSMTPHeloHostname(candidate); normalized != "" {
			return normalized
		}
	}
	return "localhost"
}

func hostname() string {
	value, _ := os.Hostname()
	return value
}

func normalizeSMTPHeloHostname(value string) string {
	value = strings.TrimSuffix(strings.TrimSpace(value), ".")
	if value == "" || len(value) > 253 || strings.ContainsAny(value, " \r\n\t") {
		return ""
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		literal := strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
		literal = strings.TrimPrefix(literal, "IPv6:")
		if net.ParseIP(literal) != nil {
			return value
		}
		return ""
	}
	if net.ParseIP(value) != nil {
		return value
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return ""
		}
		for _, char := range label {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-') {
				return ""
			}
		}
	}
	return value
}

func smtpTLSConfig(settings SMTPConfig) *tls.Config {
	return &tls.Config{ServerName: settings.Host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: settings.IgnoreTLS}
}

func classifySMTPError(err error) SendResult {
	var protocolError *textproto.Error
	if errors.As(err, &protocolError) {
		retryable := protocolError.Code >= 400 && protocolError.Code < 500
		status := SendRejected
		if retryable {
			status = SendFailed
		}
		return SendResult{Status: status, Retryable: retryable, ErrorCode: "smtp_response", Error: protocolError.Msg}
	}
	return SendResult{Status: SendFailed, Retryable: true, ErrorCode: "transport_failed", Error: err.Error()}
}

var (
	memberRemovedTemplate            = template.Must(template.New("member-removed").Parse(`<h1>Workspace access removed</h1><p>You have been removed from <strong>{{.Workspace}}</strong>. You no longer have access to this workspace.</p>`))
	memberLeaveTemplate              = template.Must(template.New("member-leave").Parse(`<h1>Member left {{.Workspace}}</h1><p>{{.Email}} has left workspace {{.Workspace}}</p>`))
	memberInvitationTemplate         = template.Must(template.New("member-invitation").Parse(`<h1>You are invited!</h1><p>{{.Actor}} invited you to join <strong>{{.Workspace}}</strong>.</p><p>Click the button to join this workspace.</p><p><a href="{{.URL}}">Accept &amp; Join</a></p>`))
	invitationApprovedTemplate       = template.Must(template.New("invitation-approved").Parse(`<h1>Welcome to the workspace!</h1><p>Your request to join <strong>{{.Workspace}}</strong> has been accepted. You can now access the team workspace and collaborate with other members.</p><p><a href="{{.URL}}">Open Workspace</a></p>`))
	invitationReviewRequestTemplate  = template.Must(template.New("invitation-review-request").Parse(`<h1>Request to join <strong>{{.Workspace}}</strong></h1><p>{{.InviteeEmail}} has requested to join {{.Workspace}}.<br>As a workspace owner/admin, you can approve or decline this request.</p><p><a href="{{.URL}}">Review request</a></p>`))
	invitationReviewDeclinedTemplate = template.Must(template.New("invitation-review-declined").Parse(`<h1>Request declined</h1><p>Your request to join <strong>{{.Workspace}}</strong> has been declined by the workspace admin.</p>`))
	signInTemplate                   = template.Must(template.New("sign-in").Parse(`<h1>Sign in to {{.ServerName}}</h1><p>You are signing in to FydeOS Notes. Here is your code:</p><p><strong>{{.OTP}}</strong></p><p>Alternatively, you can sign in directly by clicking the magic link below:</p><p><a href="{{.URL}}">Sign in with Magic Link</a></p><p>This code and link will expire in 30 minutes.</p>`))
	signUpTemplate                   = template.Must(template.New("sign-up").Parse(`<h1>Sign up to {{.ServerName}}</h1><p>You are signing up to FydeOS Notes. Here is your code:</p><p><strong>{{.OTP}}</strong></p><p>Alternatively, you can sign up directly by clicking the magic link below:</p><p><a href="{{.URL}}">Sign up with Magic Link</a></p><p>This code and link will expire in 30 minutes.</p>`))
	securityChallengeTemplate        = template.Must(template.New("security-challenge").Parse(`<h1>{{.Title}}</h1><p>{{.Message}}</p><p><a href="{{.URL}}">{{.Action}}</a></p><p>This link expires in 30 minutes.</p>`))
	emailChangedTemplate             = template.Must(template.New("email-changed").Parse(`<h1>Account email changed</h1><p>Your account email address was changed to <strong>{{.Email}}</strong>.</p><p>If you did not make this change, secure your account immediately.</p>`))
)

func renderMessage(name, recipient string, props map[string]any, workspaceName string, actorEmails ...string) (Message, bool, error) {
	var message Message
	message.Name = name
	message.To = recipient
	var html bytes.Buffer
	switch name {
	case "MemberRemoved":
		if workspaceName == "" {
			return Message{}, false, nil
		}
		message.Subject = "You have been removed from a workspace"
		if err := memberRemovedTemplate.Execute(&html, map[string]string{"Workspace": workspaceName}); err != nil {
			return Message{}, false, err
		}
	case "MemberLeave":
		if workspaceName == "" || len(actorEmails) == 0 || actorEmails[0] == "" {
			return Message{}, false, nil
		}
		message.Subject = "A workspace member left"
		if err := memberLeaveTemplate.Execute(&html, map[string]string{"Workspace": workspaceName, "Email": actorEmails[0]}); err != nil {
			return Message{}, false, err
		}
	case "MemberInvitation":
		if workspaceName == "" || len(actorEmails) < 2 || actorEmails[1] == "" {
			return Message{}, false, nil
		}
		url, _ := props["url"].(string)
		if url == "" {
			return Message{}, false, nil
		}
		message.Subject = "You were invited to join a workspace on FydeOS Notes"
		if err := memberInvitationTemplate.Execute(&html, map[string]string{"Actor": actorEmails[1], "Workspace": workspaceName, "URL": url}); err != nil {
			return Message{}, false, err
		}
	case "LinkInvitationApprove":
		if workspaceName == "" {
			return Message{}, false, nil
		}
		url, _ := props["url"].(string)
		if url == "" {
			return Message{}, false, nil
		}
		message.Subject = "Your request to join a workspace has been approved"
		if err := invitationApprovedTemplate.Execute(&html, map[string]string{"Workspace": workspaceName, "URL": url}); err != nil {
			return Message{}, false, err
		}
	case "LinkInvitationReviewRequest":
		if workspaceName == "" || len(actorEmails) == 0 || actorEmails[0] == "" {
			return Message{}, false, nil
		}
		url, _ := props["url"].(string)
		if url == "" {
			return Message{}, false, nil
		}
		message.Subject = "New request to join a workspace"
		if err := invitationReviewRequestTemplate.Execute(&html, map[string]string{"Workspace": workspaceName, "InviteeEmail": actorEmails[0], "URL": url}); err != nil {
			return Message{}, false, err
		}
	case "LinkInvitationDecline":
		if workspaceName == "" {
			return Message{}, false, nil
		}
		message.Subject = "Your request to join a workspace was declined"
		if err := invitationReviewDeclinedTemplate.Execute(&html, map[string]string{"Workspace": workspaceName}); err != nil {
			return Message{}, false, err
		}
	case "SignUp", "SignIn":
		url, _ := props["url"].(string)
		otp, _ := props["otp"].(string)
		serverName, _ := props["serverName"].(string)
		if url == "" || otp == "" {
			return Message{}, false, nil
		}
		if serverName == "" {
			serverName = "FydeOS Notes"
		}
		mailTemplate := signInTemplate
		message.Subject = "Sign in to FydeOS Notes"
		if name == "SignUp" {
			mailTemplate = signUpTemplate
			message.Subject = "Your FydeOS Notes account is waiting for you!"
		}
		if err := mailTemplate.Execute(&html, map[string]string{"URL": url, "OTP": otp, "ServerName": serverName}); err != nil {
			return Message{}, false, err
		}
	case "ChangePassword", "SetPassword", "ChangeEmail", "VerifyChangeEmail", "VerifyEmail":
		url, _ := props["url"].(string)
		if url == "" {
			return Message{}, false, nil
		}
		serverName, _ := props["serverName"].(string)
		if serverName == "" {
			serverName = "FydeOS Notes"
		}
		content := map[string]string{"URL": url, "ServerName": serverName}
		switch name {
		case "ChangePassword":
			message.Subject = "Change your FydeOS Notes password"
			content["Title"] = "Change your password"
			content["Message"] = "A password change was requested for your account."
			content["Action"] = "Change password"
		case "SetPassword":
			message.Subject = "Set your FydeOS Notes password"
			content["Title"] = "Set your password"
			content["Message"] = "Use this link to set a password for your account."
			content["Action"] = "Set password"
		case "ChangeEmail":
			message.Subject = "Change your FydeOS Notes email address"
			content["Title"] = "Change your email address"
			content["Message"] = "A change to the email address on your account was requested."
			content["Action"] = "Continue email change"
		case "VerifyChangeEmail":
			message.Subject = "Verify your new FydeOS Notes email address"
			content["Title"] = "Verify your new email address"
			content["Message"] = "Confirm this email address to finish updating your account."
			content["Action"] = "Verify email address"
		case "VerifyEmail":
			message.Subject = "Verify your email address"
			content["Title"] = "Verify your email address"
			content["Message"] = "You recently requested to verify the email address associated with your FydeOS Notes account. To complete this process, please click on the verification link below."
			content["Action"] = "Verify your email address"
		}
		if err := securityChallengeTemplate.Execute(&html, content); err != nil {
			return Message{}, false, err
		}
	case "EmailChanged":
		message.Subject = "Your FydeOS Notes account email changed"
		email, _ := props["to"].(string)
		if email == "" {
			email = recipient
		}
		if err := emailChangedTemplate.Execute(&html, map[string]string{"Email": email}); err != nil {
			return Message{}, false, err
		}
	default:
		return Message{}, false, nil
	}
	message.HTML = html.String()
	return message, true, nil
}
