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
	identityv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity"
	"dmatech.ru/internal/pkg/contracts/webapi/v1/identity/authv1connect"
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
type PhoneSender interface {
	SendCode(ctx context.Context, phone, code string) error
}
type EmailSender interface {
	SendCode(ctx context.Context, email, code string) error
}

// RegistrationHandler реализует authv1connect.IdentityRegistrationServiceHandler.
type RegistrationHandler struct {
	authv1connect.UnimplementedIdentityRegistrationServiceHandler

	repo    RegistrationRepository
	captcha CaptchaClient
	phones  PhoneSender
	emails  EmailSender
}

// NewRegistrationHandler создаёт обработчик IdentityRegistrationService.
func NewRegistrationHandler(repo RegistrationRepository, captcha CaptchaClient, phones PhoneSender, emails EmailSender) *RegistrationHandler {
	return &RegistrationHandler{repo: repo, captcha: captcha, phones: phones, emails: emails}
}

// NewRegistrationServiceHandler собирает HTTP-обработчик
// IdentityRegistrationService для монтирования в веб-сервер.
func NewRegistrationServiceHandler(repo RegistrationRepository, captcha CaptchaClient, phones PhoneSender, emails EmailSender, opts ...connect.HandlerOption) (string, http.Handler) {
	return authv1connect.NewIdentityRegistrationServiceHandler(NewRegistrationHandler(repo, captcha, phones, emails), opts...)
}

// verifyPoW — true, только если challenge/nonce присланы и проверка у
// captcha-модуля прошла. Пустые challenge/nonce — это "капча ещё не
// решалась", а не ошибка вызова, поэтому не обращаемся к captcha впустую.
func (h *RegistrationHandler) verifyPoW(ctx context.Context, challenge, nonce string) (bool, error) {
	if challenge == "" || nonce == "" {
		return false, nil
	}
	return h.captcha.VerifyChallenge(ctx, challenge, nonce)
}

// CheckIdentifier проверяет существование аккаунта. См. doc-комментарий
// сервиса в registration.proto про осознанную утечку и её ограничители.
func (h *RegistrationHandler) CheckIdentifier(
	ctx context.Context,
	req *connect.Request[identityv1.CheckIdentifierRequest],
) (*connect.Response[identityv1.CheckIdentifierResponse], error) {
	identifier := strings.TrimSpace(req.Msg.GetIdentifier())
	if identifier == "" {
		return nil, fieldError(connect.CodeInvalidArgument, "identifier", "identifier must not be empty")
	}

	ok, err := h.verifyPoW(ctx, req.Msg.GetPowChallenge(), req.Msg.GetPowNonce())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return connect.NewResponse(&identityv1.CheckIdentifierResponse{
			Status: identityv1.CheckIdentifierResponse_STATUS_CAPTCHA_REQUIRED,
		}), nil
	}

	_, err = h.repo.FindAuth(ctx, identifier)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return connect.NewResponse(&identityv1.CheckIdentifierResponse{
			Status: identityv1.CheckIdentifierResponse_STATUS_NOT_FOUND,
		}), nil
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	default:
		return connect.NewResponse(&identityv1.CheckIdentifierResponse{
			Status: identityv1.CheckIdentifierResponse_STATUS_EXISTS,
		}), nil
	}
}

// StartPhoneVerification — первый шаг регистрации.
func (h *RegistrationHandler) StartPhoneVerification(
	ctx context.Context,
	req *connect.Request[identityv1.StartPhoneVerificationRequest],
) (*connect.Response[identityv1.StartPhoneVerificationResponse], error) {
	phone := strings.TrimSpace(req.Msg.GetPhone())
	if phone == "" {
		return nil, fieldError(connect.CodeInvalidArgument, "phone", "phone must not be empty")
	}
	if !req.Msg.GetPersonalDataConsent() {
		return nil, fieldError(connect.CodeInvalidArgument, "personal_data_consent", "personal data consent is required")
	}

	ok, err := h.verifyPoW(ctx, req.Msg.GetPowChallenge(), req.Msg.GetPowNonce())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return connect.NewResponse(&identityv1.StartPhoneVerificationResponse{
			Status: identityv1.StartPhoneVerificationResponse_STATUS_CAPTCHA_REQUIRED,
		}), nil
	}

	exists, err := h.repo.PhoneExists(ctx, phone)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if exists {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("phone is already registered"))
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

	return connect.NewResponse(&identityv1.StartPhoneVerificationResponse{
		Status:                identityv1.StartPhoneVerificationResponse_STATUS_STARTED,
		RegistrationSessionId: sessionID,
	}), nil
}

