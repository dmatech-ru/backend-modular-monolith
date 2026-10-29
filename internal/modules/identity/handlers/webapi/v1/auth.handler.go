package v1

import (
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"

	"dmatech.ru/internal/pkg/authctx"
	"dmatech.ru/internal/pkg/locale"
	"dmatech.ru/internal/pkg/natsrpc"
	"dmatech.ru/internal/pkg/validation"
)

// Subscribe подписывает IdentityAuthService, IdentityRegistrationService,
// IdentityRecoveryService и IdentitySessionService на входящие запросы
// через NATS: один общий mux, одна подписка на весь модуль (методы
// различает путь запроса — коллизий между сервисами нет), чтобы гейтвей
// мог достучаться до них по маршруту {env}.{serviceName}.{version}.
//
// Все четыре сервиса получают три общих интерцептора: validation.Interceptor
// (статическая проверка формы запроса по buf.validate.* правилам в .proto —
// см. doc-комментарий пакета validation), locale.ServerInterceptor (кладёт
// локаль запроса в context.Context для дальнейших вызовов других модулей,
// см. doc-комментарий пакета locale) и authctx.ServerInterceptor (кладёт
// личность вызывающего, если gateway её проверил — см. doc-комментарий
// пакета authctx; у identity сегодня нет метода, который бы её
// требовал, но интерцептор подключён заранее, тем же приёмом, что и
// locale/validation, для единообразия) — все применяются ДО тела метода
// хендлера.
func Subscribe(
	nc *nats.Conn, log *slog.Logger, env, serviceName, version string,
	validator validation.Validator,
	repo Repository, antiEnumKey []byte,
	regRepo RegistrationRepository, captcha CaptchaClient, phones PhoneSender, emails EmailSender,
	recoveryRepo RecoveryRepository, recoveryCodes RecoveryCodeSender, passwordChangedAlerts PasswordChangedNotifier, passwordResetFreeze time.Duration,
	sessions Sessions,
) (*natsrpc.RPCSubscription, error) {
	opts := connect.WithInterceptors(validation.Interceptor(validator), locale.ServerInterceptor(), authctx.ServerInterceptor())

	mux := http.NewServeMux()

	authPath, authHandler := NewHandler(repo, antiEnumKey, sessions, opts)
	mux.Handle(authPath, authHandler)

	registrationPath, registrationHandler := NewRegistrationServiceHandler(regRepo, captcha, phones, emails, sessions, opts)
	mux.Handle(registrationPath, registrationHandler)

	recoveryPath, recoveryHandler := NewRecoveryServiceHandler(recoveryRepo, captcha, recoveryCodes, sessions, passwordChangedAlerts, log, passwordResetFreeze, opts)
	mux.Handle(recoveryPath, recoveryHandler)

	sessionPath, sessionHandler := NewSessionServiceHandler(sessions, opts)
	mux.Handle(sessionPath, sessionHandler)

	return natsrpc.ServeHandler(nc, log, env, serviceName, version, mux)
}
