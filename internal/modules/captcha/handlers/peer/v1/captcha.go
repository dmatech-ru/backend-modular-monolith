// Package v1 реализует внутренний контракт captcha-модуля
// (peer.captcha.v1.CaptchaService) поверх connect-go. Наружу через
// гейтвей не торчит — его зовут только другие модули по NATS напрямую
// (см. internal/pkg/natsrpc.Client). VerifyChallenge здесь, а не в webapi:
// иначе клиент мог бы просто заявить "решение верное" без проверки сервером.
package v1

import (
	"context"
	"net/http"
	"time"

	"connectrpc.com/connect"

	captchav1 "dmatech.ru/internal/pkg/contracts/peer/v1/captcha"
	"dmatech.ru/internal/pkg/contracts/peer/v1/captcha/captchav1connect"
)

// Service — доменная логика, нужная обработчику.
type Service interface {
	IssueChallenge(ctx context.Context, difficulty int32) (challenge string, echoedDifficulty int32, expiresAt time.Time, err error)
	VerifyChallenge(ctx context.Context, challenge, nonce string) (valid bool, err error)
}

// CaptchaHandler реализует captchav1connect.CaptchaServiceHandler.
type CaptchaHandler struct {
	captchav1connect.UnimplementedCaptchaServiceHandler

	svc Service
}

// NewHandler собирает HTTP-обработчик peer.captcha.v1.CaptchaService.
func NewHandler(svc Service, opts ...connect.HandlerOption) (string, http.Handler) {
	return captchav1connect.NewCaptchaServiceHandler(&CaptchaHandler{svc: svc}, opts...)
}

// IssueChallenge — та же операция, что у webapi-контракта, но для вызова
// server-side (если она когда-нибудь понадобится другому модулю напрямую,
// без браузера). difficulty здесь можно передать явно, в отличие от webapi.
func (h *CaptchaHandler) IssueChallenge(
	ctx context.Context,
	req *connect.Request[captchav1.IssueChallengeRequest],
) (*connect.Response[captchav1.IssueChallengeResponse], error) {
	challenge, difficulty, expiresAt, err := h.svc.IssueChallenge(ctx, req.Msg.GetDifficulty())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&captchav1.IssueChallengeResponse{
		Challenge:        challenge,
		EchoedDifficulty: difficulty,
		ExpiresAt:        expiresAt.Unix(),
	}), nil
}

// VerifyChallenge проверяет решение. Невалидный challenge (не найден,
// просрочен, уже использован) и неверный nonce неотличимы друг от друга —
// оба дают valid=false, а не разные ошибки: вызывающему модулю не нужно
// (и не должно) различать эти случаи.
func (h *CaptchaHandler) VerifyChallenge(
	ctx context.Context,
	req *connect.Request[captchav1.VerifyChallengeRequest],
) (*connect.Response[captchav1.VerifyChallengeResponse], error) {
	valid, err := h.svc.VerifyChallenge(ctx, req.Msg.GetChallenge(), req.Msg.GetNonce())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&captchav1.VerifyChallengeResponse{Valid: valid}), nil
}
