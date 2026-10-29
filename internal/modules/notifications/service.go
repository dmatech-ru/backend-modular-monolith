package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"dmatech.ru/internal/modules/notifications/sender"
	"dmatech.ru/internal/modules/notifications/template"
	deliveryv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/delivery"
	notificationsv1 "dmatech.ru/internal/pkg/contracts/peer/v1/notifications"
	"dmatech.ru/internal/pkg/locale"
	"dmatech.ru/internal/pkg/mq"
)

// queuedMessage — формат payload'а в очереди: уже отрендеренный текст, не
// template_key/variables. Рендеринг — детерминированная чистая функция
// (template.Renderer), поэтому его результат можно посчитать один раз на
// Enqueue, а не при каждой (возможно, повторной после Nak) попытке
// доставки. Отдельный от notificationsv1.SendRequest тип и JSON, не
// protobuf: это внутренний формат очереди конкретно этого модуля, не часть
// публичного контракта — никто снаружи его не десериализует.
type queuedMessage struct {
	Destination string `json:"destination"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
}

// Service — доменная логика notifications: Enqueue рендерит шаблон
// (см. internal/modules/notifications/template) и кладёт готовый текст в
// очередь своего канала (см. subjects.go) — отвечает сразу, подтверждённое
// попадание в очередь, не подтверждение реальной доставки (см.
// doc-комментарий NotificationService в notifications.proto). deliver —
// обработчик очереди на стороне каждого канала, вызывается
// internal/pkg/mq.Consumer; какой Sender соответствует какому каналу,
// задаётся при старте модуля (см. notifications.go) конфигом.
type Service struct {
	js       jetstream.JetStream
	env      string
	renderer *template.Renderer
	senders  map[deliveryv1.DeliveryChannel]sender.Sender
	log      *slog.Logger
}

func NewService(js jetstream.JetStream, env string, renderer *template.Renderer, senders map[deliveryv1.DeliveryChannel]sender.Sender, log *slog.Logger) *Service {
	return &Service{js: js, env: env, renderer: renderer, senders: senders, log: log}
}

// Enqueue рендерит шаблон req.TemplateKey для req.Channel на локали
// текущего запроса (locale.FromContext — положена туда locale.
// ServerInterceptor, см. notifications.go) и кладёт результат в очередь.
// Локаль читается здесь, а не в deliverFor: рендеринг — разовая операция на
// Enqueue (см. комментарий ниже), доставка асинхронна и не видит исходный
// ctx запроса.
func (s *Service) Enqueue(ctx context.Context, req *notificationsv1.SendRequest) error {
	if _, ok := s.senders[req.GetChannel()]; !ok {
		return fmt.Errorf("%w: %v", errUnknownChannel, req.GetChannel())
	}

	subject, body, err := s.renderer.Render(req.GetTemplateKey(), req.GetChannel(), locale.FromContext(ctx), req.GetVariables())
	if err != nil {
		return fmt.Errorf("notifications: render template: %w", err)
	}

	data, err := json.Marshal(queuedMessage{Destination: req.GetDestination(), Subject: subject, Body: body})
	if err != nil {
		return fmt.Errorf("notifications: marshal queued message: %w", err)
	}
	natsSubject := channelSubject(s.env, req.GetChannel())
	if err := mq.Publish(ctx, s.js, natsSubject, data); err != nil {
		return fmt.Errorf("notifications: enqueue: %w", err)
	}
	return nil
}

// deliverFor возвращает mq.Handler очереди канала channel: распаковывает
// queuedMessage и отправляет через соответствующий Sender. Ошибка — сигнал
// для internal/pkg/mq вернуть сообщение в очередь (Nak) и повторить позже,
// вплоть до MaxDeliver попыток.
func (s *Service) deliverFor(channel deliveryv1.DeliveryChannel) mq.Handler {
	snd := s.senders[channel]
	return func(ctx context.Context, data []byte) error {
		var msg queuedMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			// Испорченное сообщение никогда не распарсится заново —
			// повторные попытки не помогут, но и завершить их раньше
			// MaxDeliver пакет mq не даёт. Логируем и считаем
			// "обработанным", чтобы не застревало в очереди навсегда.
			s.log.Error("Failed to unmarshal queued notification, dropping", "channel", channelToken(channel), "error", err)
			return nil
		}
		return snd.Send(ctx, msg.Destination, msg.Subject, msg.Body)
	}
}
