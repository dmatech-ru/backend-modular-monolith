package identity

import (
	"context"
	"log/slog"
)

// logSender — единственная реализация authhandler.PhoneSender/EmailSender в
// этом репозитории: пишет код подтверждения в лог вместо реальной отправки.
// Интеграция с СМС-гейтвеем/почтовым сервисом не входит в эту итерацию — см.
// README "Известные ограничения". Для прода заменить на настоящую
// реализацию того же интерфейса, код вызывающей стороны (хендлер) не меняется.
type logSender struct {
	log  *slog.Logger
	kind string
}

func (s *logSender) SendCode(_ context.Context, target, code string) error {
	s.log.Warn("DEV: код подтверждения не отправлен реально, только в лог",
		"kind", s.kind, "target", target, "code", code)
	return nil
}
