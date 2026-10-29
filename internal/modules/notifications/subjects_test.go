package notifications

import (
	"io"
	"log/slog"
	"testing"

	"dmatech.ru/internal/modules/notifications/sender"
	deliveryv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/delivery"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestChannelSubject_IsNamespacedByEnvAndChannel(t *testing.T) {
	got := channelSubject("production", deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL)
	want := "production.notifications.email"
	if got != want {
		t.Fatalf("channelSubject = %q, want %q", got, want)
	}
}

func TestChannelSubject_DifferentEnvsDoNotCollide(t *testing.T) {
	dev := channelSubject("development", deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS)
	prod := channelSubject("production", deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS)
	if dev == prod {
		t.Fatalf("subjects for different envs must differ, both got %q", dev)
	}
}

func TestChannelSubject_SanitizesEnvToken(t *testing.T) {
	got := channelSubject("my.env with spaces", deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_MAX)
	want := "my_env_with_spaces.notifications.max"
	if got != want {
		t.Fatalf("channelSubject = %q, want %q", got, want)
	}
}

func TestStreamSubjectsWildcard_MatchesAllChannelSubjects(t *testing.T) {
	// Подтверждаем на реальном NATS-синтаксисе: "env.notifications.*" должен
	// совпадать по длине префикса с каждым channelSubject того же env.
	wildcard := streamSubjectsWildcard("test")
	wantPrefix := "test.notifications."
	if wildcard != wantPrefix+"*" {
		t.Fatalf("streamSubjectsWildcard = %q, want %q", wildcard, wantPrefix+"*")
	}
	for _, ch := range allChannels {
		subj := channelSubject("test", ch)
		if len(subj) <= len(wantPrefix) || subj[:len(wantPrefix)] != wantPrefix {
			t.Fatalf("channel subject %q does not match wildcard prefix %q", subj, wantPrefix)
		}
	}
}

func TestAllChannels_HaveDistinctTokens(t *testing.T) {
	seen := make(map[string]bool)
	for _, ch := range allChannels {
		token := channelToken(ch)
		if token == "" || token == "unknown" {
			t.Fatalf("channel %v produced an empty/unknown token", ch)
		}
		if seen[token] {
			t.Fatalf("duplicate token %q for channel %v", token, ch)
		}
		seen[token] = true
	}
}

func TestBuildSenders_UsesLogSenderWhenProvidersNotConfigured(t *testing.T) {
	cfg := &configuration{}
	senders := buildSenders(cfg, discardLogger(), nil)

	for _, ch := range allChannels {
		if _, ok := senders[ch].(*sender.LogSender); !ok {
			t.Fatalf("channel %v: got %T, want *sender.LogSender when no provider is configured", ch, senders[ch])
		}
	}
}

func TestBuildSenders_UsesRealSendersWhenConfigured(t *testing.T) {
	cfg := &configuration{
		Email:    sender.EmailConfig{Host: "smtp.example.com"},
		Telegram: sender.TelegramConfig{BotToken: "test-token"},
	}
	senders := buildSenders(cfg, discardLogger(), nil)

	if _, ok := senders[deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL].(*sender.EmailSender); !ok {
		t.Fatalf("email: got %T, want *sender.EmailSender", senders[deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL])
	}
	if _, ok := senders[deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_TELEGRAM].(*sender.TelegramSender); !ok {
		t.Fatalf("telegram: got %T, want *sender.TelegramSender", senders[deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_TELEGRAM])
	}
	// SMS/MAX не сконфигурированы (и не могут быть в этой итерации) — остаются логом.
	if _, ok := senders[deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS].(*sender.LogSender); !ok {
		t.Fatalf("sms: got %T, want *sender.LogSender", senders[deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS])
	}
}
