// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTestEmailSenderVerifiesAndSends(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		server := startSMTPTestConversation(t, 2, false, 0, false)
		sender := NewTestEmailSender()
		sender.timeout = 2 * time.Second
		err := sender.SendTestEmail(context.Background(), SMTPConfig{
			Name: "notes.example.test", Host: "127.0.0.1", Port: server.port,
			Sender: "Notes <notes@example.test>",
		}, "admin@example.test")
		if err != nil {
			t.Fatalf("SendTestEmail() error = %v", err)
		}
		select {
		case message := <-server.message:
			headerEnd := strings.Index(message, "\r\n\r\n")
			if headerEnd < 0 {
				t.Fatalf("SMTP message has no header/body separator: %q", message)
			}
			body, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(message[headerEnd+4:])))
			if err != nil {
				t.Fatalf("decode SMTP message body: %v", err)
			}
			decoded := message[:headerEnd+4] + string(body)
			for _, expected := range []string{
				"From: Notes <notes@example.test>",
				"To: <admin@example.test>",
				"Subject: Test Email from FydeOS Notes",
				"This is a test email from your FydeOS Notes instance.",
			} {
				if !strings.Contains(decoded, expected) {
					t.Errorf("SMTP message %q does not contain %q", decoded, expected)
				}
			}
		case <-time.After(time.Second):
			t.Fatal("SMTP server did not receive the test message")
		}
		if err := <-server.done; err != nil {
			t.Fatalf("SMTP server: %v", err)
		}
	})

	t.Run("authenticates during verification and send", func(t *testing.T) {
		server := startSMTPTestConversation(t, 2, false, 0, true)
		sender := NewTestEmailSender()
		sender.timeout = 2 * time.Second
		err := sender.SendTestEmail(context.Background(), SMTPConfig{
			Name: "localhost", Host: "localhost", Port: server.port,
			Sender: "notes@example.test", Username: "test-user", Password: "test-pass",
		}, "admin@example.test")
		if err != nil {
			t.Fatalf("SendTestEmail() error = %v", err)
		}
		if err := <-server.done; err != nil {
			t.Fatalf("SMTP server: %v", err)
		}
		if *server.authCalls != 2 {
			t.Fatalf("SMTP AUTH calls = %d, want one for verify and one for send", *server.authCalls)
		}
	})

	t.Run("verification failure prevents sending", func(t *testing.T) {
		server := startSMTPTestConversation(t, 1, true, 0, false)
		sender := NewTestEmailSender()
		sender.timeout = 2 * time.Second
		err := sender.SendTestEmail(context.Background(), SMTPConfig{
			Host: "127.0.0.1", Port: server.port, Sender: "notes@example.test",
		}, "admin@example.test")
		var testErr *SMTPTestError
		if !errors.As(err, &testErr) || testErr.Phase != "verify" {
			t.Fatalf("SendTestEmail() error = %#v, want verify-phase SMTPTestError", err)
		}
		if serverErr := <-server.done; serverErr != nil {
			t.Fatalf("SMTP server: %v", serverErr)
		}
	})

	t.Run("send rejection is reported separately", func(t *testing.T) {
		server := startSMTPTestConversation(t, 2, false, 550, false)
		sender := NewTestEmailSender()
		sender.timeout = 2 * time.Second
		err := sender.SendTestEmail(context.Background(), SMTPConfig{
			Host: "127.0.0.1", Port: server.port, Sender: "notes@example.test",
		}, "admin@example.test")
		var testErr *SMTPTestError
		if !errors.As(err, &testErr) || testErr.Phase != "send" {
			t.Fatalf("SendTestEmail() error = %#v, want send-phase SMTPTestError", err)
		}
		if serverErr := <-server.done; serverErr != nil {
			t.Fatalf("SMTP server: %v", serverErr)
		}
	})
}

type smtpConversation struct {
	port      int
	message   chan string
	done      chan error
	authCalls *int
}

func startSMTPTestConversation(t *testing.T, connections int, rejectVerify bool, rejectRecipient int, requireAuth bool) smtpConversation {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for local SMTP server: %v", err)
	}
	server := smtpConversation{
		port:      listener.Addr().(*net.TCPAddr).Port,
		message:   make(chan string, 1),
		done:      make(chan error, 1),
		authCalls: new(int),
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for connectionIndex := 0; connectionIndex < connections; connectionIndex++ {
			connection, err := listener.Accept()
			if err != nil {
				server.done <- err
				return
			}
			if err := serveSMTPTestConnection(connection, connectionIndex, rejectVerify, rejectRecipient, requireAuth, server.authCalls, server.message); err != nil {
				server.done <- err
				return
			}
		}
		server.done <- nil
	}()
	return server
}

func serveSMTPTestConnection(connection net.Conn, connectionIndex int, rejectVerify bool, rejectRecipient int, requireAuth bool, authCalls *int, message chan<- string) error {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(connection)
	write := func(line string) error {
		_, err := fmt.Fprintf(connection, "%s\r\n", line)
		return err
	}
	if err := write("220 local SMTP ready"); err != nil {
		return err
	}
	for {
		command, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		command = strings.TrimSpace(command)
		switch {
		case strings.HasPrefix(command, "EHLO "), strings.HasPrefix(command, "HELO "):
			if requireAuth {
				err = write("250-local SMTP")
				if err == nil {
					err = write("250 AUTH PLAIN")
				}
			} else {
				err = write("250 local SMTP")
			}
		case strings.HasPrefix(command, "AUTH PLAIN ") && requireAuth:
			(*authCalls)++
			err = write("235 2.7.0 authentication accepted")
		case command == "NOOP" && connectionIndex == 0 && rejectVerify:
			err = write("550 SMTP verification rejected")
		case command == "NOOP":
			err = write("250 ok")
		case strings.HasPrefix(command, "MAIL FROM:"):
			err = write("250 sender accepted")
		case strings.HasPrefix(command, "RCPT TO:") && connectionIndex > 0 && rejectRecipient != 0:
			err = write(fmt.Sprintf("%d recipient rejected", rejectRecipient))
		case strings.HasPrefix(command, "RCPT TO:"):
			err = write("250 recipient accepted")
		case command == "DATA":
			if err = write("354 send message"); err != nil {
				return err
			}
			var content strings.Builder
			for {
				line, readErr := reader.ReadString('\n')
				if readErr != nil {
					return readErr
				}
				if strings.TrimSpace(line) == "." {
					break
				}
				content.WriteString(line)
			}
			message <- content.String()
			err = write("250 message accepted")
		case command == "QUIT":
			return write("221 closing connection")
		default:
			err = write("250 ok")
		}
		if err != nil {
			return err
		}
	}
}
