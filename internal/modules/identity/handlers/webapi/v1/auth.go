// Package v1 реализует webapi-контракт identity-модуля
// (webapi.identity.auth.v1.IdentityAuthService) поверх connect-go.
package v1

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"dmatech.ru/internal/modules/identity/repository"
	common "dmatech.ru/internal/pkg/contracts/webapi/v1/common"
	identityv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity"
	"dmatech.ru/internal/pkg/contracts/webapi/v1/identity/authv1connect"
	"dmatech.ru/internal/pkg/srp"
)

// Repository — доступ к данным, нужный обработчику. Реализуется
// *repository.Repository поверх PostgreSQL; интерфейс объявлен на стороне
// потребителя (здесь), чтобы тесты могли подставить фейк без реальной БД.
type Repository interface {
	FindAuth(ctx context.Context, identifier string) (repository.AuthRecord, error)
	CreateChallenge(ctx context.Context, userID *string, verifier, serverSecret, serverPublic []byte) (string, error)
	ConsumeChallenge(ctx context.Context, challengeID string) (repository.ChallengeRecord, error)
	CreateUser(ctx context.Context, p repository.CreateUserParams) (string, error)
}

// AuthHandler реализует authv1connect.IdentityAuthServiceHandler.
type AuthHandler struct {
	authv1connect.UnimplementedIdentityAuthServiceHandler

	repo Repository
	// antiEnumKey — секрет для srp.FakeCredentials (защита InitializeSession
	// от перечисления аккаунтов), см. комментарий там.
	antiEnumKey []byte
}

// NewAuthHandler создаёт обработчик IdentityAuthService поверх repo.
// antiEnumKey — секрет для srp.FakeCredentials, общий для всех реплик.
func NewAuthHandler(repo Repository, antiEnumKey []byte) *AuthHandler {
	return &AuthHandler{repo: repo, antiEnumKey: antiEnumKey}
}

// NewHandler собирает HTTP-обработчик IdentityAuthService для монтирования
// в веб-сервер: возвращает путь монтирования и сам connect-handler.
func NewHandler(repo Repository, antiEnumKey []byte, opts ...connect.HandlerOption) (string, http.Handler) {
	return authv1connect.NewIdentityAuthServiceHandler(NewAuthHandler(repo, antiEnumKey), opts...)
}

// InitializeSession — первый шаг SRP-входа: по identifier находит учётную
// запись и генерирует эфемерные серверные значения SRP-обмена.
//
// Для несуществующего identifier ответ не отличается по форме: вместо
// CodeNotFound используются детерминированные "фейковые" salt/verifier
// (srp.FakeCredentials) — иначе сам код ответа раскрывал бы, зарегистрирован
// ли identifier (account enumeration). Пройти второй шаг с такими
// значениями невозможно ни при каком пароле — CompleteAuthentication
// вернёт то же CodeUnauthenticated, что и при неверном пароле на настоящем
// аккаунте.
func (h *AuthHandler) InitializeSession(
	ctx context.Context,
	req *connect.Request[identityv1.InitializeSessionRequest],
) (*connect.Response[identityv1.InitializeSessionResponse], error) {
	identifier := strings.TrimSpace(req.Msg.GetIdentifier())
	if identifier == "" {
		return nil, fieldError(connect.CodeInvalidArgument, "identifier", "identifier must not be empty")
	}

	var userID *string
	var salt, verifier []byte

	auth, err := h.repo.FindAuth(ctx, identifier)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		salt, verifier = srp.FakeCredentials(h.antiEnumKey, identifier)
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	default:
		userID, salt, verifier = &auth.UserID, auth.Salt, auth.Verifier
	}

	serverSecret, serverPublic, err := srp.NewServerChallenge(verifier)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	sessionID, err := h.repo.CreateChallenge(ctx, userID, verifier, serverSecret, serverPublic)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&identityv1.InitializeSessionResponse{
		SessionId:    sessionID,
		Salt:         salt,
		ServerPublic: serverPublic,
	}), nil
}

// CompleteAuthentication — второй шаг SRP-входа: проверяет доказательство
// клиента и, если оно верно, отвечает собственным доказательством.
func (h *AuthHandler) CompleteAuthentication(
	ctx context.Context,
	req *connect.Request[identityv1.CompleteAuthenticationRequest],
) (*connect.Response[identityv1.CompleteAuthenticationResponse], error) {
	sessionID := strings.TrimSpace(req.Msg.GetSessionId())
	if sessionID == "" {
		return nil, fieldError(connect.CodeInvalidArgument, "session_id", "session_id must not be empty")
	}
	clientPublic := req.Msg.GetClientPublic()
	clientProof := req.Msg.GetClientProof()
	if len(clientPublic) == 0 || len(clientProof) == 0 {
		return nil, fieldError(connect.CodeInvalidArgument, "client_public", "client_public and client_proof are required")
	}

	challenge, err := h.repo.ConsumeChallenge(ctx, sessionID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("session not found or expired"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	serverProof, err := srp.VerifyClientProof(challenge.Verifier, challenge.ServerSecret, challenge.ServerPublic, clientPublic, clientProof)
	switch {
	case errors.Is(err, srp.ErrProofMismatch), errors.Is(err, srp.ErrInvalidPublicValue), errors.Is(err, srp.ErrValueTooLarge):
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&identityv1.CompleteAuthenticationResponse{ServerProof: serverProof}), nil
}

// Register создаёт учётную запись. Пароль на сервер не передаётся: salt и
// verifier клиент вычисляет локально (srp.GenerateSalt + srp.ComputeVerifier)
// и присылает уже готовыми — сервер их только проверяет по форме и хранит.
func (h *AuthHandler) Register(
	ctx context.Context,
	req *connect.Request[identityv1.RegisterRequest],
) (*connect.Response[identityv1.RegisterResponse], error) {
	identifier := strings.TrimSpace(req.Msg.GetIdentifier())
	if identifier == "" {
		return nil, fieldError(connect.CodeInvalidArgument, "identifier", "identifier must not be empty")
	}
	salt := req.Msg.GetSalt()
	verifier := req.Msg.GetVerifier()
	if len(salt) == 0 {
		return nil, fieldError(connect.CodeInvalidArgument, "salt", "salt must not be empty")
	}
	if !srp.ValidVerifier(verifier) {
		return nil, fieldError(connect.CodeInvalidArgument, "verifier", "verifier is missing or malformed")
	}

	params := repository.CreateUserParams{
		Salt:       salt,
		Verifier:   verifier,
		FirstName:  strings.TrimSpace(req.Msg.GetFirstName()),
		LastName:   strings.TrimSpace(req.Msg.GetLastName()),
		MiddleName: strings.TrimSpace(req.Msg.GetMiddleName()),
	}
	if strings.Contains(identifier, "@") {
		params.Email = identifier
	} else {
		params.Phone = identifier
	}

	userID, err := h.repo.CreateUser(ctx, params)
	if errors.Is(err, repository.ErrAlreadyExists) {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("account already exists"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&identityv1.RegisterResponse{UserId: userID}), nil
}

// fieldError формирует connect-ошибку с деталью в виде common.ErrorPayload,
// чтобы клиент мог получить структурированную причину отказа по конкретному полю.
func fieldError(code connect.Code, field, message string) error {
	cerr := connect.NewError(code, errors.New(message))

	payload := &common.ErrorPayload{
		Code:    int32(code),
		Message: message,
		Fields: []*common.FieldError{
			{Name: field, Message: message},
		},
	}
	if detail, derr := connect.NewErrorDetail(payload); derr == nil {
		cerr.AddDetail(detail)
	}
	return cerr
}
