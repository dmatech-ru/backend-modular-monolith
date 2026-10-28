package sender

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/smtp"
)

// ErrInvalidDestination — destination/subject не прошли базовую проверку
// (см. hasControlChars) — не специфично для почты, но заводится здесь,
// потому что здесь впервые понадобилось.
var ErrInvalidDestination = errors.New("sender: destination or subject contains a line break")

// EmailConfig — настройки SMTP-релея. Если Host пуст, вызывающий код
// (notifications-модуль) использует LogSender вместо EmailSender — реальный
// SMTP-релей для локальной разработки не обязателен.
type EmailConfig struct {
	Host     string `yaml:"host" env:"NOTIFICATIONS_EMAIL_SMTP_HOST"`
	Port     int    `yaml:"port" env:"NOTIFICATIONS_EMAIL_SMTP_PORT" env-default:"587"`
	Username string `yaml:"username" env:"NOTIFICATIONS_EMAIL_SMTP_USERNAME"`
	Password string `yaml:"password" env:"NOTIFICATIONS_EMAIL_SMTP_PASSWORD"`
	From     string `yaml:"from" env:"NOTIFICATIONS_EMAIL_FROM"`
}

// EmailSender отправляет письма через SMTP relay (net/smtp — без сторонних
// зависимостей). dialAndSend вынесен отдельно, чтобы его можно было
// подменить в тестах без реального SMTP-сервера.
type EmailSender struct {
	cfg         EmailConfig
	dialAndSend func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

func NewEmailSender(cfg EmailConfig) *EmailSender {
	return &EmailSender{cfg: cfg, dialAndSend: smtp.SendMail}
}

// Send собирает письмо и отправляет его через SMTP relay. Контекст не
// влияет на сам сетевой вызов — net/smtp.SendMail его не принимает;
// таймаут всей доставки лучше ограничивать снаружи (например, дедлайном
// NATS-сообщения).
func (s *EmailSender) Send(_ context.Context, destination, subject, body string) error {
	if hasControlChars(destination) || hasControlChars(subject) {
		return ErrInvalidDestination
	}
	msg := buildEmailMessage(s.cfg.From, destination, subject, body)

	var auth smtp.Auth
	if s.cfg.Username != "" {
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	}
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	if err := s.dialAndSend(addr, auth, s.cfg.From, []string{destination}, msg); err != nil {
		return fmt.Errorf("sender: send email: %w", err)
	}
	return nil
}

// buildEmailMessage собирает минимальное plain-text письмо. destination и
// subject уже проверены вызывающим кодом (Send) на отсутствие "\r\n" —
// см. hasControlChars: без этого было бы возможно внедрить произвольные
// заголовки (SMTP header injection) через значение одного из полей.
func buildEmailMessage(from, to, subject, body string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", from)
	fmt.Fprintf(&buf, "To: %s\r\n", to)
	fmt.Fprintf(&buf, "Subject: %s\r\n", subject)
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	buf.WriteString("\r\n")
	buf.WriteString(body)
	return buf.Bytes()
}
