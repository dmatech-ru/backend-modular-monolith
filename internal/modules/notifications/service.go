package notifications

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"dmatech.ru/internal/modules/notifications/sender"
	notificationsv1 "dmatech.ru/internal/pkg/contracts/peer/v1/notifications"
	deliveryv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/delivery"
	"dmatech.ru/internal/pkg/mq"
)

// Service — доменная логика notifications: Enqueue сериализует сообщение
// в subject своего канала (см. subjects.go) и сразу отвечает —
// подтверждённое попадание в очередь, не подтверждение реальной доставки
// (см. doc-комментарий NotificationService в notifications.proto). deliver
// — обработчик очереди на стороне каждого канала, вызывается
// internal/pkg/mq.Consumer; какой Sender соответствует какому каналу,
// задаётся при старте модуля (см. notifications.go) конфигом.
type Service struct {
	js      jetstream.JetStream
	env     string
	senders map[deliveryv1.DeliveryChannel]sender.Sender
	log     *slog.Logger
}

func NewService(js jetstream.JetStream, env string, senders map[deliveryv1.DeliveryChannel]sender.Sender, log *slog.Logger) *Service {
	return &Service{js: js, env: env, senders: senders, log: log}
}

// Enqueue сериализует req и кладёт в очередь канала req.Channel.
func (s *Service) Enqueue(ctx context.Context, req *notificationsv1.SendRequest) error {
	if _, ok := s.senders[req.GetChannel()]; !ok {
		return fmt.Errorf("%w: %v", errUnknownChannel, req.GetChannel())
	}

	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("notifications: marshal request: %w", err)
	}
	subject := channelSubject(s.env, req.GetChannel())
	if err := mq.Publish(ctx, s.js, subject, data); err != nil {
		return fmt.Errorf("notifications: enqueue: %w", err)
	}
	return nil
}

// deliverFor возвращает mq.Handler очереди канала channel: распаковывает
// SendRequest и отправляет через соответствующий Sender. Ошибка — сигнал
// для internal/pkg/mq вернуть сообщение в очередь (Nak) и повторить позже,
// вплоть до MaxDeliver попыток.
func (s *Service) deliverFor(channel deliveryv1.DeliveryChannel) mq.Handler {
	snd := s.senders[channel]
	return func(ctx context.Context, data []byte) error {
		var req notificationsv1.SendRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			// Испорченное сообщение никогда не распарсится заново —
			// повторные попытки не помогут, но и завершить их раньше
			// MaxDeliver пакет mq не даёт. Логируем и считаем
			// "обработанным", чтобы не застревало в очереди навсегда.
			s.log.Error("Failed to unmarshal queued notification, dropping", "channel", channelToken(channel), "error", err)
			return nil
		}
		return snd.Send(ctx, req.GetDestination(), req.GetSubject(), req.GetBody())
	}
}
