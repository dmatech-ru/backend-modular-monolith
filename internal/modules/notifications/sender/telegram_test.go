package sender

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeChatResolver struct {
	chatID int64
	err    error
}

func (f fakeChatResolver) Resolve(context.Context, string) (int64, error) {
	return f.chatID, f.err
}

func TestTelegramSender_UnresolvedChatIsRejectedWithoutCallingAPI(t *testing.T) {
	called := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	s := NewTelegramSender(TelegramConfig{BotToken: "test-token"}, nil) // nil -> unresolvedChatResolver
	s.baseURL = api.URL

	err := s.Send(context.Background(), "+70000000000", "Subject", "body")
	if err == nil {
		t.Fatal("expected an error when no chat is resolved for the destination")
	}
	if called {
		t.Fatal("must not call the Telegram API when the chat could not be resolved")
	}
}

func TestTelegramSender_Send_PostsExpectedPayload(t *testing.T) {
	var gotPath string
	var gotBody telegramSendMessageRequest

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telegramAPIResponse{OK: true})
	}))
	defer api.Close()

	s := NewTelegramSender(TelegramConfig{BotToken: "test-token"}, fakeChatResolver{chatID: 42})
	s.baseURL = api.URL

	if err := s.Send(context.Background(), "+70000000000", "Subject", "body text"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotPath != "/bottest-token/sendMessage" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody.ChatID != 42 {
		t.Fatalf("chat_id = %d, want 42", gotBody.ChatID)
	}
	if gotBody.Text != "Subject\n\nbody text" {
		t.Fatalf("text = %q", gotBody.Text)
	}
}

func TestTelegramSender_Send_PropagatesAPIError(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(telegramAPIResponse{OK: false, Description: "chat not found"})
	}))
	defer api.Close()

	s := NewTelegramSender(TelegramConfig{BotToken: "test-token"}, fakeChatResolver{chatID: 42})
	s.baseURL = api.URL

	err := s.Send(context.Background(), "+70000000000", "Subject", "body")
	if err == nil {
		t.Fatal("expected an error when the Telegram API rejects the message")
	}
}
