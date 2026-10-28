package sender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrChatNotResolved — для destination (номера телефона) ещё нет известного
// chat_id. Резолвинг телефон → chat_id происходит через штатный механизм
// Telegram-бота ("поделиться контактом", см. doc-комментарий
// peer.notifications.v1.NotificationService в notifications.proto) — то
// есть через вебхук, который получает обновления от Telegram. Этот вебхук
// в данной итерации не реализован (нужен отдельный HTTP-эндпоинт,
// зарегистрированный в Telegram Bot API, и хранилище номер→chat_id) —
// поэтому ChatResolver здесь намеренно пустой: Send полностью готов к
// реальной отправке, но реально сработает только после того, как
// появится то, что заполняет резолвер.
var ErrChatNotResolved = errors.New("sender: no telegram chat is known for this destination yet")

// TelegramConfig — настройки Telegram Bot API. Если BotToken пуст,
// вызывающий код (notifications-модуль) использует LogSender вместо
// TelegramSender.
type TelegramConfig struct {
	BotToken string `yaml:"botToken" env:"NOTIFICATIONS_TELEGRAM_BOT_TOKEN"`
}

// ChatResolver находит chat_id Telegram-бота для контакта пользователя
// (номер телефона — тот же destination, что приходит в SendRequest).
type ChatResolver interface {
	Resolve(ctx context.Context, destination string) (chatID int64, err error)
}

// unresolvedChatResolver — единственная реализация ChatResolver в этом
// репозитории: всегда ErrChatNotResolved, см. комментарий у него. Как
// только появится вебхук-обработчик бота, он и будет писать сюда реальные
// соответствия (например, через NATS KV, как captcha хранит challenge'и) —
// TelegramSender от этого не изменится, только резолвер.
type unresolvedChatResolver struct{}

func (unresolvedChatResolver) Resolve(context.Context, string) (int64, error) {
	return 0, ErrChatNotResolved
}

// TelegramSender отправляет сообщения через Telegram Bot API
// (sendMessage). httpClient/baseURL — для подмены в тестах без реального
// обращения к api.telegram.org.
type TelegramSender struct {
	botToken   string
	resolver   ChatResolver
	httpClient *http.Client
	baseURL    string
}

func NewTelegramSender(cfg TelegramConfig, resolver ChatResolver) *TelegramSender {
	if resolver == nil {
		resolver = unresolvedChatResolver{}
	}
	return &TelegramSender{
		botToken:   cfg.BotToken,
		resolver:   resolver,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    "https://api.telegram.org",
	}
}

type telegramSendMessageRequest struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

type telegramAPIResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

func (s *TelegramSender) Send(ctx context.Context, destination, subject, body string) error {
	chatID, err := s.resolver.Resolve(ctx, destination)
	if err != nil {
		return fmt.Errorf("sender: resolve telegram chat: %w", err)
	}

	text := body
	if subject != "" {
		text = subject + "\n\n" + body
	}
	payload, err := json.Marshal(telegramSendMessageRequest{ChatID: chatID, Text: text})
	if err != nil {
		return fmt.Errorf("sender: marshal telegram payload: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", s.baseURL, s.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("sender: build telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sender: call telegram api: %w", err)
	}
	defer resp.Body.Close()

	var apiResp telegramAPIResponse
	_ = json.NewDecoder(resp.Body).Decode(&apiResp)
	if resp.StatusCode != http.StatusOK || !apiResp.OK {
		return fmt.Errorf("sender: telegram api rejected the message (status %d): %s", resp.StatusCode, apiResp.Description)
	}
	return nil
}
