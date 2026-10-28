package mq

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"
)

// Интеграционные тесты в этом пакете требуют NATS с JetStream:
//
//	NATS_TEST_URL=nats://localhost:4222 go test ./internal/pkg/mq/
func connectTestJS(t *testing.T) jetstream.JetStream {
	t.Helper()
	url := os.Getenv("NATS_TEST_URL")
	if url == "" {
		t.Skip("NATS_TEST_URL is not set")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}

func uniqueName(prefix string) string { return prefix + "_" + nuid.Next() }

func TestMQ_PublishAndConsume_RoundTrip(t *testing.T) {
	js := connectTestJS(t)
	ctx := context.Background()
	subject := uniqueName("mq_test")

	stream, err := EnsureStream(ctx, js, StreamConfig{Name: subject, Subjects: []string{subject}})
	if err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	received := make(chan string, 1)
	consumer, err := StartConsumer(ctx, stream, ConsumerOptions{Durable: "worker", MaxDeliver: 5}, func(_ context.Context, data []byte) error {
		received <- string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("StartConsumer: %v", err)
	}
	defer consumer.Close(context.Background())

	if err := Publish(ctx, js, subject, []byte("hello")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-received:
		if got != "hello" {
			t.Fatalf("received = %q, want %q", got, "hello")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the message to be consumed")
	}
}

func TestMQ_HandlerError_IsRedelivered(t *testing.T) {
	js := connectTestJS(t)
	ctx := context.Background()
	subject := uniqueName("mq_test")

	stream, err := EnsureStream(ctx, js, StreamConfig{Name: subject, Subjects: []string{subject}})
	if err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	var attempts atomic.Int32
	succeeded := make(chan struct{}, 1)
	consumer, err := StartConsumer(ctx, stream, ConsumerOptions{Durable: "worker", MaxDeliver: 5}, func(_ context.Context, _ []byte) error {
		n := attempts.Add(1)
		if n < 3 {
			return errReject // первые попытки — неудача, должна прийти редоставка
		}
		close(succeeded)
		return nil
	})
	if err != nil {
		t.Fatalf("StartConsumer: %v", err)
	}
	defer consumer.Close(context.Background())

	if err := Publish(ctx, js, subject, []byte("retry-me")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case <-succeeded:
		if n := attempts.Load(); n != 3 {
			t.Fatalf("attempts = %d, want 3 (2 failures + 1 success)", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for redelivery; attempts so far = %d", attempts.Load())
	}
}

func TestMQ_MaxDeliver_StopsRedeliveryEventually(t *testing.T) {
	js := connectTestJS(t)
	ctx := context.Background()
	subject := uniqueName("mq_test")

	stream, err := EnsureStream(ctx, js, StreamConfig{Name: subject, Subjects: []string{subject}})
	if err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	var mu sync.Mutex
	var attempts int
	const maxDeliver = 2

	consumer, err := StartConsumer(ctx, stream, ConsumerOptions{Durable: "worker", MaxDeliver: maxDeliver}, func(_ context.Context, _ []byte) error {
		mu.Lock()
		attempts++
		mu.Unlock()
		return errReject // всегда неудача
	})
	if err != nil {
		t.Fatalf("StartConsumer: %v", err)
	}
	defer consumer.Close(context.Background())

	if err := Publish(ctx, js, subject, []byte("always-fails")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Ждём дольше, чем нужно было бы на maxDeliver попыток, и убеждаемся,
	// что их действительно не больше maxDeliver.
	time.Sleep(3 * time.Second)

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got > maxDeliver {
		t.Fatalf("attempts = %d, want <= %d (MaxDeliver must stop redelivery)", got, maxDeliver)
	}
	if got == 0 {
		t.Fatal("handler was never called")
	}
}

// Несколько консьюмеров одного стрима, каждый со своим FilterSubject, не
// должны видеть "чужие" сообщения — ровно то, ради чего разводятся типы
// сообщений (например, каналы доставки в notifications) по subject'ам
// внутри одного стрима.
func TestMQ_FilterSubject_IsolatesConsumers(t *testing.T) {
	js := connectTestJS(t)
	ctx := context.Background()
	base := uniqueName("mq_test")
	subjectA, subjectB := base+".a", base+".b"

	stream, err := EnsureStream(ctx, js, StreamConfig{Name: base, Subjects: []string{base + ".*"}})
	if err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	gotA := make(chan string, 1)
	consumerA, err := StartConsumer(ctx, stream, ConsumerOptions{Durable: "a", FilterSubject: subjectA}, func(_ context.Context, data []byte) error {
		gotA <- string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("StartConsumer A: %v", err)
	}
	defer consumerA.Close(context.Background())

	gotB := make(chan string, 1)
	consumerB, err := StartConsumer(ctx, stream, ConsumerOptions{Durable: "b", FilterSubject: subjectB}, func(_ context.Context, data []byte) error {
		gotB <- string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("StartConsumer B: %v", err)
	}
	defer consumerB.Close(context.Background())

	if err := Publish(ctx, js, subjectA, []byte("for-a")); err != nil {
		t.Fatalf("Publish A: %v", err)
	}

	select {
	case got := <-gotA:
		if got != "for-a" {
			t.Fatalf("consumer A received = %q, want %q", got, "for-a")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for consumer A")
	}

	select {
	case got := <-gotB:
		t.Fatalf("consumer B must not receive messages published to subjectA, got %q", got)
	case <-time.After(500 * time.Millisecond):
		// ожидаемо: B отфильтрован от A.
	}
}

type rejectError struct{}

func (rejectError) Error() string { return "mq test: reject" }

var errReject = rejectError{}
