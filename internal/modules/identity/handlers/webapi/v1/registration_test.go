package v1

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"dmatech.ru/internal/modules/identity/repository"
	"dmatech.ru/internal/pkg/authctx"
	registrationv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/registration"
	"dmatech.ru/internal/pkg/srp"
)

const fakeMaxCodeAttempts = 5

// fakeRegistrationRepository — RegistrationRepository в памяти для тестов.
type fakeRegistrationRepository struct {
	mu                  sync.Mutex
	existingIdentifiers map[string]bool
	phones              map[string]bool
	emails              map[string]bool
	sessions            map[string]*fakeRegSession
	nextID              int
}

type fakeRegSession struct {
	phone, email                 string
	phoneVerified, emailVerified bool
	phoneCodeHash, emailCodeHash []byte
	phoneAttempts, emailAttempts int
}

func newFakeRegistrationRepository() *fakeRegistrationRepository {
	return &fakeRegistrationRepository{
		existingIdentifiers: map[string]bool{},
		phones:              map[string]bool{},
		emails:              map[string]bool{},
		sessions:            map[string]*fakeRegSession{},
	}
}

func (f *fakeRegistrationRepository) FindAuth(_ context.Context, identifier string) (repository.AuthRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.existingIdentifiers[identifier] {
		return repository.AuthRecord{UserID: "existing-user"}, nil
	}
	return repository.AuthRecord{}, repository.ErrNotFound
}

func (f *fakeRegistrationRepository) PhoneExists(_ context.Context, phone string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.phones[phone], nil
}

func (f *fakeRegistrationRepository) EmailExists(_ context.Context, email string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.emails[email], nil
}

func (f *fakeRegistrationRepository) CreatePhoneSession(_ context.Context, phone string, _ bool, codeHash []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("session-%d", f.nextID)
	f.sessions[id] = &fakeRegSession{phone: phone, phoneCodeHash: codeHash}
	return id, nil
}

func (f *fakeRegistrationRepository) VerifyPhoneCode(_ context.Context, sessionID string, codeHash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	if !ok {
		return repository.ErrNotFound
	}
	if s.phoneAttempts >= fakeMaxCodeAttempts {
		return repository.ErrTooManyAttempts
	}
	if !bytes.Equal(s.phoneCodeHash, codeHash) {
		s.phoneAttempts++
		return repository.ErrCodeMismatch
	}
	s.phoneVerified = true
	return nil
}

func (f *fakeRegistrationRepository) StartEmailVerification(_ context.Context, sessionID, email string, codeHash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	if !ok {
		return repository.ErrNotFound
	}
	if !s.phoneVerified {
		return repository.ErrPhoneNotVerified
	}
	s.email = email
	s.emailCodeHash = codeHash
	s.emailAttempts = 0
	s.emailVerified = false
	return nil
}

func (f *fakeRegistrationRepository) VerifyEmailCode(_ context.Context, sessionID string, codeHash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	if !ok {
		return repository.ErrNotFound
	}
	if s.emailAttempts >= fakeMaxCodeAttempts {
		return repository.ErrTooManyAttempts
	}
	if !bytes.Equal(s.emailCodeHash, codeHash) {
		s.emailAttempts++
		return repository.ErrCodeMismatch
	}
	s.emailVerified = true
	return nil
}

func (f *fakeRegistrationRepository) CompleteRegistration(_ context.Context, sessionID string, p repository.CompleteRegistrationParams) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = p
	s, ok := f.sessions[sessionID]
	if !ok {
		return "", repository.ErrNotFound
	}
	if !s.phoneVerified {
		return "", repository.ErrPhoneNotVerified
	}
	if !s.emailVerified {
		return "", repository.ErrEmailNotVerified
	}
	if f.phones[s.phone] || f.emails[s.email] {
		return "", repository.ErrAlreadyExists
	}
	f.phones[s.phone] = true
	f.emails[s.email] = true
	delete(f.sessions, sessionID)
	f.nextID++
	return fmt.Sprintf("user-%d", f.nextID), nil
}

// fakeCaptchaClient — CaptchaClient, управляемый тестом напрямую: не
// пересчитывает настоящий PoW (это уже проверено в internal/pkg/pow и
// internal/modules/captcha) — здесь важно только поведение хендлера в
// зависимости от результата проверки.
type fakeCaptchaClient struct {
	result bool
	err    error
}

