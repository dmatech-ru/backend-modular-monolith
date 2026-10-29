package v1

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"dmatech.ru/internal/modules/identity/repository"
	"dmatech.ru/internal/pkg/authctx"
	common "dmatech.ru/internal/pkg/contracts/webapi/v1/common"
	authv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/auth"
	"dmatech.ru/internal/pkg/srp"
)

// fakeRepository — Repository в памяти для тестов, без реального PostgreSQL.
type fakeRepository struct {
	mu         sync.Mutex
	nextID     int
	usersByID  map[string]fakeUser
	challenges map[string]repository.ChallengeRecord
}

type fakeUser struct {
	identifier     string
	salt, verifier []byte
	// status — по умолчанию пустая строка; seedUser выставляет
	// repository.UserStatusActive (как и настоящий CompleteRegistration,
	// см. registration.go), если явно не переопределить через seedUserWithStatus.
	status string
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{
		usersByID:  map[string]fakeUser{},
		challenges: map[string]repository.ChallengeRecord{},
	}
}

func (f *fakeRepository) FindAuth(_ context.Context, identifier string) (repository.AuthRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, u := range f.usersByID {
		if u.identifier == identifier {
			return repository.AuthRecord{UserID: id, Salt: u.salt, Verifier: u.verifier}, nil
		}
	}
	return repository.AuthRecord{}, repository.ErrNotFound
}

func (f *fakeRepository) CreateChallenge(_ context.Context, userID *string, verifier, serverSecret, serverPublic []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("challenge-%d", f.nextID)
	f.challenges[id] = repository.ChallengeRecord{UserID: userID, Verifier: verifier, ServerSecret: serverSecret, ServerPublic: serverPublic}
	return id, nil
}

func (f *fakeRepository) ConsumeChallenge(_ context.Context, challengeID string) (repository.ChallengeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.challenges[challengeID]
	if !ok {
		return repository.ChallengeRecord{}, repository.ErrNotFound
	}
	delete(f.challenges, challengeID)
	return rec, nil
}

func (f *fakeRepository) GetUserStatus(_ context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.usersByID[userID]
	if !ok {
		return "", repository.ErrNotFound
	}
	return u.status, nil
}

// seedUser заводит пользователя прямо в фейковом хранилище — для тестов
// SRP-входа (InitializeSession/CompleteAuthentication), которым нужен уже
// существующий аккаунт, но не интересует сама регистрация (см. registration_test.go).
// Статус — repository.UserStatusActive, как у настоящего CompleteRegistration
// (см. registration.go); для сценариев с другим статусом — seedUserWithStatus.
func (f *fakeRepository) seedUser(identifier, password string) {
	f.seedUserWithStatus(identifier, password, repository.UserStatusActive)
}

func (f *fakeRepository) seedUserWithStatus(identifier, password, status string) {
	salt, err := srp.GenerateSalt()
	if err != nil {
		panic(err) // тестовый хелпер, не часть продового пути
	}
	verifier := srp.ComputeVerifier(identifier, password, salt)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("user-%d", f.nextID)
	f.usersByID[id] = fakeUser{identifier: identifier, salt: salt, verifier: verifier, status: status}
}

// testAntiEnumKey — секрет srp.FakeCredentials для тестов (не для прода,
// см. комментарий у AntiEnumerationKey в identity/config.go).
var testAntiEnumKey = []byte("test-anti-enumeration-key-32-bytes!")

// fakeSessions — Sessions (SessionIssuer+SessionStore+SessionRevoker) в
// памяти для тестов: реального identity.sessions/JWT не нужно, только
// фиксированные значения, чтобы проверить, что хендлеры вызывают эти
// методы и выставляют куки на успехе.
type fakeSessions struct {
	mu             sync.Mutex
	nextID         int
	revoked        map[string]bool // userID -> RevokeAllSessions вызывался
	revokedRefresh []string        // refresh-токены, с которыми звали Revoke
	rotateErr      error
	revokeErr      error
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{revoked: map[string]bool{}}
}

func (f *fakeSessions) IssueSession(_ context.Context, userID, _ string) (access, refresh, permissions string, accessExpiresAt time.Time, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	return fmt.Sprintf("access-%s-%d", userID, f.nextID), fmt.Sprintf("refresh-%s-%d", userID, f.nextID), "", time.Now().Add(time.Hour), nil
}

func (f *fakeSessions) Rotate(_ context.Context, refreshToken, _ string) (access, refresh, permissions string, accessExpiresAt time.Time, err error) {
	if f.rotateErr != nil {
		return "", "", "", time.Time{}, f.rotateErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	return fmt.Sprintf("new-access-%d", f.nextID), fmt.Sprintf("new-refresh-%d", f.nextID), "", time.Now().Add(time.Hour), nil
}

func (f *fakeSessions) Revoke(_ context.Context, refreshToken string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokedRefresh = append(f.revokedRefresh, refreshToken)
	return nil
}

func (f *fakeSessions) RevokeAllSessions(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[userID] = true
	return nil
}

func newTestHandler() *AuthHandler {
	return NewAuthHandler(newFakeRepository(), testAntiEnumKey, newFakeSessions())
}

func connectCode(t *testing.T, err error) connect.Code {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected a *connect.Error, got %T: %v", err, err)
	}
	return cerr.Code()
}

