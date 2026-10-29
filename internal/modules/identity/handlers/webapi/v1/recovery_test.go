package v1

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"dmatech.ru/internal/modules/identity/repository"
	deliveryv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/delivery"
	recoveryv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/recovery"
)

// fakeRecoverySession — состояние identity.recovery_sessions в памяти.
type fakeRecoverySession struct {
	acc          repository.RecoveryAccount
	codeHash     []byte
	attempts     int
	codeVerified bool
}

// fakeRecoveryRepository — RecoveryRepository в памяти для тестов.
type fakeRecoveryRepository struct {
	mu       sync.Mutex
	accounts map[string]repository.RecoveryAccount // identifier -> account
	sessions map[string]*fakeRecoverySession
	nextID   int
}

func newFakeRecoveryRepository() *fakeRecoveryRepository {
	return &fakeRecoveryRepository{
		accounts: map[string]repository.RecoveryAccount{},
		sessions: map[string]*fakeRecoverySession{},
	}
}

func strPtr(s string) *string { return &s }

func (f *fakeRecoveryRepository) FindAuthForRecovery(_ context.Context, identifier string) (repository.RecoveryAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	acc, ok := f.accounts[identifier]
	if !ok {
		return repository.RecoveryAccount{}, repository.ErrNotFound
	}
	return acc, nil
}

func (f *fakeRecoveryRepository) CreateRecoverySession(_ context.Context, acc repository.RecoveryAccount) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("recovery-session-%d", f.nextID)
	f.sessions[id] = &fakeRecoverySession{acc: acc}
	return id, nil
}

func (f *fakeRecoveryRepository) GetRecoverySession(_ context.Context, sessionID string) (repository.RecoverySessionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	if !ok {
		return repository.RecoverySessionRecord{}, repository.ErrNotFound
	}
	return repository.RecoverySessionRecord{RecoveryAccount: s.acc, CodeVerified: s.codeVerified}, nil
}

func (f *fakeRecoveryRepository) SetRecoveryCode(_ context.Context, sessionID string, codeHash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	if !ok {
		return repository.ErrNotFound
	}
	s.codeHash = codeHash
	s.attempts = 0
	s.codeVerified = false
	return nil
}

func (f *fakeRecoveryRepository) VerifyRecoveryCode(_ context.Context, sessionID string, codeHash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	if !ok {
		return repository.ErrNotFound
	}
	if s.attempts >= fakeMaxCodeAttempts {
		return repository.ErrTooManyAttempts
	}
	if !bytes.Equal(s.codeHash, codeHash) {
		s.attempts++
		return repository.ErrCodeMismatch
	}
	s.codeVerified = true
	return nil
}

func (f *fakeRecoveryRepository) ResetRecoveryPassword(_ context.Context, sessionID string, _, _ []byte) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	if !ok {
		return "", time.Time{}, repository.ErrNotFound
	}
	if !s.codeVerified {
		return "", time.Time{}, repository.ErrRecoveryCodeNotVerified
	}
	delete(f.sessions, sessionID)
	return s.acc.UserID, time.Now(), nil
}

// fakeRecoveryCodeSender — RecoveryCodeSender, запоминающий отправленные коды.
type fakeRecoveryCodeSender struct {
	mu   sync.Mutex
	sent map[string]string // destination -> последний отправленный код
	err  error
}

func newFakeRecoveryCodeSender() *fakeRecoveryCodeSender {
	return &fakeRecoveryCodeSender{sent: map[string]string{}}
}

func (f *fakeRecoveryCodeSender) SendCode(_ context.Context, _ deliveryv1.DeliveryChannel, destination, code string) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[destination] = code
	return nil
}

func (f *fakeRecoveryCodeSender) codeFor(destination string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[destination]
}

const testPasswordResetFreeze = 24 * time.Hour

func newTestRecoveryHandler(approveCaptcha bool) (*RecoveryHandler, *fakeRecoveryRepository, *fakeRecoveryCodeSender) {
	repo := newFakeRecoveryRepository()
	codes := newFakeRecoveryCodeSender()
	h := NewRecoveryHandler(repo, &fakeCaptchaClient{result: approveCaptcha}, codes, testPasswordResetFreeze)
	return h, repo, codes
}

func TestInitiateRecovery_RequiresCaptchaWhenNotSolved(t *testing.T) {
	h, _, _ := newTestRecoveryHandler(false)

	resp, err := h.InitiateRecovery(context.Background(), connect.NewRequest(&recoveryv1.InitiateRecoveryRequest{
		Identifier: "alice@example.com",
	}))
	if err != nil {
		t.Fatalf("InitiateRecovery: %v", err)
	}
	if resp.Msg.GetStatus() != recoveryv1.InitiateRecoveryResponse_STATUS_CAPTCHA_REQUIRED {
		t.Fatalf("status = %v, want STATUS_CAPTCHA_REQUIRED", resp.Msg.GetStatus())
	}
}

