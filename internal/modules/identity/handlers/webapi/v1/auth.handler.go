package v1

import (
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"

	"dmatech.ru/internal/pkg/locale"
	"dmatech.ru/internal/pkg/natsrpc"
	"dmatech.ru/internal/pkg/validation"
)

// Subscribe подписывает IdentityAuthService, IdentityRegistrationService и
// IdentityRecoveryService на входящие запросы через NATS: один общий mux,
// одна подписка на весь модуль (методы различает путь запроса — коллизий
// между сервисами нет), чтобы гейтвей мог достучаться до них по маршруту
// {env}.{serviceName}.{version}.
//
// Все три сервиса получают два общих интерцептора: validation.Interceptor
// (статическая проверка формы запроса по buf.validate.* правилам в .proto —
// см. doc-комментарий пакета validation) и locale.ServerInterceptor
// (кладёт локаль запроса в context.Context для дальнейших вызовов других
// модулей, см. doc-комментарий пакета locale) — оба применяются ДО тела
// метода хендлера.
func Subscribe(
	nc *nats.Conn, log *slog.Logger, env, serviceName, version string,
	validator validation.Validator,
	repo Repository, antiEnumKey []byte,
	regRepo RegistrationRepository, captcha CaptchaClient, phones PhoneSender, emails EmailSender,
	recoveryRepo RecoveryRepository, recoveryCodes RecoveryCodeSender, passwordResetFreeze time.Duration,
) (*natsrpc.RPCSubscription, error) {
	opts := connect.WithInterceptors(validation.Interceptor(validator), locale.ServerInterceptor())

	mux := http.NewServeMux()

	authPath, authHandler := NewHandler(repo, antiEnumKey, opts)
	mux.Handle(authPath, authHandler)

	registrationPath, registrationHandler := NewRegistrationServiceHandler(regRepo, captcha, phones, emails, opts)
	mux.Handle(registrationPath, registrationHandler)

	recoveryPath, recoveryHandler := NewRecoveryServiceHandler(recoveryRepo, captcha, recoveryCodes, passwordResetFreeze, opts)
	mux.Handle(recoveryPath, recoveryHandler)

	return natsrpc.ServeHandler(nc, log, env, serviceName, version, mux)
}