// errorKey достаёт ключ ошибки (common.ErrorPayload.Key, либо, если есть
// привязанные к полю нарушения, common.FieldError.Key первого из них — см.
// keyedError/fieldError в auth.go) — общий для всех трёх сервисов формат,
// см. doc-комментарий ErrorPayload в webapi/v1/common/errors.proto.
func errorKey(t *testing.T, err error) string {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected a *connect.Error, got %T: %v", err, err)
	}
	details := cerr.Details()
	if len(details) != 1 {
		t.Fatalf("len(details) = %d, want 1", len(details))
	}
	msg, derr := details[0].Value()
	if derr != nil {
		t.Fatalf("decode detail: %v", derr)
	}
	payload, ok := msg.(*common.ErrorPayload)
	if !ok {
		t.Fatalf("detail type = %T, want *common.ErrorPayload", msg)
	}
	if fields := payload.GetFields(); len(fields) > 0 {
		return fields[0].GetKey()
	}
	return payload.GetKey()
}

// Анти-перечисление аккаунтов: несуществующий identifier не должен
// отличаться от существующего ни кодом ошибки (InitializeSession всегда
// успешен), ни стабильностью ответа, ни результатом второго шага.

func TestInitializeSession_UnknownIdentifierSucceeds(t *testing.T) {
	h := newTestHandler()

	resp, err := h.InitializeSession(context.Background(), connect.NewRequest(&authv1.InitializeSessionRequest{
		Identifier: "unknown@example.com",
	}))
	if err != nil {
		t.Fatalf("InitializeSession for an unknown identifier must not fail, got: %v", err)
	}
	if resp.Msg.GetSessionId() == "" || len(resp.Msg.GetSalt()) == 0 || len(resp.Msg.GetServerPublic()) == 0 {
		t.Fatalf("expected a well-formed response, got %+v", resp.Msg)
	}
}

func TestInitializeSession_UnknownIdentifierSaltIsStable(t *testing.T) {
	h := newTestHandler()
	ctx := context.Background()
	req := connect.NewRequest(&authv1.InitializeSessionRequest{Identifier: "unknown@example.com"})

	resp1, err := h.InitializeSession(ctx, req)
	if err != nil {
		t.Fatalf("InitializeSession: %v", err)
	}
	resp2, err := h.InitializeSession(ctx, req)
	if err != nil {
		t.Fatalf("InitializeSession: %v", err)
	}

	if !bytes.Equal(resp1.Msg.GetSalt(), resp2.Msg.GetSalt()) {
		t.Fatal("salt for the same unknown identifier must be stable across requests, or it would itself leak that the account does not exist")
	}
}

func TestInitializeSession_UnknownIdentifierLooksTheSameAcrossReplicas(t *testing.T) {
	// Две разные "реплики" (разные fakeRepository) с одним и тем же секретом
	// должны отвечать одинаково для одного и того же неизвестного identifier.
	h1 := NewAuthHandler(newFakeRepository(), testAntiEnumKey, newFakeSessions())
	h2 := NewAuthHandler(newFakeRepository(), testAntiEnumKey, newFakeSessions())
	req := connect.NewRequest(&authv1.InitializeSessionRequest{Identifier: "unknown@example.com"})

	resp1, err := h1.InitializeSession(context.Background(), req)
	if err != nil {
		t.Fatalf("InitializeSession (h1): %v", err)
	}
	resp2, err := h2.InitializeSession(context.Background(), req)
	if err != nil {
		t.Fatalf("InitializeSession (h2): %v", err)
	}
	if !bytes.Equal(resp1.Msg.GetSalt(), resp2.Msg.GetSalt()) {
		t.Fatal("salt for the same unknown identifier must be identical across replicas sharing the same antiEnumerationKey")
	}
}

func TestAuthFlow_UnknownIdentifierAlwaysFailsLikeWrongPassword(t *testing.T) {
	h := newTestHandler()

	_, err := loginAs(t, h, "unknown@example.com", "any password at all")
	if code := connectCode(t, err); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated (must look identical to a wrong password on a real account)", code)
	}
}

// loginAs эмулирует настоящего SRP-клиента (независимо от сервера) для
// identifier/password, уже зарегистрированных в h, и возвращает финальный
// ответ CompleteAuthentication.
func loginAs(t *testing.T, h *AuthHandler, identifier, password string) (*connect.Response[authv1.CompleteAuthenticationResponse], error) {
	t.Helper()
	ctx := context.Background()

	initResp, err := h.InitializeSession(ctx, connect.NewRequest(&authv1.InitializeSessionRequest{Identifier: identifier}))
	if err != nil {
		t.Fatalf("initialize session: %v", err)
	}

	clientSecret, clientPublic, err := srp.NewClientEphemeral()
	if err != nil {
		t.Fatalf("NewClientEphemeral: %v", err)
	}
	clientProof, sessionKey, err := srp.ComputeClientProof(identifier, password,
		initResp.Msg.GetSalt(), clientSecret, clientPublic, initResp.Msg.GetServerPublic())
	if err != nil {
		t.Fatalf("ComputeClientProof: %v", err)
	}

	resp, err := h.CompleteAuthentication(ctx, connect.NewRequest(&authv1.CompleteAuthenticationRequest{
		SessionId:    initResp.Msg.GetSessionId(),
		ClientPublic: clientPublic,
		ClientProof:  clientProof,
	}))
	if err == nil {
		if verr := srp.VerifyServerProof(resp.Msg.GetServerProof(), clientPublic, clientProof, sessionKey); verr != nil {
			t.Fatalf("server proof did not verify: %v", verr)
		}
	}
	return resp, err
}

