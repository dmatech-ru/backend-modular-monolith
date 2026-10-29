// Package v1 реализует webapi-контракт identity-модуля
// (webapi.identity.auth.v1.IdentityAuthService) поверх connect-go.
package v1

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	"dmatech.ru/internal/modules/identity/repository"
	common "dmatech.ru/internal/pkg/contracts/webapi/v1/common"
	authv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/auth"
	"dmatech.ru/internal/pkg/contracts/webapi/v1/identity/auth/authv1connect"
	"dmatech.ru/internal/pkg/srp"
)

// Repository — доступ к данным, нужный обработчику. Реализуется
// *repository.Repository поверх PostgreSQL; интерфейс объявлен на стороне
// потребителя (здесь), чтобы тесты могли подставить фейк без реальной БД.
type Repository interface {
	FindAuth(ctx context.Context, identifier string) (repository.AuthRecord, error)
	CreateChallenge(ctx context.Context, userID *string, verifier, serverSecret, serverPublic []byte) (string, error)
	ConsumeChallenge(ctx context.Context, challengeID string) (repository.ChallengeRecord, error)
	GetUserStatus(ctx context.Context, userID string) (string, error)
}

// SessionIssuer выпускает access/refresh для только что вошедшего/
// зарегистрированного пользователя (см. CompleteAuthentication здесь и
// CompleteRegistration в registration.go). Реализация — тонкая обёртка
// над identity.sessions + internal/pkg/authctx (см. identity/session.go).
type SessionIssuer interface {
	IssueSession(ctx context.Context, userID, userAgent string) (access, refresh, permissions string, accessExpiresAt time.Time, err error)
}

// AuthHandler реализует authv1connect.IdentityAuthServiceHandler.
type AuthHandler struct {
	authv1connect.UnimplementedIdentityAuthServiceHandler

	repo Repository
	// antiEnumKey — секрет для srp.FakeCredentials (защита InitializeSession
	// от перечисления аккаунтов), см. комментарий там.
	antiEnumKey []byte
	sessions    SessionIssuer
}

// NewAuthHandler создаёт обработчик IdentityAuthService поверх repo.
// antiEnumKey — секрет для srp.FakeCredentials, общий для всех реплик.
func NewAuthHandler(repo Repository, antiEnumKey []byte, sessions SessionIssuer) *AuthHandler {
	return &AuthHandler{repo: repo, antiEnumKey: antiEnumKey, sessions: sessions}
}

// NewHandler собирает HTTP-обработчик IdentityAuthService для монтирования
// в веб-сервер: возвращает путь монтирования и сам connect-handler.
func NewHandler(repo Repository, antiEnumKey []byte, sessions SessionIssuer, opts ...connect.HandlerOption) (string, http.Handler) {
	return authv1connect.NewIdentityAuthServiceHandler(NewAuthHandler(repo, antiEnumKey, sessions), opts...)
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
	req *connect.Request[authv1.InitializeSessionRequest],
) (*connect.Response[authv1.InitializeSessionResponse], error) {
	// identifier непустой — проверено до вызова этого метода (см.
	// buf.validate.field.cel на этом поле в auth.proto и validation.Interceptor
	// в auth.handler.go).
	identifier := strings.TrimSpace(req.Msg.GetIdentifier())

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

	return connect.NewResponse(&authv1.InitializeSessionResponse{
		SessionId:    sessionID,
		Salt:         salt,
		ServerPublic: serverPublic,
	}), nil
}

