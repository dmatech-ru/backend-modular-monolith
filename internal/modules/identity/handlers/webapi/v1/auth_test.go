package v1

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"dmatech.ru/internal/modules/identity/repository"
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

// seedUser заводит пользователя прямо в фейковом хранилище — для тестов
// SRP-входа (InitializeSession/CompleteAuthentication), которым нужен уже
// существующий аккаунт, но не интересует сама регистрация (см. registration_test.go).
func (f *fakeRepository) seedUser(identifier, password string) {
	salt, err := srp.GenerateSalt()
	if err != nil {
		panic(err) // тестовый хелпер, не часть продового пути
	}
	verifier := srp.ComputeVerifier(identifier, password, salt)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("user-%d", f.nextID)
	f.usersByID[id] = fakeUser{identifier: identifier, salt: salt, verifier: verifier}
}

// testAntiEnumKey — секрет srp.FakeCredentials для тестов (не для прода,
// см. комментарий у AntiEnumerationKey в identity/config.go).
var testAntiEnumKey = []byte("test-anti-enumeration-key-32-bytes!")

func newTestHandler() *AuthHandler {
	return NewAuthHandler(newFakeRepository(), testAntiEnumKey)
}

func connectCode(t *testing.T, err error) connect.Code {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected a *connect.Error, got %T: %v", err, err)
	}
	return cerr.Code()
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
	h1 := NewAuthHandler(newFakeRepository(), testAntiEnumKey)
	h2 := NewAuthHandler(newFakeRepository(), testAntiEnumKey)
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

func TestInitializeSession_EmptyIdentifier(t *testing.T) {
	h := newTestHandler()

	_, err := h.InitializeSession(context.Background(), connect.NewRequest(&authv1.InitializeSessionRequest{
		Identifier: "   ",
	}))
	if code := connectCode(t, err); code != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want CodeInvalidArgument", code)
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
	h := NewAuthHandler(repo, testAntiEnumKey)
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

func TestAuthFlow_WrongPasswordIsRejected(t *testing.T) {
	repo := newFakeRepository()
	h := NewAuthHandler(repo, testAntiEnumKey)
	const identifier = "alice@example.com"

	repo.seedUser(identifier, "correct horse battery staple")

	_, err := loginAs(t, h, identifier, "wrong password entirely")
	if code := connectCode(t, err); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated", code)
	}
}

func TestCompleteAuthentication_SessionIsSingleUse(t *testing.T) {
	repo := newFakeRepository()
	h := NewAuthHandler(repo, testAntiEnumKey)
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
}

func TestCompleteAuthentication_MissingFields(t *testing.T) {
	h := newTestHandler()

	_, err := h.CompleteAuthentication(context.Background(), connect.NewRequest(&authv1.CompleteAuthenticationRequest{
		SessionId: "   ",
	}))
	if code := connectCode(t, err); code != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want CodeInvalidArgument", code)
	}
}
