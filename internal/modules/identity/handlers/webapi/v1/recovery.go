package v1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	"dmatech.ru/internal/modules/identity/repository"
	deliveryv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/delivery"
	recoveryv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/recovery"
	"dmatech.ru/internal/pkg/contracts/webapi/v1/identity/recovery/recoveryv1connect"
	"dmatech.ru/internal/pkg/srp"
)

// RecoveryRepository — доступ к данным, нужный обработчику восстановления.
// Отдельно от Repository/RegistrationRepository (SRP-вход, регистрация) —
// общих методов нет: восстановление не читает/не пишет challenge'ы SRP-входа
// и сессии регистрации.
type RecoveryRepository interface {
	FindAuthForRecovery(ctx context.Context, identifier string) (repository.RecoveryAccount, error)
	CreateRecoverySession(ctx context.Context, acc repository.RecoveryAccount) (string, error)
	GetRecoverySession(ctx context.Context, sessionID string) (repository.RecoverySessionRecord, error)
	SetRecoveryCode(ctx context.Context, sessionID string, codeHash []byte) error
	VerifyRecoveryCode(ctx context.Context, sessionID string, codeHash []byte) error
	ResetRecoveryPassword(ctx context.Context, sessionID string, salt, verifier []byte) (acc repository.RecoveryAccount, passwordChangedAt time.Time, err error)
}

// RecoveryCodeSender отправляет код восстановления по выбранному каналу.
// Единственная реализация — тонкая обёртка над peer.notifications.v1.
// NotificationService.Send с template_key = TEMPLATE_KEY_PASSWORD_RESET_CODE
// (см. identity.go/notifications_client.go). Для TELEGRAM/MAX destination —
// телефон, не chat_id: резолвинг в чат — забота notifications (см.
// doc-комментарий NotificationService) — и единственная сегодня заглушка на
// этом пути (ChatResolver, который всегда возвращает "чат не найден") живёт
// там же, не здесь.
type RecoveryCodeSender interface {
	SendCode(ctx context.Context, channel deliveryv1.DeliveryChannel, destination, code string) error
}

// SessionRevoker гасит все сессии пользователя — после сброса пароля (см.
// ResetPassword) ни один ранее выданный refresh-токен не должен
// продолжать работать. Отдельный от SessionStore интерфейс: там гашение
// по refresh-токену (Logout), здесь — по user_id, без какого-либо токена
// на руках у вызывающего кода.
type SessionRevoker interface {
	RevokeAllSessions(ctx context.Context, userID string) error
}

// PasswordChangedNotifier сообщает владельцу аккаунта, что пароль был
// изменён — отдельно на каждый известный контактный канал (email и/или
// телефон через SMS), не только на тот, что использовался для самого
// восстановления: если ResetPassword вызвал злоумышленник, захватив ровно
// один из каналов, у легитимного владельца должен быть шанс узнать об этом
// по другому (см. doc-комментарий ResetPassword). Единственная реализация —
// тонкая обёртка над peer.notifications.v1.NotificationService.Send с
// template_key = TEMPLATE_KEY_PASSWORD_CHANGED (см. notifications_client.go).
// Best-effort: ошибка отправки не блокирует уже совершённый сброс пароля
// (см. notifyPasswordChanged), только логируется.
type PasswordChangedNotifier interface {
	NotifyEmail(ctx context.Context, email string) error
	NotifyPhone(ctx context.Context, phone string) error
}

// RecoveryHandler реализует recoveryv1connect.IdentityRecoveryServiceHandler.
type RecoveryHandler struct {
	recoveryv1connect.UnimplementedIdentityRecoveryServiceHandler

	repo     RecoveryRepository
	captcha  CaptchaClient
	codes    RecoveryCodeSender
	sessions SessionRevoker
	alerts   PasswordChangedNotifier
	log      *slog.Logger
	// passwordResetFreeze — см. ResetPasswordResponse.frozen_until в
	// recovery.proto: период после смены пароля, задуманный как окно, в
	// течение которого чувствительные операции должны быть недоступны
	// (сегодня нигде не enforced — см. README "Известные ограничения").
	passwordResetFreeze time.Duration
}

// NewRecoveryHandler создаёт обработчик IdentityRecoveryService.
func NewRecoveryHandler(repo RecoveryRepository, captcha CaptchaClient, codes RecoveryCodeSender, sessions SessionRevoker, alerts PasswordChangedNotifier, log *slog.Logger, passwordResetFreeze time.Duration) *RecoveryHandler {
	return &RecoveryHandler{repo: repo, captcha: captcha, codes: codes, sessions: sessions, alerts: alerts, log: log, passwordResetFreeze: passwordResetFreeze}
}