// CompleteAuthentication — второй шаг SRP-входа: проверяет доказательство
// клиента и, если оно верно, отвечает собственным доказательством, а также
// выпускает access/refresh/session_info куки (см. internal/pkg/authctx и
// identity/session.go) — это и есть момент, когда у клиента появляется
// сессия.
//
// Доказательство верно — не единственное условие: сессия выпускается
// только пользователю в статусе repository.UserStatusActive (см.
// doc-комментарий GetUserStatus и CompleteRegistration). Проверка — здесь,
// после проверки доказательства, не в InitializeSession: до этого момента
// личность вызывающего не подтверждена, а раскрывать статус аккаунта
// (например, "заблокирован") тому, кто ещё не доказал владение паролем,
// было бы утечкой данных сродни перечислению аккаунтов, от которой защищает
// InitializeSession (см. doc-комментарий там).
func (h *AuthHandler) CompleteAuthentication(
	ctx context.Context,
	req *connect.Request[authv1.CompleteAuthenticationRequest],
) (*connect.Response[authv1.CompleteAuthenticationResponse], error) {
	// session_id/client_public/client_proof непусты — проверено до вызова
	// этого метода (см. buf.validate.field.cel на этих полях в auth.proto).
	sessionID := strings.TrimSpace(req.Msg.GetSessionId())
	clientPublic := req.Msg.GetClientPublic()
	clientProof := req.Msg.GetClientProof()

	challenge, err := h.repo.ConsumeChallenge(ctx, sessionID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, keyedError(connect.CodeNotFound, "identity.session.not_found", "session not found or expired")
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	serverProof, err := srp.VerifyClientProof(challenge.Verifier, challenge.ServerSecret, challenge.ServerPublic, clientPublic, clientProof)
	switch {
	case errors.Is(err, srp.ErrProofMismatch), errors.Is(err, srp.ErrInvalidPublicValue), errors.Is(err, srp.ErrValueTooLarge):
		return nil, keyedError(connect.CodeUnauthenticated, "identity.credentials.invalid", "invalid credentials")
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// challenge.UserID — nil только у фейкового challenge'а несуществующего
	// identifier (см. srp.FakeCredentials в InitializeSession); проверка
	// доказательства с таким challenge'ом не проходит ни при каком пароле,
	// так что сюда можно дойти только с настоящим аккаунтом.
	status, err := h.repo.GetUserStatus(ctx, *challenge.UserID)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		// Аккаунт исчез между InitializeSession и CompleteAuthentication
		// (например, конкурентное жёсткое удаление) — для вызывающего кода
		// это то же самое, что "не активен", отдельная ветка не нужна.
		return nil, keyedError(connect.CodeFailedPrecondition, "identity.account.not_active", "account is not active")
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if status != repository.UserStatusActive {
		return nil, keyedError(connect.CodeFailedPrecondition, "identity.account.not_active", "account is not active")
	}

	access, refresh, permissions, accessExpiresAt, err := h.sessions.IssueSession(ctx, *challenge.UserID, req.Header().Get("User-Agent"))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := connect.NewResponse(&authv1.CompleteAuthenticationResponse{ServerProof: serverProof})
	setSessionCookies(resp.Header(), access, refresh, permissions, accessExpiresAt)
	return resp, nil
}

// fieldError формирует connect-ошибку с деталью в виде common.ErrorPayload,
// привязанной к конкретному полю. key — стабильный, не переведённый
// идентификатор ошибки (см. doc-комментарий ErrorPayload в
// webapi/v1/common/errors.proto и internal/pkg/validation, откуда идёт тот
// же формат для статической валидации формы запроса): здесь — для
// бизнес-правил, которые buf.validate не выражает (например, "канал
// недоступен для этой сессии"), а не для формы запроса. message —
// фиксированный, нелокализуемый текст для логов/отладки, клиент его не
// показывает и переводит key сам.
func fieldError(code connect.Code, field, key, message string) error {
	cerr := connect.NewError(code, errors.New(message))

	payload := &common.ErrorPayload{
		Code: int32(code),
		Fields: []*common.FieldError{
			{Name: field, Key: key, Message: message},
		},
	}
	if detail, derr := connect.NewErrorDetail(payload); derr == nil {
		cerr.AddDetail(detail)
	}
	return cerr
}

// keyedError — то же самое, что fieldError, но для ошибок, не привязанных
// к конкретному полю (не форма запроса, а состояние системы: "сессия не
// найдена", "слишком много попыток" и т.п.) — key идёт прямо в
// ErrorPayload, без FieldError.
func keyedError(code connect.Code, key, message string) error {
	cerr := connect.NewError(code, errors.New(message))

	payload := &common.ErrorPayload{
		Code:    int32(code),
		Message: message,
		Key:     key,
	}
	if detail, derr := connect.NewErrorDetail(payload); derr == nil {
		cerr.AddDetail(detail)
	}
	return cerr
}
