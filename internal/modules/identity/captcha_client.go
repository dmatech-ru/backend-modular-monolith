package identity

import (
	"context"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"

	authhandler "dmatech.ru/internal/modules/identity/handlers/webapi/v1"
	captchapeerv1 "dmatech.ru/internal/pkg/contracts/peer/v1/captcha"
	"dmatech.ru/internal/pkg/contracts/peer/v1/captcha/captchav1connect"
	"dmatech.ru/internal/pkg/natsrpc"
)

// newCaptchaClient строит authhandler.CaptchaClient поверх сгенерированного
// connect-клиента peer.captcha.v1.CaptchaService (не webapi — наружу через
// гейтвей этот вызов не идёт) и natsrpc.Client, который переносит обычный
// HTTP-вызов connect-клиента через NATS request-reply. identity знает про
// captcha только эти сгенерированные типы и имя/версию сервиса из своего
// конфига — никакого прямого Go-импорта internal/modules/captcha.
func newCaptchaClient(nc *nats.Conn, env, serviceName, version string) authhandler.CaptchaClient {
	httpClient := natsrpc.NewClient(nc, env, serviceName, version, captchaCallTimeout)
	client := captchav1connect.NewCaptchaServiceClient(httpClient, "http://"+serviceName)
	return &captchaClientAdapter{client: client}
}

type captchaClientAdapter struct {
	client captchav1connect.CaptchaServiceClient
}

func (a *captchaClientAdapter) VerifyChallenge(ctx context.Context, challenge, nonce string) (bool, error) {
	resp, err := a.client.VerifyChallenge(ctx, connect.NewRequest(&captchapeerv1.VerifyChallengeRequest{
		Challenge: challenge,
		Nonce:     nonce,
	}))
	if err != nil {
		return false, err
	}
	return resp.Msg.GetValid(), nil
}
