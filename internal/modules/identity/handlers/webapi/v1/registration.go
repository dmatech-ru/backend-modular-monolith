package v1

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"dmatech.ru/internal/modules/identity/repository"
	"dmatech.ru/internal/pkg/clientip"
	registrationv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/registration"
	"dmatech.ru/internal/pkg/contracts/webapi/v1/identity/registration/registrationv1connect"
	"dmatech.ru/internal/pkg/srp"
)

// RegistrationRepository — доступ к данным, нужный обработчику регистрации.
// Отдельно от Repository (SRP-вход): пересекается только в FindAuth, нужном
// CheckIdentifier для той же семантики "существует", что у InitializeSession.
type RegistrationRepository interface {
	FindAuth(ctx context.Context, identifier string) (repository.AuthRecord, error)
	PhoneExists(ctx context.Context, phone string) (bool, error)
	EmailExists(ctx context.Context, email string) (bool, error)
	CreatePhoneSession(ctx context.Context, phone string, consent bool, codeHash []byte) (string, error)
	VerifyPhoneCode(ctx context.Context, sessionID string, codeHash []byte) error
	StartEmailVerification(ctx context.Context, sessionID, email string, codeHash []byte) error
	VerifyEmailCode(ctx context.Context, sessionID string, codeHash []byte) error
	CompleteRegistration(ctx context.Context, sessionID string, p repository.CompleteRegistrationParams) (string, error)
}

// CaptchaClient — проверка PoW-решения у captcha-модуля (peer.captcha.v1.
// CaptchaService.VerifyChallenge). Интерфейс объявлен на стороне
// потребителя: реальная реализация — тонкая обёртка над сгенерированным
// connect-клиентом (см. identity.go), тесты подставляют фейк без NATS.
type CaptchaClient interface {
	VerifyChallenge(ctx context.Context, challenge, nonce string) (valid bool, err error)
}

// PhoneSender/EmailSender — отправка кода подтверждения. Единственная
// реализация в этом репозитории — dev-заглушка, которая пишет код в лог
// (см. identity.go, logSender): реальная интеграция со СМС-гейтвеем/почтовым
// сервисом не входит в эту итерацию — см. README "Известные ограничения".
//
// Оба интерфейса имеют идентичную сигнатуру SendCode(ctx, string, string) —
// без маркер-методов IsPhoneSender/IsEmailSender любая реализация одного
// автоматически удовлетворяла бы и другому: конструктор при монтировании
// (identity.go) или будущая правка Subscribe могли бы поменять местами
// phones/emails, и это молча скомпилировалось бы и работало бы (SMS-код
// уходил бы в "емейловый" канал и наоборот). Методы экспортированы (а не
// isPhoneSender/isEmailSender), потому что реализации живут в другом
// пакете (identity, см. notifications_client.go) — неэкспортированный
// метод интерфейса может реализовать только тип из того же пакета, где
// объявлен интерфейс.
type PhoneSender interface {
	SendCode(ctx context.Context, phone, code string) error
	IsPhoneSender()
}
type EmailSender interface {
	SendCode(ctx context.Context, email, code string) error
	IsEmailSender()
}

// RegistrationHandler реализует registrationv1connect.IdentityRegistrationServiceHandler.
type RegistrationHandler struct {
	registrationv1connect.UnimplementedIdentityRegistrationServiceHandler

	repo     RegistrationRepository
	captcha  CaptchaClient
	phones   PhoneSender
	emails   EmailSender
	sessions SessionIssuer
	// abuse/powThreshold — адаптивный PoW (см. adaptivePoWGate в pow.go):
	// CheckIdentifier ключуется по IP вызывающего (см. clientip), т.к.
	// защищает от перечисления — переходов между МНОГИМИ identifier'ами;
	// StartPhoneVerification/StartEmailVerification — по адресату
	// (телефон/email), т.к. защищают от бомбёжки ОДНОГО номера/почты, не
	// зависящей от IP атакующего (см. doc-комментарий сервиса в
	// registration.proto).
	abuse        AbuseTracker
	powThreshold int
}