// NewRecoveryServiceHandler собирает HTTP-обработчик IdentityRecoveryService
// для монтирования в веб-сервер.
func NewRecoveryServiceHandler(repo RecoveryRepository, captcha CaptchaClient, codes RecoveryCodeSender, sessions SessionRevoker, alerts PasswordChangedNotifier, log *slog.Logger, passwordResetFreeze time.Duration, opts ...connect.HandlerOption) (string, http.Handler) {
	return recoveryv1connect.NewIdentityRecoveryServiceHandler(NewRecoveryHandler(repo, captcha, codes, sessions, alerts, log, passwordResetFreeze), opts...)
}

// verifyPoW — см. комментарий у RegistrationHandler.verifyPoW; тот же приём,
// отдельный метод, потому что captcha — поле этого, другого, хендлера.
func (h *RecoveryHandler) verifyPoW(ctx context.Context, challenge, nonce string) (bool, error) {
	if challenge == "" || nonce == "" {
		return false, nil
	}
	return h.captcha.VerifyChallenge(ctx, challenge, nonce)
}

// recoveryMethod — канал плюс то, с какого поля RecoveryAccount берётся
// destination для него: единая точка, которую использует и InitiateRecovery
// (чтобы перечислить доступные методы), и SendRecoveryCode (чтобы проверить,
// что выбранный канал — один из перечисленных, и найти адрес).
//
// recoveryv1.DeliveryChannel — webapi-локальный enum этого контракта (см.
// doc-комментарий DeliveryChannel в recovery.proto), не
// peer.common.delivery.v1.DeliveryChannel — до него есть смысл добраться
// только в момент реального вызова NotificationService (см. toPeerChannel
// ниже), не раньше.
//
// DELIVERY_CHANNEL_MAX здесь нет: в отличие от TELEGRAM (см.
// doc-комментарий IdentityRecoveryService в recovery.proto — доставка через
// бота, без привязки заранее), у MAX в схеме пока нет даже колонки,
// определяющей, привязан ли аккаунт к нему (см. комментарий
// DELIVERY_CHANNEL_MAX в peer/common/v1/delivery/delivery.proto) — то есть
// сегодня невозможно даже решить, доступен ли он для конкретного аккаунта,
// не только отправить.
func recoveryDestination(acc repository.RecoveryAccount, channel recoveryv1.DeliveryChannel) (destination string, ok bool) {
	switch channel {
	case recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL:
		if acc.Email == nil || *acc.Email == "" {
			return "", false
		}
		return *acc.Email, true
	case recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS, recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_TELEGRAM:
		if acc.Phone == nil || *acc.Phone == "" {
			return "", false
		}
		return *acc.Phone, true
	default:
		return "", false
	}
}

// toPeerChannel переводит webapi-локальный recoveryv1.DeliveryChannel в
// peer.common.delivery.v1.DeliveryChannel, который реально ожидает
// peer.notifications.v1.NotificationService.Send. Явная функция, не общий
// тип — см. doc-комментарий DeliveryChannel в recovery.proto про то, почему
// webapi не переиспользует peer-овский enum напрямую: эти два enum'а
// совпадают по значениям намеренно (EMAIL/SMS/TELEGRAM/MAX), но это два
// разных контракта, которые имеют право разойтись, если потребуется.
func toPeerChannel(channel recoveryv1.DeliveryChannel) (deliveryv1.DeliveryChannel, bool) {
	switch channel {
	case recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL:
		return deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL, true
	case recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS:
		return deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS, true
	case recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_TELEGRAM:
		return deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_TELEGRAM, true
	case recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_MAX:
		return deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_MAX, true
	default:
		return deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_UNSPECIFIED, false
	}
}

// availableRecoveryMethods перечисляет каналы, для которых recoveryDestination
// вернул бы адрес, с замаскированным destination (см. doc-комментарий
// AvailableMethod в recovery.proto про осознанную утечку и её ограничители).
func availableRecoveryMethods(acc repository.RecoveryAccount) []*recoveryv1.AvailableMethod {
	var methods []*recoveryv1.AvailableMethod
	if acc.Email != nil && *acc.Email != "" {
		methods = append(methods, &recoveryv1.AvailableMethod{
			Channel:           recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
			MaskedDestination: maskEmail(*acc.Email),
		})
	}
	if acc.Phone != nil && *acc.Phone != "" {
		methods = append(methods,
			&recoveryv1.AvailableMethod{
				Channel:           recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS,
				MaskedDestination: maskPhone(*acc.Phone),
			},
			// TELEGRAM — без masked_destination: нет сохранённого адреса,
			// доставка пойдёт в чат, который определится через бота (см.
			// recoveryDestination и doc-комментарий сервиса).
			&recoveryv1.AvailableMethod{
				Channel: recoveryv1.DeliveryChannel_DELIVERY_CHANNEL_TELEGRAM,
			},
		)
	}
	return methods
}

