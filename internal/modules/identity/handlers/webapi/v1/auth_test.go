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
	identityv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity"
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

func (f *fakeRepository) CreateUser(_ context.Context, p repository.CreateUserParams) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	identifier := p.Email
	if identifier == "" {
		identifier = p.Phone
	}
	for _, u := range f.usersByID {
		if u.identifier == identifier {
			return "", repository.ErrAlreadyExists
		}
	}
	f.nextID++
	id := fmt.Sprintf("user-%d", f.nextID)
	f.usersByID[id] = fakeUser{identifier: identifier, salt: p.Salt, verifier: p.Verifier}
	return id, nil
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

// registerClientSide эмулирует клиента при регистрации: пароль никогда не
// отправляется обработчику, только вычисленные из него salt/verifier.
func registerClientSide(t *testing.T, h *AuthHandler, identifier, password string, firstName string) *connect.Response[identityv1.RegisterResponse] {
	t.Helper()
	salt, err := srp.GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	verifier := srp.ComputeVerifier(identifier, password, salt)

	resp, err := h.Register(context.Background(), connect.NewRequest(&identityv1.RegisterRequest{
		Identifier: identifier,
		Salt:       salt,
		Verifier:   verifier,
		FirstName:  firstName,
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return resp
}

func TestRegister_Success(t *testing.T) {
	h := newTestHandler()

	resp := registerClientSide(t, h, "alice@example.com", "correct horse battery staple", "Alice")
	if resp.Msg.GetUserId() == "" {
		t.Fatal("expected non-empty user_id")
	}
}

func TestRegister_DuplicateIdentifier(t *testing.T) {
	h := newTestHandler()
	registerClientSide(t, h, "alice@example.com", "correct horse battery staple", "")

	salt, err := srp.GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	verifier := srp.ComputeVerifier("alice@example.com", "a different password", salt)
	_, err = h.Register(context.Background(), connect.NewRequest(&identityv1.RegisterRequest{
		Identifier: "alice@example.com", Salt: salt, Verifier: verifier,
	}))
	if code := connectCode(t, err); code != connect.CodeAlreadyExists {
		t.Fatalf("code = %v, want CodeAlreadyExists", code)
	}
}

func TestRegister_MissingSaltOrVerifierRejected(t *testing.T) {
	h := newTestHandler()

	_, err := h.Register(context.Background(), connect.NewRequest(&identityv1.RegisterRequest{
		Identifier: "alice@example.com", Verifier: []byte("v"), // без salt
	}))
	if code := connectCode(t, err); code != connect.CodeInvalidArgument {
		t.Fatalf("missing salt: code = %v, want CodeInvalidArgument", code)
	}

	_, err = h.Register(context.Background(), connect.NewRequest(&identityv1.RegisterRequest{
		Identifier: "alice@example.com", Salt: []byte("s"), // без verifier
	}))
	if code := connectCode(t, err); code != connect.CodeInvalidArgument {
		t.Fatalf("missing verifier: code = %v, want CodeInvalidArgument", code)
	}
}

// Анти-перечисление аккаунтов: несуществующий identifier не должен
// отличаться от существующего ни кодом ошибки (InitializeSession всегда
// успешен), ни стабильностью ответа, ни результатом второго шага.

func TestInitializeSession_UnknownIdentifierSucceeds(t *testing.T) {
	h := newTestHandler()

	resp, err := h.InitializeSession(context.Background(), connect.NewRequest(&identityv1.InitializeSessionRequest{
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
	req := connect.NewRequest(&identityv1.InitializeSessionRequest{Identifier: "unknown@example.com"})

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
	req := connect.NewRequest(&identityv1.InitializeSessionRequest{Identifier: "unknown@example.com"})

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

	_, err := h.InitializeSession(context.Background(), connect.NewRequest(&identityv1.InitializeSessionRequest{
		Identifier: "   ",
	}))
	if code := connectCode(t, err); code != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want CodeInvalidArgument", code)
	}
}

// loginAs эмулирует настоящего SRP-клиента (независимо от сервера) для
// identifier/password, уже зарегистрированных в h, и возвращает финальный
// ответ CompleteAuthentication.
func loginAs(t *testing.T, h *AuthHandler, identifier, password string) (*connect.Response[identityv1.CompleteAuthenticationResponse], error) {
	t.Helper()
	ctx := context.Background()

	initResp, err := h.InitializeSession(ctx, connect.NewRequest(&identityv1.InitializeSessionRequest{Identifier: identifier}))
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

	resp, err := h.CompleteAuthentication(ctx, connect.NewRequest(&identityv1.CompleteAuthenticationRequest{
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
	h := newTestHandler()
	const identifier = "alice@example.com"
	const password = "correct horse battery staple"

	registerClientSide(t, h, identifier, password, "Alice")

	resp, err := loginAs(t, h, identifier, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if len(resp.Msg.GetServerProof()) == 0 {
		t.Fatal("expected non-empty server_proof")
	}
}

func TestAuthFlow_WrongPasswordIsRejected(t *testing.T) {
	h := newTestHandler()
	const identifier = "alice@example.com"

	registerClientSide(t, h, identifier, "correct horse battery staple", "")

	_, err := loginAs(t, h, identifier, "wrong password entirely")
	if code := connectCode(t, err); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated", code)
	}
}

func TestCompleteAuthentication_SessionIsSingleUse(t *testing.T) {
	h := newTestHandler()
	const identifier = "alice@example.com"
	const password = "correct horse battery staple"

	registerClientSide(t, h, identifier, password, "")

	ctx := context.Background()
	initResp, err := h.InitializeSession(ctx, connect.NewRequest(&identityv1.InitializeSessionRequest{Identifier: identifier}))
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
	req := connect.NewRequest(&identityv1.CompleteAuthenticationRequest{
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

	_, err := h.CompleteAuthentication(context.Background(), connect.NewRequest(&identityv1.CompleteAuthenticationRequest{
		SessionId: "   ",
	}))
	if code := connectCode(t, err); code != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want CodeInvalidArgument", code)
	}
}
