package notifications

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"connectrpc.com/connect"

	peerhandler "dmatech.ru/internal/modules/notifications/handlers/peer/v1"
	"dmatech.ru/internal/modules/notifications/sender"
	"dmatech.ru/internal/modules/notifications/template"
	deliveryv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/delivery"

	"dmatech.ru/internal/pkg/config"
	"dmatech.ru/internal/pkg/locale"
	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/mq"
	"dmatech.ru/internal/pkg/natsrpc"
	"dmatech.ru/internal/pkg/service"
)

const configPathEnv = "NOTIFICATIONS_CONFIG_PATH"

func init() {
	service.Register(initialization)
}

func initialization(ctx context.Context) error {
	cfg, err := config.Load[configuration](configPathEnv)
	if err != nil {
		return fmt.Errorf("notifications config: %w", err)
	}
	log := logger.NewModuleLogger(cfg.ServiceName, cfg.LogLevel)

	nc, err := service.Get[nats.Conn]()
	if err != nil {
		return fmt.Errorf("notifications: %w", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("notifications: jetstream: %w", err)
	}

	// Один стрим на всё окружение, subject на канал внутри него (см.
	// subjects.go) — отдельный консьюмер на канал ниже фильтруется по
	// своему subject'у, чтобы медленный/проблемный канал (например,
	// недоступный Telegram Bot API) не блокировал обработку остальных.
	stream, err := mq.EnsureStream(ctx, js, mq.StreamConfig{
		Name:     cfg.StreamName,
		Subjects: []string{streamSubjectsWildcard(cfg.Env)},
	})
	if err != nil {
		return fmt.Errorf("notifications: %w", err)
	}

	renderer, err := template.New()
	if err != nil {
		return fmt.Errorf("notifications: %w", err)
	}

	senders := buildSenders(cfg, log)
	svc := NewService(js, cfg.Env, renderer, senders, log)

	// Нет discovery/webapi-регистрации: у notifications нет webapi-файла
	// вообще (см. notifications.proto) — гейтвею здесь физически нечего
	// проксировать, вызывающие модули адресуются напрямую по имени/версии
	// из своего конфига (как identity вызывает captcha, см.
	// internal/modules/identity/captcha_client.go).
	// locale.ServerInterceptor кладёт локаль запроса (перенесённую через
	// заголовок исходящего вызова identity, см. locale.ClientInterceptor в
	// identity/notifications_client.go) в context.Context — Service.Enqueue
	// читает её оттуда, чтобы выбрать локализованный вариант шаблона (см.
	// internal/modules/notifications/template и doc-комментарий locale).
	_, handler := peerhandler.NewHandler(svc, connect.WithInterceptors(locale.ServerInterceptor()))
	sub, err := natsrpc.ServeHandler(nc, log, cfg.Env, cfg.ServiceName, cfg.AppVersion, handler)
	if err != nil {
		return fmt.Errorf("failed to subscribe NotificationService: %w", err)
	}
	service.OnShutdown(sub.Close)

	// durable-имя консьюмера общее для всех реплик этой версии — JetStream
	// сам балансирует сообщения между ними (см. internal/pkg/mq). Отдельный
	// консьюмер на канал, не один общий, — см. комментарий у EnsureStream выше.
	for channel := range senders {
		consumer, err := mq.StartConsumer(ctx, stream, mq.ConsumerOptions{
			Durable:       "deliver_" + channelToken(channel),
			MaxDeliver:    cfg.MaxDeliver,
			FilterSubject: channelSubject(cfg.Env, channel),
		}, svc.deliverFor(channel))
		if err != nil {
			return fmt.Errorf("notifications: start consumer for %s: %w", channelToken(channel), err)
		}
		service.OnShutdown(consumer.Close)
	}

	log.Info("Модуль успешно запущен")
	return nil
}

// buildSenders — Email/Telegram получают настоящую реализацию, только если
// провайдер сконфигурирован (непустой Host/BotToken); иначе, как SMS/MAX
// всегда, — logSender. Так локальная разработка без настоящих SMTP/Telegram
// credentials не ломается, но и не притворяется, что отправка настоящая.
func buildSenders(cfg *configuration, log *slog.Logger) map[deliveryv1.DeliveryChannel]sender.Sender {
	senders := make(map[deliveryv1.DeliveryChannel]sender.Sender, len(allChannels))
	for _, channel := range allChannels {
		senders[channel] = sender.NewLogSender(log, channelToken(channel))
	}

	if cfg.Email.Host != "" {
		senders[deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL] = sender.NewEmailSender(cfg.Email)
	} else {
		log.Warn("SMTP-релей не настроен (email.host пуст) — письма только логируются")
	}

	if cfg.Telegram.BotToken != "" {
		senders[deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_TELEGRAM] = sender.NewTelegramSender(cfg.Telegram, nil)
	} else {
		log.Warn("Telegram Bot API не настроен (telegram.botToken пуст) — сообщения только логируются")
	}

	return senders
}