func (f *fakeCaptchaClient) VerifyChallenge(context.Context, string, string) (bool, error) {
	return f.result, f.err
}

// fakeSender — PhoneSender/EmailSender, запоминающий отправленные коды.
type fakeSender struct {
	mu   sync.Mutex
	sent map[string]string // target -> последний отправленный код
}

func newFakeSender() *fakeSender { return &fakeSender{sent: map[string]string{}} }

func (f *fakeSender) SendCode(_ context.Context, target, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[target] = code
	return nil
}

func (f *fakeSender) codeFor(target string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[target]
}

func newTestRegistrationHandler(approveCaptcha bool) (*RegistrationHandler, *fakeRegistrationRepository, *fakeSender, *fakeSender) {
	repo := newFakeRegistrationRepository()
	phones := newFakeSender()
	emails := newFakeSender()
	h := NewRegistrationHandler(repo, &fakeCaptchaClient{result: approveCaptcha}, phones, emails, newFakeSessions())
	return h, repo, phones, emails
}

func TestCheckIdentifier_RequiresCaptchaWhenNotSolved(t *testing.T) {
	h, _, _, _ := newTestRegistrationHandler(false)

	resp, err := h.CheckIdentifier(context.Background(), connect.NewRequest(&registrationv1.CheckIdentifierRequest{
		Identifier: "alice@example.com",
	}))
	if err != nil {
		t.Fatalf("CheckIdentifier: %v", err)
	}
	if resp.Msg.GetStatus() != registrationv1.CheckIdentifierResponse_STATUS_CAPTCHA_REQUIRED {
		t.Fatalf("status = %v, want STATUS_CAPTCHA_REQUIRED", resp.Msg.GetStatus())
	}
}

