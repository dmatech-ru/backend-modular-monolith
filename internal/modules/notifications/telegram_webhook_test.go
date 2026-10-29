package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeChatLinker — telegramChatLinker в памяти для тестов: запоминает
// последнюю привязку, чтобы проверить, что handleContact вызвал (или не
// вызвал) Link с ожидаемыми аргументами.
type fakeChatLinker struct {
	mu     sync.Mutex
	phone  string
	chatID int64
	linked bool
	err    error
}

func (f *fakeChatLinker) Link(_ context.Context, phone string, chatID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phone, f.chatID, f.linked = phone, chatID, true
	return f.err
}

func newTestWebhookHandler(linker telegramChatLinker, botAPI *httptest.Server) *telegramWebhookHandler {
	h := newTelegramWebhookHandler(linker, "test-token", "s3cr3t", discardLogger())
	h.baseURL = botAPI.URL
	return h
}

func postUpdate(t *testing.T, h *telegramWebhookHandler, secretToken string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/telegram/webhook", strings.NewReader(body))
	if secretToken != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secretToken)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTelegramWebhook_RejectsWrongSecretToken(t *testing.T) {
	linker := &fakeChatLinker{}
	botAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not call the Telegram API when the secret token is wrong")
	}))
	defer botAPI.Close()
	h := newTestWebhookHandler(linker, botAPI)

	rec := postUpdate(t, h, "wrong", `{"message":{"chat":{"id":1},"text":"/start"}}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if linker.linked {
		t.Fatal("must not link anything when the secret token is wrong")
	}
}

func TestTelegramWebhook_MalformedBodyIsAcknowledged(t *testing.T) {
	linker := &fakeChatLinker{}
	botAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer botAPI.Close()
	h := newTestWebhookHandler(linker, botAPI)

	rec := postUpdate(t, h, "s3cr3t", `not json`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (Telegram must not be told to retry)", rec.Code, http.StatusOK)
	}
}

func TestTelegramWebhook_Start_PromptsForContact(t *testing.T) {
	linker := &fakeChatLinker{}
	var gotBody telegramOutgoingMessage
	botAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer botAPI.Close()
	h := newTestWebhookHandler(linker, botAPI)

	rec := postUpdate(t, h, "s3cr3t", `{"message":{"chat":{"id":42},"text":"/start"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotBody.ChatID != 42 {
		t.Fatalf("chat_id = %d, want 42", gotBody.ChatID)
	}
	if gotBody.ReplyMarkup == nil {
		t.Fatal("expected a reply_markup requesting the contact")
	}
	if linker.linked {
		t.Fatal("must not link anything on /start alone")
	}
}

func TestTelegramWebhook_OwnContact_Links(t *testing.T) {
	linker := &fakeChatLinker{}
	var gotBody telegramOutgoingMessage
	botAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer botAPI.Close()
	h := newTestWebhookHandler(linker, botAPI)

	rec := postUpdate(t, h, "s3cr3t", `{"message":{"chat":{"id":42},"contact":{"phone_number":"+79990001234","user_id":42}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !linker.linked || linker.phone != "+79990001234" || linker.chatID != 42 {
		t.Fatalf("linked = %v, phone = %q, chatID = %d, want linked with +79990001234/42", linker.linked, linker.phone, linker.chatID)
	}
	if gotBody.ReplyMarkup != nil {
		t.Fatal("the confirmation message must not request anything else")
	}
}

// Контакт без user_id — переслан от кого-то другого, не получен через
// кнопку "Поделиться номером" (см. doc-комментарий handleContact):
// привязывать такой контакт — дырка, позволяющая привязать чужой номер.
func TestTelegramWebhook_ForwardedContact_IsRejected(t *testing.T) {
	linker := &fakeChatLinker{}
	botAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer botAPI.Close()
	h := newTestWebhookHandler(linker, botAPI)

	rec := postUpdate(t, h, "s3cr3t", `{"message":{"chat":{"id":42},"contact":{"phone_number":"+79990001234","user_id":0}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if linker.linked {
		t.Fatal("must not link a forwarded (not request_contact) contact")
	}
}