// maskEmail маскирует всё, кроме первого символа локальной части — "a***@example.com".
func maskEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	return email[:1] + "***" + email[at:]
}

// maskPhone маскирует всё, кроме первых двух и последних двух символов.
func maskPhone(phone string) string {
	const visiblePrefix, visibleSuffix = 2, 2
	if len(phone) <= visiblePrefix+visibleSuffix {
		return strings.Repeat("*", len(phone))
	}
	masked := strings.Repeat("*", len(phone)-visiblePrefix-visibleSuffix)
	return phone[:visiblePrefix] + masked + phone[len(phone)-visibleSuffix:]
}

// InitiateRecovery проверяет identifier и возвращает доступные способы
// восстановления. См. doc-комментарий сервиса в recovery.proto про
// осознанную утечку и её ограничители.
func (h *RecoveryHandler) InitiateRecovery(
	ctx context.Context,
	req *connect.Request[recoveryv1.InitiateRecoveryRequest],
) (*connect.Response[recoveryv1.InitiateRecoveryResponse], error) {
	// identifier непустой — проверено до вызова этого метода (см.
	// buf.validate.field.cel на этом поле в recovery.proto).
	identifier := strings.TrimSpace(req.Msg.GetIdentifier())

	ok, err := h.verifyPoW(ctx, req.Msg.GetPowChallenge(), req.Msg.GetPowNonce())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return connect.NewResponse(&recoveryv1.InitiateRecoveryResponse{
			Status: recoveryv1.InitiateRecoveryResponse_STATUS_CAPTCHA_REQUIRED,
		}), nil
	}

	acc, err := h.repo.FindAuthForRecovery(ctx, identifier)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return connect.NewResponse(&recoveryv1.InitiateRecoveryResponse{
			Status: recoveryv1.InitiateRecoveryResponse_STATUS_NOT_FOUND,
		}), nil
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	methods := availableRecoveryMethods(acc)
	if len(methods) == 0 {
		// Аккаунт без email и телефона — восстановление недоступно никаким
		// каналом; с точки зрения раскрытия информации это не хуже, чем
		// обычный STATUS_NOT_FOUND (см. doc-комментарий сервиса).
		return connect.NewResponse(&recoveryv1.InitiateRecoveryResponse{
			Status: recoveryv1.InitiateRecoveryResponse_STATUS_NOT_FOUND,
		}), nil
	}

	sessionID, err := h.repo.CreateRecoverySession(ctx, acc)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&recoveryv1.InitiateRecoveryResponse{
		Status:            recoveryv1.InitiateRecoveryResponse_STATUS_AVAILABLE,
		RecoverySessionId: sessionID,
		AvailableMethods:  methods,
	}), nil
}

// SendRecoveryCode отправляет одноразовый код по выбранному каналу. Канал
// должен быть одним из тех, что InitiateRecovery перечислил бы как доступные
// для этого аккаунта (recoveryDestination — та же проверка, что
// availableRecoveryMethods использовала для перечисления) — иначе
// INVALID_ARGUMENT.
func (h *RecoveryHandler) SendRecoveryCode(
	ctx context.Context,
	req *connect.Request[recoveryv1.SendRecoveryCodeRequest],
) (*connect.Response[recoveryv1.SendRecoveryCodeResponse], error) {
	// recovery_session_id непустой — проверено до вызова этого метода (см.
	// buf.validate.field.cel на этом поле в recovery.proto).
	sessionID := strings.TrimSpace(req.Msg.GetRecoverySessionId())

	ok, err := h.verifyPoW(ctx, req.Msg.GetPowChallenge(), req.Msg.GetPowNonce())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return connect.NewResponse(&recoveryv1.SendRecoveryCodeResponse{
			Status: recoveryv1.SendRecoveryCodeResponse_STATUS_CAPTCHA_REQUIRED,
		}), nil
	}

	session, err := h.repo.GetRecoverySession(ctx, sessionID)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return nil, keyedError(connect.CodeNotFound, "identity.recovery_session.not_found", "recovery session not found or expired")
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	destination, ok := recoveryDestination(session.RecoveryAccount, req.Msg.GetChannel())
	if !ok {
		return nil, fieldError(connect.CodeInvalidArgument, "channel", "identity.recovery_channel.unavailable", "channel is not available for this recovery session")
	}
	peerChannel, ok := toPeerChannel(req.Msg.GetChannel())
	if !ok {
		return nil, fieldError(connect.CodeInvalidArgument, "channel", "identity.channel.unknown", "unknown channel")
	}

	code, err := generateOTP()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if err := h.repo.SetRecoveryCode(ctx, sessionID, hashOTP(code)); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, keyedError(connect.CodeNotFound, "identity.recovery_session.not_found", "recovery session not found or expired")
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if err := h.codes.SendCode(ctx, peerChannel, destination, code); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("send recovery code: %w", err))
	}

	return connect.NewResponse(&recoveryv1.SendRecoveryCodeResponse{
		Status: recoveryv1.SendRecoveryCodeResponse_STATUS_SENT,
	}), nil
}