func TestCheckIdentifier_ExistsAndNotFound(t *testing.T) {
	h, repo, _, _ := newTestRegistrationHandler(true)
	repo.existingIdentifiers["alice@example.com"] = true

	resp, err := h.CheckIdentifier(context.Background(), connect.NewRequest(&registrationv1.CheckIdentifierRequest{
		Identifier: "alice@example.com", PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("CheckIdentifier: %v", err)
	}
	if resp.Msg.GetStatus() != registrationv1.CheckIdentifierResponse_STATUS_EXISTS {
		t.Fatalf("status = %v, want STATUS_EXISTS", resp.Msg.GetStatus())
	}

	resp, err = h.CheckIdentifier(context.Background(), connect.NewRequest(&registrationv1.CheckIdentifierRequest{
		Identifier: "bob@example.com", PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("CheckIdentifier: %v", err)
	}
	if resp.Msg.GetStatus() != registrationv1.CheckIdentifierResponse_STATUS_NOT_FOUND {
		t.Fatalf("status = %v, want STATUS_NOT_FOUND", resp.Msg.GetStatus())
	}
}

func TestStartPhoneVerification_RejectsExistingPhone(t *testing.T) {
	h, repo, _, _ := newTestRegistrationHandler(true)
	repo.phones["+70000000000"] = true

	_, err := h.StartPhoneVerification(context.Background(), connect.NewRequest(&registrationv1.StartPhoneVerificationRequest{
		Phone: "+70000000000", PersonalDataConsent: true, PowChallenge: "c", PowNonce: "n",
	}))
	if code := connectCode(t, err); code != connect.CodeAlreadyExists {
		t.Fatalf("code = %v, want CodeAlreadyExists", code)
	}
	if key := errorKey(t, err); key != "identity.phone.already_exists" {
		t.Fatalf("key = %q, want %q", key, "identity.phone.already_exists")
	}
}

func TestStartPhoneVerification_SendsCodeAndCreatesSession(t *testing.T) {
	h, _, phones, _ := newTestRegistrationHandler(true)

	resp, err := h.StartPhoneVerification(context.Background(), connect.NewRequest(&registrationv1.StartPhoneVerificationRequest{
		Phone: "+70000000000", PersonalDataConsent: true, PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("StartPhoneVerification: %v", err)
	}
	if resp.Msg.GetStatus() != registrationv1.StartPhoneVerificationResponse_STATUS_STARTED {
		t.Fatalf("status = %v, want STATUS_STARTED", resp.Msg.GetStatus())
	}
	if resp.Msg.GetRegistrationSessionId() == "" {
		t.Fatal("expected a non-empty registration_session_id")
	}
	if phones.codeFor("+70000000000") == "" {
		t.Fatal("expected a code to be sent to the phone")
	}
}

// fullRegistrationUpToPhone проходит шаг 1 (телефон+согласие) и подтверждает
// код из SMS, возвращает sessionID с уже подтверждённым телефоном.
func fullRegistrationUpToPhone(t *testing.T, h *RegistrationHandler, phones *fakeSender, phone string) string {
	t.Helper()
	ctx := context.Background()

	startResp, err := h.StartPhoneVerification(ctx, connect.NewRequest(&registrationv1.StartPhoneVerificationRequest{
		Phone: phone, PersonalDataConsent: true, PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("StartPhoneVerification: %v", err)
	}
	sessionID := startResp.Msg.GetRegistrationSessionId()
	code := phones.codeFor(phone)

	if _, err := h.ConfirmPhoneVerification(ctx, connect.NewRequest(&registrationv1.ConfirmPhoneVerificationRequest{
		RegistrationSessionId: sessionID, Code: code,
	})); err != nil {
		t.Fatalf("ConfirmPhoneVerification: %v", err)
	}
	return sessionID
}

func TestConfirmPhoneVerification_WrongCodeThenTooManyAttempts(t *testing.T) {
	h, _, phones, _ := newTestRegistrationHandler(true)
	ctx := context.Background()

	startResp, err := h.StartPhoneVerification(ctx, connect.NewRequest(&registrationv1.StartPhoneVerificationRequest{
		Phone: "+70000000000", PersonalDataConsent: true, PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("StartPhoneVerification: %v", err)
	}
	sessionID := startResp.Msg.GetRegistrationSessionId()
	_ = phones.codeFor("+70000000000") // реальный код игнорируем — шлём заведомо неверный

	var lastErr error
	for i := 0; i < fakeMaxCodeAttempts; i++ {
		_, lastErr = h.ConfirmPhoneVerification(ctx, connect.NewRequest(&registrationv1.ConfirmPhoneVerificationRequest{
			RegistrationSessionId: sessionID, Code: "000000",
		}))
		if code := connectCode(t, lastErr); code != connect.CodeUnauthenticated {
			t.Fatalf("attempt %d: code = %v, want CodeUnauthenticated", i, code)
		}
		if key := errorKey(t, lastErr); key != "identity.code.invalid" {
			t.Fatalf("attempt %d: key = %q, want %q", i, key, "identity.code.invalid")
		}
	}

	_, err = h.ConfirmPhoneVerification(ctx, connect.NewRequest(&registrationv1.ConfirmPhoneVerificationRequest{
		RegistrationSessionId: sessionID, Code: "000000",
	}))
	if code := connectCode(t, err); code != connect.CodeResourceExhausted {
		t.Fatalf("after exceeding attempts: code = %v, want CodeResourceExhausted", code)
	}
	if key := errorKey(t, err); key != "identity.code.too_many_attempts" {
		t.Fatalf("key = %q, want %q", key, "identity.code.too_many_attempts")
	}
}

func TestStartEmailVerification_RequiresPhoneVerifiedFirst(t *testing.T) {
	h, _, phones, _ := newTestRegistrationHandler(true)
	ctx := context.Background()

	startResp, err := h.StartPhoneVerification(ctx, connect.NewRequest(&registrationv1.StartPhoneVerificationRequest{
		Phone: "+70000000000", PersonalDataConsent: true, PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("StartPhoneVerification: %v", err)
	}
	_ = phones

	_, err = h.StartEmailVerification(ctx, connect.NewRequest(&registrationv1.StartEmailVerificationRequest{
		RegistrationSessionId: startResp.Msg.GetRegistrationSessionId(),
		Email:                 "alice@example.com", PowChallenge: "c", PowNonce: "n",
	}))
	if code := connectCode(t, err); code != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want CodeFailedPrecondition (phone not verified yet)", code)
	}
}

func TestStartEmailVerification_RejectsExistingEmail(t *testing.T) {
	h, repo, phones, _ := newTestRegistrationHandler(true)
	repo.emails["alice@example.com"] = true
	sessionID := fullRegistrationUpToPhone(t, h, phones, "+70000000000")

	_, err := h.StartEmailVerification(context.Background(), connect.NewRequest(&registrationv1.StartEmailVerificationRequest{
		RegistrationSessionId: sessionID, Email: "alice@example.com", PowChallenge: "c", PowNonce: "n",
	}))
	if code := connectCode(t, err); code != connect.CodeAlreadyExists {
		t.Fatalf("code = %v, want CodeAlreadyExists", code)
	}
}

func TestCompleteRegistration_RequiresEmailVerified(t *testing.T) {
	h, _, phones, _ := newTestRegistrationHandler(true)
	sessionID := fullRegistrationUpToPhone(t, h, phones, "+70000000000")

	salt, _ := srp.GenerateSalt()
	verifier := srp.ComputeVerifier("alice@example.com", "correct horse battery staple", salt)

	_, err := h.CompleteRegistration(context.Background(), connect.NewRequest(&registrationv1.CompleteRegistrationRequest{
		RegistrationSessionId: sessionID, Salt: salt, Verifier: verifier,
	}))
	if code := connectCode(t, err); code != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want CodeFailedPrecondition (email not verified yet)", code)
	}
}

func TestRegistrationFlow_EndToEndSucceeds(t *testing.T) {
	h, _, phones, emails := newTestRegistrationHandler(true)
	ctx := context.Background()
	const phone, email = "+70000000000", "alice@example.com"
	const password = "correct horse battery staple"

	// Шаг 0: аккаунта ещё нет.
	checkResp, err := h.CheckIdentifier(ctx, connect.NewRequest(&registrationv1.CheckIdentifierRequest{
		Identifier: email, PowChallenge: "c", PowNonce: "n",
	}))
	if err != nil {
		t.Fatalf("CheckIdentifier: %v", err)
	}
	if checkResp.Msg.GetStatus() != registrationv1.CheckIdentifierResponse_STATUS_NOT_FOUND {
		t.Fatalf("status = %v, want STATUS_NOT_FOUND", checkResp.Msg.GetStatus())
	}

	// Шаг 1-2: телефон + подтверждение.
	sessionID := fullRegistrationUpToPhone(t, h, phones, phone)

	// Шаг 3: email.
	if _, err := h.StartEmailVerification(ctx, connect.NewRequest(&registrationv1.StartEmailVerificationRequest{
		RegistrationSessionId: sessionID, Email: email, PowChallenge: "c", PowNonce: "n",
	})); err != nil {
		t.Fatalf("StartEmailVerification: %v", err)
	}

	// Шаг 4: подтверждение email.
	emailCode := emails.codeFor(email)
	if _, err := h.ConfirmEmailVerification(ctx, connect.NewRequest(&registrationv1.ConfirmEmailVerificationRequest{
		RegistrationSessionId: sessionID, Code: emailCode,
	})); err != nil {
		t.Fatalf("ConfirmEmailVerification: %v", err)
	}

	// Шаг 5: пароль (клиент считает salt/verifier локально).
	salt, err := srp.GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	verifier := srp.ComputeVerifier(email, password, salt)

	completeResp, err := h.CompleteRegistration(ctx, connect.NewRequest(&registrationv1.CompleteRegistrationRequest{
		RegistrationSessionId: sessionID, Salt: salt, Verifier: verifier, FirstName: "Alice",
	}))
	if err != nil {
		t.Fatalf("CompleteRegistration: %v", err)
	}
	if completeResp.Msg.GetUserId() == "" {
		t.Fatal("expected a non-empty user_id")
	}

	// CompleteRegistration авто-логинит (см. doc-комментарий метода) — те
	// же три куки, что у IdentityAuthService.CompleteAuthentication.
	cookies := (&http.Response{Header: completeResp.Header()}).Cookies()
	names := map[string]bool{}
	for _, c := range cookies {
		names[c.Name] = true
	}
	for _, want := range []string{authctx.CookieAccessToken, authctx.CookieRefreshToken, authctx.CookieSessionInfo} {
		if !names[want] {
			t.Fatalf("missing %s cookie in CompleteRegistration response, got %v", want, names)
		}
	}

	// Повторный вызов с той же сессией — она одноразовая.
	_, err = h.CompleteRegistration(ctx, connect.NewRequest(&registrationv1.CompleteRegistrationRequest{
		RegistrationSessionId: sessionID, Salt: salt, Verifier: verifier,
	}))
	if code := connectCode(t, err); code != connect.CodeNotFound {
		t.Fatalf("second CompleteRegistration: code = %v, want CodeNotFound (session must be single-use)", code)
	}
}
