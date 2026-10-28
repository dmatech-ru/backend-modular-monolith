package sender

import (
	"context"
	"net/smtp"
	"strings"
	"testing"
)

func TestBuildEmailMessage_ContainsExpectedHeaders(t *testing.T) {
	msg := buildEmailMessage("from@example.com", "to@example.com", "Hello", "body text")
	got := string(msg)

	for _, want := range []string{
		"From: from@example.com\r\n",
		"To: to@example.com\r\n",
		"Subject: Hello\r\n",
		"MIME-Version: 1.0\r\n",
		"\r\n\r\nbody text",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("message = %q, want it to contain %q", got, want)
		}
	}
}

// Без проверки CR/LF в destination/subject можно было бы внедрить
// произвольный дополнительный заголовок (например, Bcc) — классическая
// SMTP header injection. EmailSender.Send должен отклонять такой ввод
// раньше, чем он попадёт в buildEmailMessage.
func TestEmailSender_RejectsHeaderInjectionInDestination(t *testing.T) {
	var dialed bool
	s := &EmailSender{
		cfg: EmailConfig{Host: "smtp.example.com", Port: 587, From: "from@example.com"},
		dialAndSend: func(string, smtp.Auth, string, []string, []byte) error {
			dialed = true
			return nil
		},
	}

	err := s.Send(context.Background(), "victim@example.com\r\nBcc: attacker@evil.com", "Subject", "body")
	if err != ErrInvalidDestination {
		t.Fatalf("err = %v, want ErrInvalidDestination", err)
	}
	if dialed {
		t.Fatal("must not attempt to send when destination contains a header injection payload")
	}
}

func TestEmailSender_RejectsHeaderInjectionInSubject(t *testing.T) {
	var dialed bool
	s := &EmailSender{
		cfg: EmailConfig{Host: "smtp.example.com", Port: 587, From: "from@example.com"},
		dialAndSend: func(string, smtp.Auth, string, []string, []byte) error {
			dialed = true
			return nil
		},
	}

	err := s.Send(context.Background(), "to@example.com", "Subject\r\nBcc: attacker@evil.com", "body")
	if err != ErrInvalidDestination {
		t.Fatalf("err = %v, want ErrInvalidDestination", err)
	}
	if dialed {
		t.Fatal("must not attempt to send when subject contains a header injection payload")
	}
}

func TestEmailSender_Send_CallsDialAndSendWithComposedMessage(t *testing.T) {
	var gotAddr, gotFrom string
	var gotTo []string
	var gotMsg []byte

	s := &EmailSender{
		cfg: EmailConfig{Host: "smtp.example.com", Port: 587, From: "from@example.com"},
		dialAndSend: func(addr string, _ smtp.Auth, from string, to []string, msg []byte) error {
			gotAddr, gotFrom, gotTo, gotMsg = addr, from, to, msg
			return nil
		},
	}

	if err := s.Send(context.Background(), "to@example.com", "Subject", "body"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAddr != "smtp.example.com:587" {
		t.Fatalf("addr = %q", gotAddr)
	}
	if gotFrom != "from@example.com" {
		t.Fatalf("from = %q", gotFrom)
	}
	if len(gotTo) != 1 || gotTo[0] != "to@example.com" {
		t.Fatalf("to = %v", gotTo)
	}
	if !strings.Contains(string(gotMsg), "Subject: Subject") {
		t.Fatalf("message does not contain the expected subject: %q", gotMsg)
	}
}