// ConfirmRecoveryCode проверяет код — та же логика ошибок, что у
// ConfirmPhoneVerification/ConfirmEmailVerification (см. mapVerifyCodeError
// в registration.go).
func (h *RecoveryHandler) ConfirmRecoveryCode(
	ctx context.Context,
	req *connect.Request[recoveryv1.ConfirmRecoveryCodeRequest],
) (*connect.Response[recoveryv1.ConfirmRecoveryCodeResponse], error) {
	// recovery_session_id/code непусты — проверено до вызова этого метода
	// (см. buf.validate.field.cel на этих полях в recovery.proto).
	sessionID := strings.TrimSpace(req.Msg.GetRecoverySessionId())
	code := strings.TrimSpace(req.Msg.GetCode())

	err := h.repo.VerifyRecoveryCode(ctx, sessionID, hashOTP(code))
	if cerr := mapVerifyCodeError(err); cerr != nil {
		return nil, cerr
	}
	return connect.NewResponse(&recoveryv1.ConfirmRecoveryCodeResponse{}), nil
}

// ResetPassword — финальный шаг: пароль на сервер не передаётся (как и
// CompleteRegistration/CompleteAuthentication), только salt/verifier.
// Требует подтверждённого кода в этой сессии — иначе FAILED_PRECONDITION.
func (h *RecoveryHandler) ResetPassword(
	ctx context.Context,
	req *connect.Request[recoveryv1.ResetPasswordRequest],
) (*connect.Response[recoveryv1.ResetPasswordResponse], error) {
	// recovery_session_id/salt непусты — проверено до вызова этого метода
	// (см. buf.validate.field.cel на этих полях в recovery.proto). verifier
	// — не просто "непусто", а правдоподобный элемент группы SRP (см. тот
	// же комментарий у CompleteRegistrationRequest.verifier в registration.go).
	sessionID := strings.TrimSpace(req.Msg.GetRecoverySessionId())
	salt := req.Msg.GetSalt()
	verifier := req.Msg.GetVerifier()
	if !srp.ValidVerifier(verifier) {
		return nil, fieldError(connect.CodeInvalidArgument, "verifier", "identity.verifier.invalid", "verifier is missing or malformed")
	}

	acc, changedAt, err := h.repo.ResetRecoveryPassword(ctx, sessionID, salt, verifier)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return nil, keyedError(connect.CodeNotFound, "identity.recovery_session.not_found", "recovery session not found or expired")
	case errors.Is(err, repository.ErrRecoveryCodeNotVerified):
		return nil, keyedError(connect.CodeFailedPrecondition, "identity.recovery_code.not_confirmed", "recovery code must be confirmed first")
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Гасит все сессии пользователя (см. doc-комментарий ResetPassword в
	// recovery.proto) — после сброса пароля ни один ранее выданный
	// refresh-токен не должен продолжать работать.
	if err := h.sessions.RevokeAllSessions(ctx, acc.UserID); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	h.notifyPasswordChanged(ctx, acc)

	return connect.NewResponse(&recoveryv1.ResetPasswordResponse{
		FrozenUntil: changedAt.Add(h.passwordResetFreeze).Unix(),
	}), nil
}

// notifyPasswordChanged уведомляет владельца аккаунта на все известные
// контактные каналы (см. doc-комментарий PasswordChangedNotifier) —
// best-effort: сброс пароля уже совершён и сессии уже погашены, отказ
// самого уведомления не должен превращаться в ошибку ответа клиенту,
// только попадает в лог.
func (h *RecoveryHandler) notifyPasswordChanged(ctx context.Context, acc repository.RecoveryAccount) {
	if acc.Email != nil && *acc.Email != "" {
		if err := h.alerts.NotifyEmail(ctx, *acc.Email); err != nil {
			h.log.ErrorContext(ctx, "не удалось отправить уведомление о смене пароля (email)", "error", err)
		}
	}
	if acc.Phone != nil && *acc.Phone != "" {
		if err := h.alerts.NotifyPhone(ctx, *acc.Phone); err != nil {
			h.log.ErrorContext(ctx, "не удалось отправить уведомление о смене пароля (sms)", "error", err)
		}
	}
}
