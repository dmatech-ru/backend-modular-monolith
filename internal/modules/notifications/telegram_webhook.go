package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Тексты ответов бота — только на дефолтной локали (ru). В отличие от
// шаблонов notifications/template, у этого диалога нет сигнала локали
// вообще: это первое сообщение, которым пользователь обращается к боту, до
// какой-либо связи с его аккаунтом/запросом — брать локаль неоткуда (та же
// граница, что описана в doc-комментарии пакета locale про уведомления, не
// инициированные входящим запросом). Усложнять этот диалог ради единственной
// локали, которой сегодня никто не попросил, не стали — см. README.
const (
	telegramShareButtonText      = "Поделиться номером"
	telegramTextSharePrompt      = "Чтобы получать коды подтверждения и восстановления в Telegram, поделитесь своим номером телефона — он должен совпадать с номером в вашем аккаунте."
	telegramTextLinked           = "Готово! Теперь коды могут приходить сюда."
	telegramTextContactMustBeOwn = "Поделитесь, пожалуйста, своим собственным номером через кнопку ниже, а не пересланным контактом."
)

// telegramUpdate — подмножество полей Telegram Update, которое здесь
// нужно: сообщение с /start (ответ — просьба поделиться контактом) или с
// contact (сам контакт, см. handleContact). Остальные типы обновлений
// (отредактированные сообщения, callback-запросы и т.п.) не разбираются —
// вебхук их молча игнорирует.
type telegramUpdate struct {
	Message *telegramMessage `json:"message"`
}