func TestInitiateRecovery_NotFound(t *testing.T) {
	h, _, _ := newTestRecoveryHandler(true)

	resp, err := h.InitiateRecovery(context.Background(), connect.NewRequest(&recoveryv1.InitiateRecoveryRequest{
		Identifier: "nobody@example.com", PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("InitiateRecovery: %v", err)
	}
	if resp.Msg.GetStatus() != recoveryv1.InitiateRecoveryResponse_STATUS_NOT_FOUND {
		t.Fatalf("status = %v, want STATUS_NOT_FOUND", resp.Msg.GetStatus())
	}
}

// Аккаунт с email и телефоном должен получить три метода: EMAIL (с
// маской), SMS (с маской) и TELEGRAM (без masked_destination — см.
// doc-комментарий AvailableMethod) — но не MAX, для него сегодня нет даже
// способа решить, доступен ли он (см. recoveryDestination).
func TestInitiateRecovery_AvailableListsEmailSmsTelegramNotMax(t *testing.T) {
	h, repo, _ := newTestRecoveryHandler(true)
	repo.accounts["alice@example.com"] = repository.RecoveryAccount{
		UserID: "user-1", Email: strPtr("alice@example.com"), Phone: strPtr("+79990001234"),
	}

	resp, err := h.InitiateRecovery(context.Background(), connect.NewRequest(&recoveryv1.InitiateRecoveryRequest{
		Identifier: "alice@example.com", PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("InitiateRecovery: %v", err)
	}
	if resp.Msg.GetStatus() != recoveryv1.InitiateRecoveryResponse_STATUS_AVAILABLE {
		t.Fatalf("status = %v, want STATUS_AVAILABLE", resp.Msg.GetStatus())
	}
	if resp.Msg.GetRecoverySessionId() == "" {
		t.Fatal("expected a non-empty recovery_session_id")
	}

	methods := resp.Msg.GetAvailableMethods()
	if len(methods) != 3 {
		t.Fatalf("len(methods) = %d, want 3 (email, sms, telegram)", len(methods))
	}
	seen := map[recoveryv1.DeliveryChannel]string{}
	for _, m := range methods {
		seen[m.GetChannel()] = m.GetMaskedDestination()
	}
	if _, ok := seen[recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_MAX]; ok {
		t.Fatal("DELIVERY_CHANNEL_MAX must not be offered")
	}
	if seen[recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL] == "" {
		t.Fatal("expected a masked destination for EMAIL")
	}
	if seen[recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS] == "" {
		t.Fatal("expected a masked destination for SMS")
	}
	if dest, ok := seen[recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_TELEGRAM]; !ok || dest != "" {
		t.Fatalf("TELEGRAM masked_destination = %q, want present and empty", dest)
	}
}

func TestSendRecoveryCode_RejectsUnavailableChannel(t *testing.T) {
	h, repo, _ := newTestRecoveryHandler(true)
	repo.accounts["alice@example.com"] = repository.RecoveryAccount{UserID: "user-1", Email: strPtr("alice@example.com")}
	initResp, err := h.InitiateRecovery(context.Background(), connect.NewRequest(&recoveryv1.InitiateRecoveryRequest{
		Identifier: "alice@example.com", PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("InitiateRecovery: %v", err)
	}
	sessionID := initResp.Msg.GetRecoverySessionId()

	// Аккаунт без телефона — SMS должен быть недоступен.
	_, err = h.SendRecoveryCode(context.Background(), connect.NewRequest(&recoveryv1.SendRecoveryCodeRequest{
		RecoverySessionId: sessionID, Channel: recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS,
		PowChallenge: "c", PowNonce: "n",
	}))
	if code := connectCode(t, err); code != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want CodeInvalidArgument", code)
	}
	if key := errorKey(t, err); key != "identity.recovery_channel.unavailable" {
		t.Fatalf("key = %q, want %q", key, "identity.recovery_channel.unavailable")
	}
}

func TestSendRecoveryCode_SendsToEmailDestination(t *testing.T) {
	h, repo, codes := newTestRecoveryHandler(true)
	repo.accounts["alice@example.com"] = repository.RecoveryAccount{UserID: "user-1", Email: strPtr("alice@example.com")}
	initResp, err := h.InitiateRecovery(context.Background(), connect.NewRequest(&recoveryv1.InitiateRecoveryRequest{
		Identifier: "alice@example.com", PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("InitiateRecovery: %v", err)
	}
	sessionID := initResp.Msg.GetRecoverySessionId()

	resp, err := h.SendRecoveryCode(context.Background(), connect.NewRequest(&recoveryv1.SendRecoveryCodeRequest{
		RecoverySessionId: sessionID, Channel: recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
		PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("SendRecoveryCode: %v", err)
	}
	if resp.Msg.GetStatus() != recoveryv1.SendRecoveryCodeResponse_STATUS_SENT {
		t.Fatalf("status = %v, want STATUS_SENT", resp.Msg.GetStatus())
	}
	if codes.codeFor("alice@example.com") == "" {
		t.Fatal("expected a code to have been sent to alice@example.com")
	}
}

func TestConfirmRecoveryCode_MismatchThenSuccess(t *testing.T) {
	h, repo, codes := newTestRecoveryHandler(true)
	repo.accounts["alice@example.com"] = repository.RecoveryAccount{UserID: "user-1", Email: strPtr("alice@example.com")}
	initResp, err := h.InitiateRecovery(context.Background(), connect.NewRequest(&recoveryv1.InitiateRecoveryRequest{
		Identifier: "alice@example.com", PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("InitiateRecovery: %v", err)
	}
	sessionID := initResp.Msg.GetRecoverySessionId()
	if _, err := h.SendRecoveryCode(context.Background(), connect.NewRequest(&recoveryv1.SendRecoveryCodeRequest{
		RecoverySessionId: sessionID, Channel: recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
		PowChallenge: "c", PowNonce: "n",
	})); err != nil {
		t.Fatalf("SendRecoveryCode: %v", err)
	}
	code := codes.codeFor("alice@example.com")

	_, err = h.ConfirmRecoveryCode(context.Background(), connect.NewRequest(&recoveryv1.ConfirmRecoveryCodeRequest{
		RecoverySessionId: sessionID, Code: "000000",
	}))
	if code := connectCode(t, err); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated for a wrong code", code)
	}

	if _, err := h.ConfirmRecoveryCode(context.Background(), connect.NewRequest(&recoveryv1.ConfirmRecoveryCodeRequest{
		RecoverySessionId: sessionID, Code: code,
	})); err != nil {
		t.Fatalf("ConfirmRecoveryCode with the real code: %v", err)
	}
}

func TestResetPassword_RequiresConfirmedCode(t *testing.T) {
	h, repo, _ := newTestRecoveryHandler(true)
	repo.accounts["alice@example.com"] = repository.RecoveryAccount{UserID: "user-1", Email: strPtr("alice@example.com")}
	initResp, err := h.InitiateRecovery(context.Background(), connect.NewRequest(&recoveryv1.InitiateRecoveryRequest{
		Identifier: "alice@example.com", PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("InitiateRecovery: %v", err)
	}
	sessionID := initResp.Msg.GetRecoverySessionId()

	_, err = h.ResetPassword(context.Background(), connect.NewRequest(&recoveryv1.ResetPasswordRequest{
		RecoverySessionId: sessionID, Salt: []byte("salt"), Verifier: bytes.Repeat([]byte{1}, 128),
	}))
	if code := connectCode(t, err); code != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want CodeFailedPrecondition before the code is confirmed", code)
	}
	if key := errorKey(t, err); key != "identity.recovery_code.not_confirmed" {
		t.Fatalf("key = %q, want %q", key, "identity.recovery_code.not_confirmed")
	}
}

func TestResetPassword_SucceedsAfterConfirmationAndReturnsFrozenUntil(t *testing.T) {
	h, repo, codes := newTestRecoveryHandler(true)
	repo.accounts["alice@example.com"] = repository.RecoveryAccount{UserID: "user-1", Email: strPtr("alice@example.com")}
	initResp, err := h.InitiateRecovery(context.Background(), connect.NewRequest(&recoveryv1.InitiateRecoveryRequest{
		Identifier: "alice@example.com", PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("InitiateRecovery: %v", err)
	}
	sessionID := initResp.Msg.GetRecoverySessionId()
	if _, err := h.SendRecoveryCode(context.Background(), connect.NewRequest(&recoveryv1.SendRecoveryCodeRequest{
		RecoverySessionId: sessionID, Channel: recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
		PowChallenge: "c", PowNonce: "n",
	})); err != nil {
		t.Fatalf("SendRecoveryCode: %v", err)
	}
	code := codes.codeFor("alice@example.com")
	if _, err := h.ConfirmRecoveryCode(context.Background(), connect.NewRequest(&recoveryv1.ConfirmRecoveryCodeRequest{
		RecoverySessionId: sessionID, Code: code,
	})); err != nil {
		t.Fatalf("ConfirmRecoveryCode: %v", err)
	}

	before := time.Now()
	resp, err := h.ResetPassword(context.Background(), connect.NewRequest(&recoveryv1.ResetPasswordRequest{
		RecoverySessionId: sessionID, Salt: []byte("salt"), Verifier: bytes.Repeat([]byte{1}, 128),
	}))
	if err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	wantFrozenUntil := before.Add(testPasswordResetFreeze).Unix()
	if got := resp.Msg.GetFrozenUntil(); got < wantFrozenUntil || got > wantFrozenUntil+5 {
		t.Fatalf("frozen_until = %d, want close to %d", got, wantFrozenUntil)
	}

	// Сессия одноразовая — повторный вызов должен вернуть NotFound.
	_, err = h.ResetPassword(context.Background(), connect.NewRequest(&recoveryv1.ResetPasswordRequest{
		RecoverySessionId: sessionID, Salt: []byte("salt"), Verifier: bytes.Repeat([]byte{1}, 128),
	}))
	if code := connectCode(t, err); code != connect.CodeNotFound {
		t.Fatalf("code = %v, want CodeNotFound on replay", code)
	}
}