// NewRegistrationHandler создаёт обработчик IdentityRegistrationService.
func NewRegistrationHandler(repo RegistrationRepository, captcha CaptchaClient, phones PhoneSender, emails EmailSender, sessions SessionIssuer, abuse AbuseTracker, powThreshold int) *RegistrationHandler {
	return &RegistrationHandler{repo: repo, captcha: captcha, phones: phones, emails: emails, sessions: sessions, abuse: abuse, powThreshold: powThreshold}
}

// NewRegistrationServiceHandler собирает HTTP-обработчик
// IdentityRegistrationService для монтирования в веб-сервер.
func NewRegistrationServiceHandler(repo RegistrationRepository, captcha CaptchaClient, phones PhoneSender, emails EmailSender, sessions SessionIssuer, abuse AbuseTracker, powThreshold int, opts ...connect.HandlerOption) (string, http.Handler) {
	return registrationv1connect.NewIdentityRegistrationServiceHandler(NewRegistrationHandler(repo, captcha, phones, emails, sessions, abuse, powThreshold), opts...)
}

// CheckIdentifier проверяет существование аккаунта. См. doc-комментарий
// сервиса в registration.proto про осознанную утечку и её ограничители.
func (h *RegistrationHandler) CheckIdentifier(
	ctx context.Context,
	req *connect.Request[registrationv1.CheckIdentifierRequest],
) (*connect.Response[registrationv1.CheckIdentifierResponse], error) {
	// identifier непустой — проверено до вызова этого метода (см.
	// buf.validate.field.cel на этом поле в registration.proto).
	identifier := strings.TrimSpace(req.Msg.GetIdentifier())

	ok, err := adaptivePoWGate(ctx, h.captcha, h.abuse, h.powThreshold,
		abuseScopeCheckIdentifier, req.Header().Get(clientip.Header),
		req.Msg.GetPowChallenge(), req.Msg.GetPowNonce())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return connect.NewResponse(&registrationv1.CheckIdentifierResponse{
			Status: registrationv1.CheckIdentifierResponse_STATUS_CAPTCHA_REQUIRED,
		}), nil
	}

	_, err = h.repo.FindAuth(ctx, identifier)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return connect.NewResponse(&registrationv1.CheckIdentifierResponse{
			Status: registrationv1.CheckIdentifierResponse_STATUS_NOT_FOUND,
		}), nil
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	default:
		return connect.NewResponse(&registrationv1.CheckIdentifierResponse{
			Status: registrationv1.CheckIdentifierResponse_STATUS_EXISTS,
		}), nil
	}
}

// StartPhoneVerification — первый шаг регистрации.
func (h *RegistrationHandler) StartPhoneVerification(
	ctx context.Context,
	req *connect.Request[registrationv1.StartPhoneVerificationRequest],
) (*connect.Response[registrationv1.StartPhoneVerificationResponse], error) {
	// phone непустой и personal_data_consent — true: проверено до вызова
	// этого метода (см. buf.validate.field.cel на этих полях в
	// registration.proto).
	phone := strings.TrimSpace(req.Msg.GetPhone())

	ok, err := adaptivePoWGate(ctx, h.captcha, h.abuse, h.powThreshold,
		abuseScopeStartPhoneVerification, phone,
		req.Msg.GetPowChallenge(), req.Msg.GetPowNonce())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return connect.NewResponse(&registrationv1.StartPhoneVerificationResponse{
			Status: registrationv1.StartPhoneVerificationResponse_STATUS_CAPTCHA_REQUIRED,
		}), nil
	}

	exists, err := h.repo.PhoneExists(ctx, phone)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if exists {
		return nil, keyedError(connect.CodeAlreadyExists, "identity.phone.already_exists", "phone is already registered")
	}

	code, err := generateOTP()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	sessionID, err := h.repo.CreatePhoneSession(ctx, phone, true, hashOTP(code))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := h.phones.SendCode(ctx, phone, code); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("send sms: %w", err))
	}

	return connect.NewResponse(&registrationv1.StartPhoneVerificationResponse{
		Status:                registrationv1.StartPhoneVerificationResponse_STATUS_STARTED,
		RegistrationSessionId: sessionID,
	}), nil
}