type telegramMessage struct {
	Chat    telegramChat     `json:"chat"`
	Text    string           `json:"text"`
	Contact *telegramContact `json:"contact"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

// telegramContact — UserID заполнен Telegram только тогда, когда контакт
// получен через кнопку "Поделиться номером" (request_contact), а не когда
// собеседник переслал боту чужую визитку — см. doc-комментарий handleContact.
type telegramContact struct {
	PhoneNumber string `json:"phone_number"`
	UserID      int64  `json:"user_id"`
}

type telegramKeyboardButton struct {
	Text           string `json:"text"`
	RequestContact bool   `json:"request_contact,omitempty"`
}

type telegramReplyKeyboardMarkup struct {
	Keyboard        [][]telegramKeyboardButton `json:"keyboard"`
	ResizeKeyboard  bool                       `json:"resize_keyboard"`
	OneTimeKeyboard bool                       `json:"one_time_keyboard"`
}

type telegramOutgoingMessage struct {
	ChatID      int64       `json:"chat_id"`
	Text        string      `json:"text"`
	ReplyMarkup interface{} `json:"reply_markup,omitempty"`
}

// telegramChatLinker — то немногое от telegramChatStore, что нужно вебхуку
// (см. handleContact). Отдельный интерфейс, не конкретный тип: тесты
// обработчика проверяют только маршрутизацию обновлений Telegram и текст
// ответов, им не нужен настоящий NATS JetStream KV ради одного метода.
type telegramChatLinker interface {
	Link(ctx context.Context, phone string, chatID int64) error
}

// telegramWebhookHandler обслуживает POST от Telegram Bot API (см.
// doc-комментарий registerTelegramWebhook): на /start отвечает просьбой
// поделиться контактом через reply-клавиатуру, на сообщение с contact —
// привязывает номер к chat_id (telegramChatLinker.Link) и подтверждает это.
//
// Не переиспользует sender.TelegramSender: тот резолвит chat_id по номеру
// и шлёт голый текст без reply_markup — здесь chat_id уже известен из
// самого входящего обновления, а ответы боту нужны с клавиатурой, которой
// у TelegramSender попросту нет (и не должно быть — это другая, не связанная
// с доставкой уведомлений, обязанность).
type telegramWebhookHandler struct {
	store       telegramChatLinker
	botToken    string
	secretToken string
	httpClient  *http.Client
	// baseURL — https://api.telegram.org по умолчанию; переопределяется в
	// тестах тем же приёмом, что sender.TelegramSender.baseURL, без
	// обращения к реальному Telegram Bot API.
	baseURL string
	log     *slog.Logger
}

func newTelegramWebhookHandler(store telegramChatLinker, botToken, secretToken string, log *slog.Logger) *telegramWebhookHandler {
	return &telegramWebhookHandler{
		store:       store,
		botToken:    botToken,
		secretToken: secretToken,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		baseURL:     "https://api.telegram.org",
		log:         log,
	}
}

// newTelegramWebhookMux собирает mux с единственным маршрутом — путь
// настраивается (TelegramWebhookConfig.Path), а не захардкожен, чтобы его
// можно было сменить без пересборки, если понадобится (например, при
// миграции на другого бота).
func newTelegramWebhookMux(store telegramChatLinker, botToken, secretToken, path string, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(path, newTelegramWebhookHandler(store, botToken, secretToken, log))
	return mux
}

func (h *telegramWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// secretToken непустой всегда — вебхук не поднимается без него (см.
	// TelegramWebhookConfig.SecretToken). Без этой проверки вебхук был бы
	// открыт для любого, кто узнает его URL: это настоящий публичный
	// HTTP-эндпоинт (не внутренний NATS-вызов), и ровно он пишет в
	// telegram_chats — без проверки кто угодно мог бы привязать
	// произвольный номер к произвольному chat_id.
	if r.Header.Get("X-Telegram-Bot-Api-Secret-Token") != h.secretToken {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var upd telegramUpdate
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&upd); err != nil {
		// Telegram теряет интерес к вебхуку (вплоть до автоматического
		// отключения), если тот стабильно отвечает не 2xx — для
		// обновления, которое мы не смогли разобрать, повторная доставка
		// всё равно не поможет, поэтому молча подтверждаем приём.
		w.WriteHeader(http.StatusOK)
		return
	}
	// Отвечаем сразу: дальнейшая обработка (которая сама зовёт Telegram Bot
	// API — см. sendText/sendContactRequest) не должна рисковать тем, что
	// сам вебхук будет сочтён зависшим из-за латентности исходящего вызова.
	w.WriteHeader(http.StatusOK)

	if upd.Message == nil {
		return
	}

	ctx := r.Context()
	switch {
	case upd.Message.Contact != nil:
		h.handleContact(ctx, *upd.Message)
	case strings.HasPrefix(upd.Message.Text, "/start"):
		h.sendContactRequest(ctx, upd.Message.Chat.ID, telegramTextSharePrompt)
	}
}

// handleContact привязывает номер к чату, если контакт — собственный номер
// отправителя. contact.user_id Telegram заполняет только тогда, когда
// контакт получен нажатием кнопки "Поделиться номером" (request_contact) —
// если бы мы принимали любой присланный контакт, кто угодно мог бы
// привязать произвольный чужой номер к своему чату, переслав его визитку.
func (h *telegramWebhookHandler) handleContact(ctx context.Context, msg telegramMessage) {
	if msg.Contact.UserID == 0 {
		h.sendText(ctx, msg.Chat.ID, telegramTextContactMustBeOwn)
		return
	}
	if err := h.store.Link(ctx, msg.Contact.PhoneNumber, msg.Chat.ID); err != nil {
		h.log.Error("не удалось привязать Telegram-чат к номеру", "error", err)
		return
	}
	h.sendText(ctx, msg.Chat.ID, telegramTextLinked)
}

func (h *telegramWebhookHandler) sendText(ctx context.Context, chatID int64, text string) {
	h.call(ctx, telegramOutgoingMessage{ChatID: chatID, Text: text})
}

func (h *telegramWebhookHandler) sendContactRequest(ctx context.Context, chatID int64, text string) {
	h.call(ctx, telegramOutgoingMessage{
		ChatID: chatID,
		Text:   text,
		ReplyMarkup: telegramReplyKeyboardMarkup{
			Keyboard:        [][]telegramKeyboardButton{{{Text: telegramShareButtonText, RequestContact: true}}},
			ResizeKeyboard:  true,
			OneTimeKeyboard: true,
		},
	})
}

func (h *telegramWebhookHandler) call(ctx context.Context, payload telegramOutgoingMessage) {
	data, err := json.Marshal(payload)
	if err != nil {
		h.log.Error("не удалось собрать ответ Telegram-боту", "error", err)
		return
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", h.baseURL, h.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		h.log.Error("не удалось собрать запрос к Telegram Bot API", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		h.log.Error("Telegram Bot API недоступен", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		h.log.Error("Telegram Bot API отверг сообщение боту", "status", resp.StatusCode, "body", string(body))
	}
}

// registerTelegramWebhook сообщает Telegram Bot API, куда слать обновления
// (setWebhook) — вызывается один раз при старте модуля, идемпотентно:
// повторный вызов с тем же url просто подтверждает текущую настройку, так
// что звонить на каждый старт каждой реплики безопасно. secretToken
// передаётся Telegram'у и приходит обратно в заголовке на каждый вызов
// вебхука (см. telegramWebhookHandler.ServeHTTP) — так вебхук отличает
// настоящий Telegram от случайного запроса на его публичный URL.
//
// Ошибка не фатальна для старта модуля: Telegram может быть временно
// недоступен именно в момент деплоя, а это не должно ронять email/SMS —
// попробовать снова можно вызвав setWebhook вручную, не передеплоя.
func registerTelegramWebhook(ctx context.Context, botToken, publicURL, secretToken string, log *slog.Logger) {
	payload, err := json.Marshal(struct {
		URL         string `json:"url"`
		SecretToken string `json:"secret_token"`
	}{URL: publicURL, SecretToken: secretToken})
	if err != nil {
		log.Error("не удалось собрать запрос setWebhook", "error", err)
		return
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/setWebhook", botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		log.Error("не удалось собрать запрос setWebhook", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		log.Error("Telegram Bot API недоступен при регистрации вебхука", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Error("Telegram отверг регистрацию вебхука (setWebhook)", "status", resp.StatusCode, "body", string(body))
		return
	}
	log.Info("Telegram-вебхук зарегистрирован", "url", publicURL)
}
