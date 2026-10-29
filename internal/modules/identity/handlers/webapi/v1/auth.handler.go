package v1

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"

	"dmatech.ru/internal/pkg/natsrpc"
)

// Subscribe подписывает IdentityAuthService, IdentityRegistrationService и
// IdentityRecoveryService на входящие запросы через NATS: один общий mux,
// одна подписка на весь модуль (методы различает путь запроса — коллизий
// между сервисами нет), чтобы гейтвей мог достучаться до них по маршруту
// {env}.{serviceName}.{version}.
func Subscribe(
	nc *nats.Conn, log *slog.Logger, env, serviceName, version string,
	repo Repository, antiEnumKey []byte,
	regRepo RegistrationRepository, captcha CaptchaClient, phones PhoneSender, emails EmailSender,
	recoveryRepo RecoveryRepository, recoveryCodes RecoveryCodeSender, passwordResetFreeze time.Duration,
) (*natsrpc.RPCSubscription, error) {
	mux := http.NewServeMux()

	authPath, authHandler := NewHandler(repo, antiEnumKey)
	mux.Handle(authPath, authHandler)

	registrationPath, registrationHandler := NewRegistrationServiceHandler(regRepo, captcha, phones, emails)
	mux.Handle(registrationPath, registrationHandler)

	recoveryPath, recoveryHandler := NewRecoveryServiceHandler(recoveryRepo, captcha, recoveryCodes, passwordResetFreeze)
	mux.Handle(recoveryPath, recoveryHandler)

	return natsrpc.ServeHandler(nc, log, env, serviceName, version, mux)
}