// ConfirmPhoneVerification проверяет код из SMS.
func (h *RegistrationHandler) ConfirmPhoneVerification(
	ctx context.Context,
	req *connect.Request[registrationv1.ConfirmPhoneVerificationRequest],
) (*connect.Response[registrationv1.ConfirmPhoneVerificationResponse], error) {
	// registration_session_id/code непусты — проверено до вызова этого
	// метода (см. buf.validate.field.cel на этих полях в registration.proto).
	sessionID := strings.TrimSpace(req.Msg.GetRegistrationSessionId())
	code := strings.TrimSpace(req.Msg.GetCode())

	err := h.repo.VerifyPhoneCode(ctx, sessionID, hashOTP(code))
	if cerr := mapVerifyCodeError(err); cerr != nil {
		return nil, cerr
	}
	return connect.NewResponse(&registrationv1.ConfirmPhoneVerificationResponse{}), nil
}

// StartEmailVerification — шаг после подтверждения телефона.
func (h *RegistrationHandler) StartEmailVerification(
	ctx context.Context,
	req *connect.Request[registrationv1.StartEmailVerificationRequest],
) (*connect.Response[registrationv1.StartEmailVerificationResponse], error) {
	// registration_session_id/email непусты — проверено до вызова этого
	// метода (см. buf.validate.field.cel на этих полях в registration.proto).
	sessionID := strings.TrimSpace(req.Msg.GetRegistrationSessionId())
	email := strings.TrimSpace(req.Msg.GetEmail())

	// Ключ — lower(email), как EmailExists сравнивает email регистронезависимо
	// (см. repository.go): Alice@example.com и alice@EXAMPLE.com не должны
	// считаться двумя разными адресатами бомбёжки.
	ok, err := adaptivePoWGate(ctx, h.captcha, h.abuse, h.powThreshold,
		abuseScopeStartEmailVerification, strings.ToLower(email),
		req.Msg.GetPowChallenge(), req.Msg.GetPowNonce())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return connect.NewResponse(&registrationv1.StartEmailVerificationResponse{
			Status: registrationv1.StartEmailVerificationResponse_STATUS_CAPTCHA_REQUIRED,
		}), nil
	}

	exists, err := h.repo.EmailExists(ctx, email)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if exists {
		return nil, keyedError(connect.CodeAlreadyExists, "identity.email.already_exists", "email is already registered")
	}

	code, err := generateOTP()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	err = h.repo.StartEmailVerification(ctx, sessionID, email, hashOTP(code))
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return nil, keyedError(connect.CodeNotFound, "identity.registration_session.not_found", "registration session not found or expired")
	case errors.Is(err, repository.ErrPhoneNotVerified):
		return nil, keyedError(connect.CodeFailedPrecondition, "identity.phone.not_verified", "phone must be verified first")
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if err := h.emails.SendCode(ctx, email, code); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("send email: %w", err))
	}

	return connect.NewResponse(&registrationv1.StartEmailVerificationResponse{
		Status: registrationv1.StartEmailVerificationResponse_STATUS_STARTED,
	}), nil
}

// ConfirmEmailVerification проверяет код из письма.
func (h *RegistrationHandler) ConfirmEmailVerification(
	ctx context.Context,
	req *connect.Request[registrationv1.ConfirmEmailVerificationRequest],
) (*connect.Response[registrationv1.ConfirmEmailVerificationResponse], error) {
	// registration_session_id/code непусты — проверено до вызова этого
	// метода (см. buf.validate.field.cel на этих полях в registration.proto).
	sessionID := strings.TrimSpace(req.Msg.GetRegistrationSessionId())
	code := strings.TrimSpace(req.Msg.GetCode())

	err := h.repo.VerifyEmailCode(ctx, sessionID, hashOTP(code))
	if cerr := mapVerifyCodeError(err); cerr != nil {
		return nil, cerr
	}
	return connect.NewResponse(&registrationv1.ConfirmEmailVerificationResponse{}), nil
}