// ConfirmPhoneVerification проверяет код из SMS.
func (h *RegistrationHandler) ConfirmPhoneVerification(
	ctx context.Context,
	req *connect.Request[identityv1.ConfirmPhoneVerificationRequest],
) (*connect.Response[identityv1.ConfirmPhoneVerificationResponse], error) {
	sessionID := strings.TrimSpace(req.Msg.GetRegistrationSessionId())
	code := strings.TrimSpace(req.Msg.GetCode())
	if sessionID == "" || code == "" {
		return nil, fieldError(connect.CodeInvalidArgument, "code", "registration_session_id and code are required")
	}

	err := h.repo.VerifyPhoneCode(ctx, sessionID, hashOTP(code))
	if cerr := mapVerifyCodeError(err); cerr != nil {
		return nil, cerr
	}
	return connect.NewResponse(&identityv1.ConfirmPhoneVerificationResponse{}), nil
}

// StartEmailVerification — шаг после подтверждения телефона.
func (h *RegistrationHandler) StartEmailVerification(
	ctx context.Context,
	req *connect.Request[identityv1.StartEmailVerificationRequest],
) (*connect.Response[identityv1.StartEmailVerificationResponse], error) {
	sessionID := strings.TrimSpace(req.Msg.GetRegistrationSessionId())
	email := strings.TrimSpace(req.Msg.GetEmail())
	if sessionID == "" || email == "" {
		return nil, fieldError(connect.CodeInvalidArgument, "email", "registration_session_id and email are required")
	}

	ok, err := h.verifyPoW(ctx, req.Msg.GetPowChallenge(), req.Msg.GetPowNonce())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return connect.NewResponse(&identityv1.StartEmailVerificationResponse{
			Status: identityv1.StartEmailVerificationResponse_STATUS_CAPTCHA_REQUIRED,
		}), nil
	}

	exists, err := h.repo.EmailExists(ctx, email)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if exists {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("email is already registered"))
	}

	code, err := generateOTP()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	err = h.repo.StartEmailVerification(ctx, sessionID, email, hashOTP(code))
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, errors.New("registration session not found or expired"))
	case errors.Is(err, repository.ErrPhoneNotVerified):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("phone must be verified first"))
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if err := h.emails.SendCode(ctx, email, code); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("send email: %w", err))
	}

	return connect.NewResponse(&identityv1.StartEmailVerificationResponse{
		Status: identityv1.StartEmailVerificationResponse_STATUS_STARTED,
	}), nil
}

// ConfirmEmailVerification проверяет код из письма.
func (h *RegistrationHandler) ConfirmEmailVerification(
	ctx context.Context,
	req *connect.Request[identityv1.ConfirmEmailVerificationRequest],
) (*connect.Response[identityv1.ConfirmEmailVerificationResponse], error) {
	sessionID := strings.TrimSpace(req.Msg.GetRegistrationSessionId())
	code := strings.TrimSpace(req.Msg.GetCode())
	if sessionID == "" || code == "" {
		return nil, fieldError(connect.CodeInvalidArgument, "code", "registration_session_id and code are required")
	}

	err := h.repo.VerifyEmailCode(ctx, sessionID, hashOTP(code))
	if cerr := mapVerifyCodeError(err); cerr != nil {
		return nil, cerr
	}
	return connect.NewResponse(&identityv1.ConfirmEmailVerificationResponse{}), nil
}

// CompleteRegistration — финальный шаг: пароль задаётся клиентом локально
// (как и IdentityAuthService.Register), сервер получает только salt/verifier.
func (h *RegistrationHandler) CompleteRegistration(
	ctx context.Context,
	req *connect.Request[identityv1.CompleteRegistrationRequest],
) (*connect.Response[identityv1.CompleteRegistrationResponse], error) {
	sessionID := strings.TrimSpace(req.Msg.GetRegistrationSessionId())
	if sessionID == "" {
		return nil, fieldError(connect.CodeInvalidArgument, "registration_session_id", "registration_session_id must not be empty")
	}
	salt := req.Msg.GetSalt()
	verifier := req.Msg.GetVerifier()
	if len(salt) == 0 {
		return nil, fieldError(connect.CodeInvalidArgument, "salt", "salt must not be empty")
	}
	if !srp.ValidVerifier(verifier) {
		return nil, fieldError(connect.CodeInvalidArgument, "verifier", "verifier is missing or malformed")
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
		return nil, connect.NewError(connect.CodeNotFound, errors.New("registration session not found or expired"))
	case errors.Is(err, repository.ErrPhoneNotVerified):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("phone must be verified first"))
	case errors.Is(err, repository.ErrEmailNotVerified):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("email must be verified first"))
	case errors.Is(err, repository.ErrAlreadyExists):
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("account already exists"))
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&identityv1.CompleteRegistrationResponse{UserId: userID}), nil
}

// mapVerifyCodeError переводит ошибки repository.VerifyPhoneCode/
// VerifyEmailCode в connect-ошибки — общие для обоих шагов подтверждения.
func mapVerifyCodeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, repository.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("registration session not found or expired"))
	case errors.Is(err, repository.ErrTooManyAttempts):
		return connect.NewError(connect.CodeResourceExhausted, errors.New("too many attempts, request a new code"))
	case errors.Is(err, repository.ErrCodeMismatch):
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid code"))
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
