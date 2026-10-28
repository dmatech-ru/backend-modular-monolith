// Package mq — durable work-queue поверх NATS JetStream: стрим хранит
// сообщения, pull-консьюмер забирает их и подтверждает (Ack) или отказывает
// (Nak) обработку, непотверждённые сообщения JetStream сам повторно
// доставляет до MaxDeliver попыток.
//
// Осознанно не отдельный брокер (RabbitMQ и т.п.): NATS уже обязательная
// инфраструктура для каждого модуля (service.Get[nats.Conn]), а JetStream
// (WorkQueuePolicy + AckExplicitPolicy + MaxDeliver) даёт ровно ту же
// семантику очереди задач — второй брокер добавил бы отдельный контейнер,
// клиент и набор отказов ради возможностей, которые уже есть.
package mq

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// StreamConfig описывает durable work-queue стрим.
type StreamConfig struct {
	// Name — имя стрима, уникальное в рамках NATS-аккаунта.
	Name string
	// Subjects — subject'ы, публикации в которые стрим сохраняет.
	Subjects []string
}

// EnsureStream создаёt work-queue стрим (WorkQueuePolicy: сообщение
// покидает стрим сразу после подтверждения — это очередь задач, а не
// журнал/лог событий) или возвращает уже существующий с таким именем.
func EnsureStream(ctx context.Context, js jetstream.JetStream, cfg StreamConfig) (jetstream.Stream, error) {
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      cfg.Name,
		Subjects:  cfg.Subjects,
		Retention: jetstream.WorkQueuePolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("mq: ensure stream %q: %w", cfg.Name, err)
	}
	return stream, nil
}

// Publish публикует payload в subject — он должен входить в Subjects
// стрима, созданного EnsureStream, иначе сообщение никто не сохранит.
func Publish(ctx context.Context, js jetstream.JetStream, subject string, payload []byte) error {
	if _, err := js.Publish(ctx, subject, payload); err != nil {
		return fmt.Errorf("mq: publish to %q: %w", subject, err)
	}
	return nil
}

// Handler обрабатывает одно сообщение очереди. Ошибка — сигнал повторить
// доставку (Nak), nil — подтвердить (Ack) и больше не присылать.
type Handler func(ctx context.Context, data []byte) error

// Consumer — durable pull-консьюмер стрима, запущенный StartConsumer.
type Consumer struct {
	cc jetstream.ConsumeContext
}

// ConsumerOptions настраивает durable-консьюмер, запускаемый StartConsumer.
type ConsumerOptions struct {
	// Durable — имя консьюмера; одно и то же нужно использовать во всех
	// репликах модуля — JetStream сам балансирует сообщения между ними,
	// как очередь конкурирующих потребителей.
	Durable string
	// MaxDeliver — сколько раз повторить доставку сообщения, прежде чем
	// JetStream перестанет пытаться (0 — не ограничено).
	MaxDeliver int
	// FilterSubject — если задан, консьюмер получает только сообщения с
	// этим subject'ом (а не весь набор, который принимает стрим через свой,
	// возможно более широкий, Subjects). Так несколько независимых
	// консьюмеров одного стрима разводятся по типам сообщений — медленный
	// или проблемный тип не блокирует обработку остальных.
	FilterSubject string
}

// StartConsumer запускает durable pull-консьюмер по opts.
func StartConsumer(ctx context.Context, stream jetstream.Stream, opts ConsumerOptions, handler Handler) (*Consumer, error) {
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       opts.Durable,
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    opts.MaxDeliver,
		FilterSubject: opts.FilterSubject,
	})
	if err != nil {
		return nil, fmt.Errorf("mq: create consumer %q: %w", opts.Durable, err)
	}

	cc, err := cons.Consume(func(msg jetstream.Msg) {
		if err := handler(context.Background(), msg.Data()); err != nil {
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return nil, fmt.Errorf("mq: consume %q: %w", opts.Durable, err)
	}
	return &Consumer{cc: cc}, nil
}

// Close останавливает консьюмер: Drain — буферизованные сообщения
// успевают обработаться в handler, а не теряются молча. Возвращает
// ctx.Err(), если обработка буфера не успела завершиться к дедлайну ctx.
func (c *Consumer) Close(ctx context.Context) error {
	c.cc.Drain()
	select {
	case <-c.cc.Closed():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