func TestAuthFlow_RegisterThenLoginSucceeds(t *testing.T) {
	repo := newFakeRepository()
	h := NewAuthHandler(repo, testAntiEnumKey, newFakeSessions())
	const identifier = "alice@example.com"
	const password = "correct horse battery staple"

	repo.seedUser(identifier, password)

	resp, err := loginAs(t, h, identifier, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if len(resp.Msg.GetServerProof()) == 0 {
		t.Fatal("expected non-empty server_proof")
	}
}

// CompleteAuthentication — момент, когда у клиента появляется сессия (см.
// doc-комментарий метода): успешный вход должен выставить все три куки.
func TestAuthFlow_SuccessfulLoginSetsSessionCookies(t *testing.T) {
	repo := newFakeRepository()
	h := NewAuthHandler(repo, testAntiEnumKey, newFakeSessions())
	const identifier = "alice@example.com"
	const password = "correct horse battery staple"

	repo.seedUser(identifier, password)

	resp, err := loginAs(t, h, identifier, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	cookies := (&http.Response{Header: resp.Header()}).Cookies()
	names := map[string]bool{}
	for _, c := range cookies {
		names[c.Name] = true
	}
	for _, want := range []string{authctx.CookieAccessToken, authctx.CookieRefreshToken, authctx.CookieSessionInfo} {
		if !names[want] {
			t.Fatalf("missing %s cookie in response, got %v", want, names)
		}
	}
}

// Доказательство верно, но аккаунт не активен — вход должен быть отказан
// (см. doc-комментарий CompleteAuthentication) независимо от корректности
// пароля.
func TestAuthFlow_InactiveAccountIsRejectedEvenWithCorrectPassword(t *testing.T) {
	repo := newFakeRepository()
	h := NewAuthHandler(repo, testAntiEnumKey, newFakeSessions())
	const identifier = "alice@example.com"
	const password = "correct horse battery staple"

	repo.seedUserWithStatus(identifier, password, repository.UserStatusBlocked)

	_, err := loginAs(t, h, identifier, password)
	if code := connectCode(t, err); code != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want CodeFailedPrecondition", code)
	}
	if key := errorKey(t, err); key != "identity.account.not_active" {
		t.Fatalf("key = %q, want %q", key, "identity.account.not_active")
	}
}

func TestAuthFlow_WrongPasswordIsRejected(t *testing.T) {
	repo := newFakeRepository()
	h := NewAuthHandler(repo, testAntiEnumKey, newFakeSessions())
	const identifier = "alice@example.com"

	repo.seedUser(identifier, "correct horse battery staple")

	_, err := loginAs(t, h, identifier, "wrong password entirely")
	if code := connectCode(t, err); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated", code)
	}
}

func TestCompleteAuthentication_SessionIsSingleUse(t *testing.T) {
	repo := newFakeRepository()
	h := NewAuthHandler(repo, testAntiEnumKey, newFakeSessions())
	const identifier = "alice@example.com"
	const password = "correct horse battery staple"

	repo.seedUser(identifier, password)

	ctx := context.Background()
	initResp, err := h.InitializeSession(ctx, connect.NewRequest(&authv1.InitializeSessionRequest{Identifier: identifier}))
	if err != nil {
		t.Fatalf("initialize session: %v", err)
	}
	clientSecret, clientPublic, err := srp.NewClientEphemeral()
	if err != nil {
		t.Fatalf("NewClientEphemeral: %v", err)
	}
	clientProof, _, err := srp.ComputeClientProof(identifier, password,
		initResp.Msg.GetSalt(), clientSecret, clientPublic, initResp.Msg.GetServerPublic())
	if err != nil {
		t.Fatalf("ComputeClientProof: %v", err)
	}
	req := connect.NewRequest(&authv1.CompleteAuthenticationRequest{
		SessionId: initResp.Msg.GetSessionId(), ClientPublic: clientPublic, ClientProof: clientProof,
	})

	if _, err := h.CompleteAuthentication(ctx, req); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	_, err = h.CompleteAuthentication(ctx, req)
	if code := connectCode(t, err); code != connect.CodeNotFound {
		t.Fatalf("second complete code = %v, want CodeNotFound (session must be single-use)", code)
	}
	if key := errorKey(t, err); key != "identity.session.not_found" {
		t.Fatalf("key = %q, want %q", key, "identity.session.not_found")
	}
}
