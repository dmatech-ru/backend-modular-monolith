// Package v1 реализует webapi-контракт identity-модуля
// (webapi.identity.auth.v1.IdentityAuthService) поверх connect-go.
package v1

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	common "dmatech.ru/internal/pkg/contracts/webapi/v1/common"
	identityv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity"
	"dmatech.ru/internal/pkg/contracts/webapi/v1/identity/authv1connect"
)

// AccountLookup проверяет, существует ли учётная запись с указанным
// идентификатором (username, email или номер телефона).
type AccountLookup interface {
	Exists(ctx context.Context, identifier string) (bool, error)
}

// AuthHandler реализует authv1connect.IdentityAuthServiceHandler.
type AuthHandler struct {
	authv1connect.UnimplementedIdentityAuthServiceHandler

	lookup AccountLookup
}

// NewAuthHandler создаёт обработчик IdentityAuthService, использующий lookup
// для проверки существования учётной записи.
func NewAuthHandler(lookup AccountLookup) *AuthHandler {
	return &AuthHandler{lookup: lookup}
}

// NewHandler собирает HTTP-обработчик IdentityAuthService для монтирования
// в веб-сервер: возвращает путь монтирования и сам connect-handler.
func NewHandler(lookup AccountLookup, opts ...connect.HandlerOption) (string, http.Handler) {
	return authv1connect.NewIdentityAuthServiceHandler(NewAuthHandler(lookup), opts...)
}

// InitializeSession проверяет идентификатор (username, email или телефон)
// и сообщает, существует ли учётная запись с таким идентификатором.
func (h *AuthHandler) InitializeSession(
	ctx context.Context,
	req *connect.Request[identityv1.InitializeSessionRequest],
) (*connect.Response[identityv1.InitializeSessionResponse], error) {
	identifier := strings.TrimSpace(req.Msg.GetIdentifier())
	if identifier == "" {
		return nil, fieldError(connect.CodeInvalidArgument, "identifier", "identifier must not be empty")
	}

	exists, err := h.lookup.Exists(ctx, identifier)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&identityv1.InitializeSessionResponse{Exists: exists}), nil
}

// fieldError формирует connect-ошибку с деталями в формате InitializeSessionError,
// чтобы клиент мог получить структурированную причину отказа по конкретному полю.
func fieldError(code connect.Code, field, message string) error {
	cerr := connect.NewError(code, errors.New(message))

	payload := &identityv1.InitializeSessionError{
		Error: &common.ErrorPayload{
			Code:    int32(code),
			Message: message,
			Fields: []*common.FieldError{
				{Name: field, Message: message},
			},
		},
	}
	if detail, derr := connect.NewErrorDetail(payload); derr == nil {
		cerr.AddDetail(detail)
	}
	return cerr
}
