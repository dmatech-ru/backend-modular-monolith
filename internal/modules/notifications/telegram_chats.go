package notifications

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"dmatech.ru/internal/modules/notifications/sender"
)

// telegramChatsBucket — NATS JetStream KV-бакет для привязок телефон→chat_id
// Telegram-бота (пишет telegram_webhook.go, когда пользователь делится
// контактом). Без TTL, в отличие от captcha_challenges: привязка не
// одноразовая и сама не истекает — живёт, пока пользователь не поделится
// контактом повторно (перезапишет) или не отзовёт бота на своей стороне
// (Telegram ничего не присылает об этом вебхуком — отвязка в этой
// итерации не реализована).
const telegramChatsBucket = "telegram_chats"

// nonDigits — всё, что не цифра: нормализация телефона для ключа KV (см.
// normalizePhone).
var nonDigits = regexp.MustCompile(`[^0-9]`)

// normalizePhone приводит телефон к ключу KV — только цифры, без "+" и
// разделителей. Нужно, потому что два источника одного и того же номера
// расходятся по форме: identity.auth.phone — то, что ввёл пользователь на
// сайте (recovery.proto не навязывает формат, см. buf.validate там — только
// непустое значение), а Telegram-овский contact.phone_number — то, что
// отдаёт клиент Telegram (с "+" или без, в зависимости от клиента/региона).
// Привязка (Link) и резолвинг (Resolve) должны прийти к одному и тому же
// ключу для того же физического номера, иначе они просто не совпадут.
func normalizePhone(phone string) string {
	return nonDigits.ReplaceAllString(phone, "")
}

// telegramChatStore реализует sender.ChatResolver поверх NATS JetStream KV
// — тот же приём, что captcha.kvStore для challenge'ей (см.
// internal/modules/captcha/store.go), только без TTL и без one-shot чтения:
// привязка используется многократно, пока не будет перезаписана.
type telegramChatStore struct {
	kv jetstream.KeyValue
}

func newTelegramChatStore(ctx context.Context, nc *nats.Conn) (*telegramChatStore, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("notifications: jetstream: %w", err)
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      telegramChatsBucket,
		Description: "Notifications: phone number -> Telegram chat_id bindings (bot contact share)",
	})
	if err != nil {
		return nil, fmt.Errorf("notifications: open kv bucket %q: %w", telegramChatsBucket, err)
	}
	return &telegramChatStore{kv: kv}, nil
}

// Link запоминает chat_id для phone — вызывается вебхуком (telegram_webhook.go),
// когда пользователь делится контактом с ботом. Put, не Create: повторная
// привязка того же номера (например, после переустановки Telegram или со
// второго устройства) просто перезаписывает старый chat_id, без ошибки.
func (s *telegramChatStore) Link(ctx context.Context, phone string, chatID int64) error {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(chatID))
	if _, err := s.kv.Put(ctx, normalizePhone(phone), buf); err != nil {
		return fmt.Errorf("notifications: link telegram chat: %w", err)
	}
	return nil
}

// Resolve реализует sender.ChatResolver.
func (s *telegramChatStore) Resolve(ctx context.Context, phone string) (int64, error) {
	entry, err := s.kv.Get(ctx, normalizePhone(phone))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return 0, sender.ErrChatNotResolved
	}
	if err != nil {
		return 0, fmt.Errorf("notifications: resolve telegram chat: %w", err)
	}
	data := entry.Value()
	if len(data) != 8 {
		return 0, fmt.Errorf("notifications: corrupt telegram chat record (%d bytes)", len(data))
	}
	return int64(binary.LittleEndian.Uint64(data)), nil
}

var _ sender.ChatResolver = (*telegramChatStore)(nil)
