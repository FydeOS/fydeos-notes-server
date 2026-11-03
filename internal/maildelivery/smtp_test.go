// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSMTPSenderClassifiesAcceptanceAndRecipientRejection(t *testing.T) {
	for _, test := range []struct {
		name       string
		code       int
		wantStatus SendStatus
		wantRetry  bool
	}{
		{name: "accepted", wantStatus: SendAccepted},
		{name: "temporary failure", code: 450, wantStatus: SendFailed, wantRetry: true},
		{name: "permanent rejection", code: 550, wantStatus: SendRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := startSMTPTestServer(t, test.code, "temporary user@example.test code 123456 https://secret.example/reset")
			settings := Settings{SMTP: SMTPConfig{
				Host:   "127.0.0.1",
				Port:   server.port,
				Sender: "Notes <notes@example.test>",
				Name:   "notes.example.test",
			}}
			result := NewSMTPSender(settings).Send(context.Background(), Message{
				Name: "MemberRemoved", To: "user@example.test", Subject: "Removed", HTML: "<p>Workspace</p>",
			})
			if result.Status != test.wantStatus || result.Retryable != test.wantRetry {
				t.Fatalf("SMTP result = %#v, want status %q retryable=%t", result, test.wantStatus, test.wantRetry)
			}
			if test.code == 0 {
				select {
				case body := <-server.body:
					if !strings.Contains(body, "Subject: Removed") || !strings.Contains(body, "<p>Workspace</p>") {
						t.Fatalf("SMTP message body = %q", body)
					}
				case <-time.After(time.Second):
					t.Fatal("SMTP server did not receive the message")
				}
			}
			if err := <-server.done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRenderMagicLinkUsesFydeOSNotesDefaultServerName(t *testing.T) {
	message, ok, err := renderMessage("SignIn", "user@example.test", map[string]any{
		"url": "https://notes.example.test/magic-link", "otp": "123456",
	}, "")
	if err != nil || !ok {
		t.Fatalf("render sign-in mail = %#v, ok=%t, err=%v", message, ok, err)
	}
	if message.Subject != "Sign in to FydeOS Notes" || !strings.Contains(message.HTML, "Sign in to FydeOS Notes") {
		t.Fatalf("rendered default sign-in mail = %#v", message)
	}
}

func TestRenderSecurityChallengeMailsRequireAndEscapeActionURLs(t *testing.T) {
	for _, test := range []struct {
		name    string
		subject string
		button  string
	}{
		{name: "ChangePassword", subject: "Change your FydeOS Notes password", button: "Change password"},
		{name: "SetPassword", subject: "Set your FydeOS Notes password", button: "Set password"},
		{name: "ChangeEmail", subject: "Change your FydeOS Notes email address", button: "Continue email change"},
		{name: "VerifyChangeEmail", subject: "Verify your new FydeOS Notes email address", button: "Verify email address"},
		{name: "VerifyEmail", subject: "Verify your email address", button: "Verify your email address"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, ok, err := renderMessage(test.name, "user@example.test", map[string]any{
				"url": "https://notes.example.test/security?token=a&email=b",
			}, "")
			if err != nil || !ok || message.Subject != test.subject ||
				!strings.Contains(message.HTML, test.button) ||
				!strings.Contains(message.HTML, "https://notes.example.test/security?token=a&amp;email=b") {
				t.Fatalf("rendered security mail = %#v, ok=%t, err=%v", message, ok, err)
			}
			if _, ok, err := renderMessage(test.name, "user@example.test", map[string]any{}, ""); err != nil || ok {
				t.Fatalf("security mail without URL returned ok=%t err=%v", ok, err)
			}
		})
	}
	emailChanged, ok, err := renderMessage("EmailChanged", "new@example.test", map[string]any{"to": "new@example.test"}, "")
	if err != nil || !ok || emailChanged.Subject != "Your FydeOS Notes account email changed" || !strings.Contains(emailChanged.HTML, "new@example.test") {
		t.Fatalf("rendered email-changed mail = %#v, ok=%t, err=%v", emailChanged, ok, err)
	}
}

func TestRenderMemberLeaveUsesLegacyTemplate(t *testing.T) {
	message, ok, err := renderMessage("MemberLeave", "owner@example.test", map[string]any{}, "Project North Star", "member@example.test")
	if err != nil || !ok {
		t.Fatalf("render member-leave mail = %#v, ok=%t, err=%v", message, ok, err)
	}
	if message.Subject != "A workspace member left" || !strings.Contains(message.HTML, "Member left Project North Star") ||
		!strings.Contains(message.HTML, "member@example.test has left workspace Project North Star") {
		t.Fatalf("rendered member-leave mail = %#v", message)
	}
}

func TestRenderMemberInvitationUsesDynamicSenderWorkspaceAndURL(t *testing.T) {
	message, ok, err := renderMessage("MemberInvitation", "invitee@example.test", map[string]any{
		"url": "https://notes.example.test/invite/invite-1?source=a&next=b",
	}, `<Project & Team>`, "", `<Admin & Owner>`)
	if err != nil || !ok {
		t.Fatalf("render member invitation = %#v, ok=%t, err=%v", message, ok, err)
	}
	if message.Subject != "You were invited to join a workspace on FydeOS Notes" ||
		!strings.Contains(message.HTML, "&lt;Admin &amp; Owner&gt;") ||
		!strings.Contains(message.HTML, "&lt;Project &amp; Team&gt;") ||
		!strings.Contains(message.HTML, "https://notes.example.test/invite/invite-1?source=a&amp;next=b") ||
		!strings.Contains(message.HTML, "Accept &amp; Join") {
		t.Fatalf("rendered member invitation = %#v", message)
	}
}

func TestRenderInvitationApprovalUsesLegacyTemplateAndEscapesWorkspaceName(t *testing.T) {
	message, ok, err := renderMessage("LinkInvitationApprove", "invitee@example.test", map[string]any{
		"url": "https://notes.example.test/workspace/workspace-1",
	}, `<Review & Team> <script>alert(1)</script>`)
	if err != nil || !ok {
		t.Fatalf("render invitation approval mail = %#v, ok=%t, err=%v", message, ok, err)
	}
	if message.Subject != "Your request to join a workspace has been approved" ||
		!strings.Contains(message.HTML, "Your request to join") || !strings.Contains(message.HTML, "https://notes.example.test/workspace/workspace-1") ||
		strings.Contains(message.HTML, "<script>") || !strings.Contains(message.HTML, "&lt;script&gt;") {
		t.Fatalf("rendered invitation approval mail = %#v", message)
	}
}

func TestRenderInvitationReviewRequestAndDeclineUseLegacySubjectsAndEscapeDynamicValues(t *testing.T) {
	request, ok, err := renderMessage("LinkInvitationReviewRequest", "reviewer@example.test", map[string]any{
		"url": "https://notes.example.test/workspace/workspace-1/settings?tab=workspace%3Amembers",
	}, `<Review & Team>`, `invitee<&>@example.test`)
	if err != nil || !ok {
		t.Fatalf("render invitation review request = %#v, ok=%t, err=%v", request, ok, err)
	}
	if request.Subject != "New request to join a workspace" || !strings.Contains(request.HTML, "invitee&lt;&amp;&gt;@example.test") ||
		!strings.Contains(request.HTML, "&lt;Review &amp; Team&gt;") || !strings.Contains(request.HTML, "Review request") || strings.Contains(request.HTML, "<Review & Team>") {
		t.Fatalf("rendered invitation review request = %#v", request)
	}
	declined, ok, err := renderMessage("LinkInvitationDecline", "invitee@example.test", map[string]any{}, `<Review & Team>`)
	if err != nil || !ok {
		t.Fatalf("render invitation decline = %#v, ok=%t, err=%v", declined, ok, err)
	}
	if declined.Subject != "Your request to join a workspace was declined" || !strings.Contains(declined.HTML, "&lt;Review &amp; Team&gt;") {
		t.Fatalf("rendered invitation decline = %#v", declined)
	}
}

func TestMailSettingsConfiguredForRecipientFallbackDomain(t *testing.T) {
	settings := Settings{FallbackSMTP: SMTPConfig{Host: "smtp-fallback.example.test"}, FallbackDomains: []string{"example.test"}}
	if !mailSettingsConfiguredFor(settings, "example.test") || mailSettingsConfiguredFor(settings, "other.example.test") {
		t.Fatalf("fallback mail configuration did not match recipient domain: %+v", settings)
	}
}

func TestWorkerResolvesMemberLeaveDynamicProperties(t *testing.T) {
	store := &memberLeaveStoreFake{}
	sender := &memberLeaveSenderFake{}
	worker, err := NewWorker(store, sender, Settings{BatchSize: 1, Lease: time.Second, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	worker.workerID = "worker-test"
	delivery := Delivery{
		ID: "delivery-1", MailName: "MemberLeave", Recipient: stringPointer("owner@example.test"),
		Payload: []byte(`{"name":"MemberLeave","props":{"workspace":{"$$workspaceId":"workspace-1"},"user":{"$$userId":"member-1"}}}`),
	}
	if err := worker.process(context.Background(), delivery); err != nil {
		t.Fatalf("process member-leave mail: %v", err)
	}
	if store.workspaceID != "workspace-1" || store.userID != "member-1" || sender.message.Subject != "A workspace member left" ||
		!strings.Contains(sender.message.HTML, "member@example.test has left workspace Project North Star") || store.sent != 1 {
		t.Fatalf("member-leave processing workspace=%q user=%q message=%#v sent=%d", store.workspaceID, store.userID, sender.message, store.sent)
	}
}

func TestWorkerResolvesInvitationApprovalWorkspaceProperty(t *testing.T) {
	store := &memberLeaveStoreFake{}
	sender := &memberLeaveSenderFake{}
	worker, err := NewWorker(store, sender, Settings{BatchSize: 1, Lease: time.Second, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	worker.workerID = "worker-test"
	delivery := Delivery{
		ID: "delivery-approval", MailName: "LinkInvitationApprove", Recipient: stringPointer("invitee@example.test"),
		Payload: []byte(`{"name":"LinkInvitationApprove","props":{"workspace":{"$$workspaceId":"workspace-1"},"url":"https://notes.example.test/workspace/workspace-1"}}`),
	}
	if err := worker.process(context.Background(), delivery); err != nil {
		t.Fatalf("process invitation approval mail: %v", err)
	}
	if store.workspaceID != "workspace-1" || sender.message.Subject != "Your request to join a workspace has been approved" ||
		!strings.Contains(sender.message.HTML, "Project North Star") || !strings.Contains(sender.message.HTML, "https://notes.example.test/workspace/workspace-1") || store.sent != 1 {
		t.Fatalf("approval processing workspace=%q message=%#v sent=%d", store.workspaceID, sender.message, store.sent)
	}
}

func TestWorkerResolvesMemberInvitationDynamicProperties(t *testing.T) {
	baseStore := &memberLeaveStoreFake{}
	store := memberInvitationWorkerStoreFake{memberLeaveStoreFake: baseStore}
	sender := &memberLeaveSenderFake{}
	worker, err := NewWorker(store, sender, Settings{BatchSize: 1, Lease: time.Second, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	worker.workerID = "worker-test"
	delivery := Delivery{
		ID: "delivery-member-invitation", MailName: "MemberInvitation", Recipient: stringPointer("invitee@example.test"),
		Payload: []byte(`{"name":"MemberInvitation","props":{"workspace":{"$$workspaceId":"workspace-1"},"user":{"$$userId":"actor-1"},"url":"https://notes.example.test/invite/invite-1"}}`),
	}
	if err := worker.process(context.Background(), delivery); err != nil {
		t.Fatalf("process member-invitation mail: %v", err)
	}
	if baseStore.workspaceID != "workspace-1" || baseStore.userID != "actor-1" ||
		sender.message.Subject != "You were invited to join a workspace on FydeOS Notes" ||
		!strings.Contains(sender.message.HTML, "Admin &amp; Owner invited you to join") ||
		!strings.Contains(sender.message.HTML, "Project North Star") || !strings.Contains(sender.message.HTML, "/invite/invite-1") || baseStore.sent != 1 {
		t.Fatalf("member-invitation processing workspace=%q user=%q message=%#v sent=%d", baseStore.workspaceID, baseStore.userID, sender.message, baseStore.sent)
	}
}

func TestWorkerResolvesInvitationReviewRequestDynamicProperties(t *testing.T) {
	store := &memberLeaveStoreFake{}
	sender := &memberLeaveSenderFake{}
	worker, err := NewWorker(store, sender, Settings{BatchSize: 1, Lease: time.Second, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	worker.workerID = "worker-test"
	delivery := Delivery{
		ID: "delivery-review-request", MailName: "LinkInvitationReviewRequest", Recipient: stringPointer("reviewer@example.test"),
		Payload: []byte(`{"name":"LinkInvitationReviewRequest","props":{"workspace":{"$$workspaceId":"workspace-1"},"user":{"$$userId":"invitee-1"},"url":"https://notes.example.test/workspace/workspace-1/settings?tab=workspace%3Amembers"}}`),
	}
	if err := worker.process(context.Background(), delivery); err != nil {
		t.Fatalf("process invitation review request: %v", err)
	}
	if store.workspaceID != "workspace-1" || store.userID != "invitee-1" || sender.message.Subject != "New request to join a workspace" ||
		!strings.Contains(sender.message.HTML, "member@example.test") || !strings.Contains(sender.message.HTML, "Project North Star") ||
		!strings.Contains(sender.message.HTML, "/settings?tab=workspace%3Amembers") || store.sent != 1 {
		t.Fatalf("invitation review request processing workspace=%q user=%q message=%#v sent=%d", store.workspaceID, store.userID, sender.message, store.sent)
	}
}

type memberLeaveStoreFake struct {
	workspaceID string
	userID      string
	sent        int
}

type memberInvitationWorkerStoreFake struct{ *memberLeaveStoreFake }

func (s memberInvitationWorkerStoreFake) UserDisplayName(_ context.Context, userID string) (string, bool, error) {
	s.memberLeaveStoreFake.userID = userID
	return "Admin & Owner", true, nil
}

func (s *memberLeaveStoreFake) ClaimReady(context.Context, string, int, time.Duration) ([]Delivery, error) {
	return nil, nil
}
func (s *memberLeaveStoreFake) StartAttempt(context.Context, string, string) (Attempt, bool, error) {
	return Attempt{Count: 1, Max: 3}, true, nil
}
func (s *memberLeaveStoreFake) MarkSent(context.Context, string, string, DeliveryResult) error {
	s.sent++
	return nil
}
func (s *memberLeaveStoreFake) MarkSkipped(context.Context, string, string, string) error { return nil }
func (s *memberLeaveStoreFake) MarkFailed(context.Context, string, string, string, string) error {
	return nil
}
func (s *memberLeaveStoreFake) MarkRetry(context.Context, string, string, time.Time, DeliveryResult) (bool, error) {
	return false, nil
}
func (s *memberLeaveStoreFake) WorkspaceName(_ context.Context, workspaceID string) (string, bool, error) {
	s.workspaceID = workspaceID
	return "Project North Star", true, nil
}
func (s *memberLeaveStoreFake) UserEmail(_ context.Context, userID string) (string, bool, error) {
	s.userID = userID
	return "member@example.test", true, nil
}
func (s *memberLeaveStoreFake) RecipientDisabled(context.Context, string) (bool, error) {
	return false, nil
}
func (s *memberLeaveStoreFake) DeleteAnonymizedBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type memberLeaveSenderFake struct{ message Message }

func (s *memberLeaveSenderFake) Send(_ context.Context, message Message) SendResult {
	s.message = message
	return SendResult{Status: SendAccepted}
}

func stringPointer(value string) *string { return &value }

type smtpTestServer struct {
	port int
	body chan string
	done chan error
}

func startSMTPTestServer(t *testing.T, recipientCode int, response string) smtpTestServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for local SMTP server: %v", err)
	}
	server := smtpTestServer{port: listener.Addr().(*net.TCPAddr).Port, body: make(chan string, 1), done: make(chan error, 1)}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			server.done <- err
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(connection)
		write := func(line string) error {
			_, err := fmt.Fprintf(connection, "%s\r\n", line)
			return err
		}
		if err := write("220 local SMTP ready"); err != nil {
			server.done <- err
			return
		}
		for {
			command, err := reader.ReadString('\n')
			if err != nil {
				server.done <- err
				return
			}
			command = strings.TrimSpace(command)
			switch {
			case strings.HasPrefix(command, "EHLO "), strings.HasPrefix(command, "HELO "):
				err = write("250 local SMTP")
			case strings.HasPrefix(command, "MAIL FROM:"):
				err = write("250 sender accepted")
			case strings.HasPrefix(command, "RCPT TO:"):
				if recipientCode != 0 {
					err = write(fmt.Sprintf("%d %s", recipientCode, response))
					server.done <- err
					return
				}
				err = write("250 recipient accepted")
			case command == "DATA":
				if err = write("354 send message"); err != nil {
					server.done <- err
					return
				}
				var message strings.Builder
				for {
					line, readErr := reader.ReadString('\n')
					if readErr != nil {
						server.done <- readErr
						return
					}
					if strings.TrimSpace(line) == "." {
						break
					}
					message.WriteString(line)
				}
				server.body <- message.String()
				err = write("250 message accepted")
			case command == "QUIT":
				server.done <- write("221 closing connection")
				return
			default:
				err = write("250 ok")
			}
			if err != nil {
				server.done <- err
				return
			}
		}
	}()
	return server
}
