// Package sender — отправители сообщений, один тип на канал доставки
// (peer.common.delivery.v1.DeliveryChannel): разный провайдер, разный протокол,
// разные сбои, общий только интерфейс Sender.
package sender

import (
	"context"
	"log/slog"
	"strings"
)

// Sender отправляет одно сообщение.
type Sender interface {
	Send(ctx context.Context, destination, subject, body string) error
}

// LogSender — дефолт для каналов без настроенного провайдера (SMS/MAX в
// этой итерации — у нас нет ни СМС-гейтвея, ни интеграции с MAX) и
// фоллбэк для email/Telegram, если их не сконфигурировали: пишет в лог
// вместо реальной отправки. Тот же приём, что identity's logSender
// (internal/modules/identity/dev_senders.go) — независимая небольшая
// реализация, не общая абстракция ради неё.
type LogSender struct {
	log  *slog.Logger
	kind string
}

// NewLogSender — kind попадает в лог как есть (например, "sms", "max").
func NewLogSender(log *slog.Logger, kind string) *LogSender {
	return &LogSender{log: log, kind: kind}
}

func (s *LogSender) Send(_ context.Context, destination, subject, body string) error {
	s.log.Warn("DEV: уведомление не отправлено реально, только в лог",
		"channel", s.kind, "destination", destination, "subject", subject, "body", body)
	return nil
}

// hasControlChars — true, если s содержит CR или LF. Email-заголовки
// (From/To/Subject) собираются конкатенацией строк (см. EmailSender) — без
// этой проверки destination/subject с "\r\n" внутри позволили бы внедрить
// произвольные дополнительные заголовки (например, лишний Bcc) — классическая
// SMTP header injection. Полноценный MIME-энкодер за счёт этого не нужен,
// достаточно отклонить сам факт переноса строки во входных данных.
func hasControlChars(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}