// CompleteRegistration — финальный шаг: пароль задаётся клиентом локально
// (как и IdentityAuthService.Register), сервер получает только
// salt/verifier. При успехе сразу выпускает сессию (см.
// IdentityAuthService.CompleteAuthentication и doc-комментарий этого RPC
// в registration.proto) — отдельный повторный вход сразу после
// регистрации был бы лишним шагом.
func (h *RegistrationHandler) CompleteRegistration(
	ctx context.Context,
	req *connect.Request[registrationv1.CompleteRegistrationRequest],
) (*connect.Response[registrationv1.CompleteRegistrationResponse], error) {
	// registration_session_id/salt непусты — проверено до вызова этого
	// метода (см. buf.validate.field.cel на этих полях в registration.proto).
	// verifier — не просто "непусто", а правдоподобный элемент группы SRP
	// (граница размера — деталь internal/pkg/srp, не формы запроса), эта
	// проверка остаётся здесь.
	sessionID := strings.TrimSpace(req.Msg.GetRegistrationSessionId())
	salt := req.Msg.GetSalt()
	verifier := req.Msg.GetVerifier()
	if !srp.ValidVerifier(verifier) {
		return nil, fieldError(connect.CodeInvalidArgument, "verifier", "identity.verifier.invalid", "verifier is missing or malformed")
	}

	userID, err := h.repo.CompleteRegistration(ctx, sessionID, repository.CompleteRegistrationParams{
		Salt:       salt,
		Verifier:   verifier,
		FirstName:  strings.TrimSpace(req.Msg.GetFirstName()),
		LastName:   strings.TrimSpace(req.Msg.GetLastName()),
		MiddleName: strings.TrimSpace(req.Msg.GetMiddleName()),
	})
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return nil, keyedError(connect.CodeNotFound, "identity.registration_session.not_found", "registration session not found or expired")
	case errors.Is(err, repository.ErrPhoneNotVerified):
		return nil, keyedError(connect.CodeFailedPrecondition, "identity.phone.not_verified", "phone must be verified first")
	case errors.Is(err, repository.ErrEmailNotVerified):
		return nil, keyedError(connect.CodeFailedPrecondition, "identity.email.not_verified", "email must be verified first")
	case errors.Is(err, repository.ErrAlreadyExists):
		return nil, keyedError(connect.CodeAlreadyExists, "identity.account.already_exists", "account already exists")
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	access, refresh, permissions, accessExpiresAt, err := h.sessions.IssueSession(ctx, userID, req.Header().Get("User-Agent"))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := connect.NewResponse(&registrationv1.CompleteRegistrationResponse{UserId: userID})
	setSessionCookies(resp.Header(), access, refresh, permissions, accessExpiresAt)
	return resp, nil
}

// mapVerifyCodeError переводит ошибки repository.VerifyPhoneCode/
// VerifyEmailCode в connect-ошибки — общие для обоих шагов подтверждения.
func mapVerifyCodeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, repository.ErrNotFound):
		return keyedError(connect.CodeNotFound, "identity.registration_session.not_found", "registration session not found or expired")
	case errors.Is(err, repository.ErrTooManyAttempts):
		return keyedError(connect.CodeResourceExhausted, "identity.code.too_many_attempts", "too many attempts, request a new code")
	case errors.Is(err, repository.ErrCodeMismatch):
		return keyedError(connect.CodeUnauthenticated, "identity.code.invalid", "invalid code")
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

// generateOTP возвращает случайный 6-значный код подтверждения.
func generateOTP() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate otp: %w", err)
	}
	n := binary.BigEndian.Uint32(buf) % 1_000_000
	return fmt.Sprintf("%06d", n), nil
}

// hashOTP — код подтверждения хранится в базе только хешем, не в открытом виде.
func hashOTP(code string) []byte {
	sum := sha256.Sum256([]byte(code))
	return sum[:]
}
