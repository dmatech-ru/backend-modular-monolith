// Package v1 реализует публичный webapi-контракт captcha-модуля
// (webapi.captcha.v1.CaptchaService) поверх connect-go. Единственная
// операция, которую видит браузер через гейтвей, — выдача challenge'я;
// проверка решения — в peer-контракте (см. handlers/peer/v1).
package v1

import (
	"context"
	"net/http"
	"time"

	"connectrpc.com/connect"

	captchav1 "dmatech.ru/internal/pkg/contracts/webapi/v1/captcha"
	"dmatech.ru/internal/pkg/contracts/webapi/v1/captcha/captchav1connect"
)

// Service — доменная логика, нужная обработчику. Реализуется
// *captcha.Service; интерфейс объявлен на стороне потребителя (здесь), как
// и везде в проекте, чтобы тесты могли подставить фейк.
type Service interface {
	IssueChallenge(ctx context.Context, difficulty int32) (challenge string, echoedDifficulty int32, expiresAt time.Time, err error)
}

// CaptchaHandler реализует captchav1connect.CaptchaServiceHandler.
type CaptchaHandler struct {
	captchav1connect.UnimplementedCaptchaServiceHandler

	svc Service
}

// NewHandler собирает HTTP-обработчик webapi.captcha.v1.CaptchaService для
// монтирования в веб-сервер: возвращает путь монтирования и сам connect-handler.
func NewHandler(svc Service, opts ...connect.HandlerOption) (string, http.Handler) {
	return captchav1connect.NewCaptchaServiceHandler(&CaptchaHandler{svc: svc}, opts...)
}

// IssueChallenge отдаёт новый challenge. Сложность клиент не выбирает — её
// определяет сервис (см. Service.IssueChallenge с difficulty=0).
func (h *CaptchaHandler) IssueChallenge(
	ctx context.Context,
	_ *connect.Request[captchav1.IssueChallengeRequest],
) (*connect.Response[captchav1.IssueChallengeResponse], error) {
	challenge, difficulty, expiresAt, err := h.svc.IssueChallenge(ctx, 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&captchav1.IssueChallengeResponse{
		Challenge:  challenge,
		Difficulty: difficulty,
		ExpiresAt:  expiresAt.Unix(),
	}), nil
}
