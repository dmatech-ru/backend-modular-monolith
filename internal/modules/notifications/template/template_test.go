package template

import (
	"errors"
	"strings"
	"testing"

	deliveryv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/delivery"
	templatesv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/templates"
	"dmatech.ru/internal/pkg/locale"
)

func TestNew_LoadsAllTemplatesWithoutError(t *testing.T) {
	if _, err := New(); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestRender_OTPCode_Email_ReturnsSubjectAndEscapedBody(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	subject, body, err := r.Render(
		templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE,
		deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
		locale.Default,
		map[string]string{"code": "123456"},
	)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if subject == "" {
		t.Fatal("expected a non-empty subject for the email channel")
	}
	if !strings.Contains(body, "123456") {
		t.Fatalf("body = %q, want it to contain the code", body)
	}
	if !strings.Contains(body, "<strong>") {
		t.Fatalf("body = %q, want HTML markup (html/template)", body)
	}
}

// html/template должен автоэскейпить переменные — иначе значение
// переменной (в проде — потенциально что-то, что хоть отдалённо
// получено от пользователя) могло бы внедрить произвольный HTML/JS в
// письмо.
func TestRender_Email_EscapesHTMLInVariables(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, body, err := r.Render(
		templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE,
		deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
		locale.Default,
		map[string]string{"code": "<script>alert(1)</script>"},
	)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(body, "<script>") {
		t.Fatalf("body = %q, want the <script> tag to be escaped", body)
	}
}

func TestRender_OTPCode_SMS_ReturnsPlainTextWithoutSubject(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	subject, body, err := r.Render(
		templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE,
		deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS,
		locale.Default,
		map[string]string{"code": "654321"},
	)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if subject != "" {
		t.Fatalf("subject = %q, want empty for a channel without a header", subject)
	}
	if !strings.Contains(body, "654321") {
		t.Fatalf("body = %q, want it to contain the code", body)
	}
	if strings.Contains(body, "<") {
		t.Fatalf("body = %q, plain-text channel must not contain HTML markup", body)
	}
}

func TestRender_PasswordResetCode_Email_ReturnsSubjectAndBody(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	subject, body, err := r.Render(
		templatesv1.TemplateKey_TEMPLATE_KEY_PASSWORD_RESET_CODE,
		deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
		locale.Default,
		map[string]string{"code": "111222"},
	)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if subject == "" {
		t.Fatal("expected a non-empty subject for the email channel")
	}
	if !strings.Contains(body, "111222") {
		t.Fatalf("body = %q, want it to contain the code", body)
	}
}

func TestRender_PasswordChanged_NoVariables(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	subject, body, err := r.Render(
		templatesv1.TemplateKey_TEMPLATE_KEY_PASSWORD_CHANGED,
		deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
		locale.Default,
		nil,
	)
	if err != nil {
		t.Fatalf("Render (email): %v", err)
	}
	if subject == "" || body == "" {
		t.Fatalf("subject/body = %q/%q, want both non-empty for the email channel", subject, body)
	}

	_, body, err = r.Render(
		templatesv1.TemplateKey_TEMPLATE_KEY_PASSWORD_CHANGED,
		deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS,
		locale.Default,
		nil,
	)
	if err != nil {
		t.Fatalf("Render (sms): %v", err)
	}
	if body == "" {
		t.Fatal("expected a non-empty body for the sms channel")
	}
}

// email.body.tmpl — только блок content; оформление (doctype, <html lang>)
// приходит из общей email_layout.tmpl (см. doc-комментарий template.go) —
// проверяем, что рендер и правда идёт через неё, а не просто совпадает по
// границам content.
func TestRender_Email_UsesSharedLayout(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, body, err := r.Render(
		templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE,
		deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
		"en",
		map[string]string{"code": "123456"},
	)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("body = %q, want the shared email_layout.tmpl doctype", body)
	}
	if !strings.Contains(body, `lang="en"`) {
		t.Fatalf("body = %q, want <html lang=\"en\"> from the layout, driven by the requested locale", body)
	}
}

// Render должен использовать именно переданную локаль, не всегда Default —
// иначе локализация шаблонов была бы фикцией.
func TestRender_UsesRequestedLocale(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, ruBody, err := r.Render(
		templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE,
		deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS,
		"ru",
		map[string]string{"code": "777"},
	)
	if err != nil {
		t.Fatalf("Render (ru): %v", err)
	}
	_, enBody, err := r.Render(
		templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE,
		deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS,
		"en",
		map[string]string{"code": "777"},
	)
	if err != nil {
		t.Fatalf("Render (en): %v", err)
	}
	if ruBody == enBody {
		t.Fatalf("ru and en bodies must differ, got identical text %q", ruBody)
	}
	if !strings.Contains(enBody, "Verification code") {
		t.Fatalf("en body = %q, want it to contain the English wording", enBody)
	}
}

// Локаль без перевода (например, её ещё не завели) не должна ломать
// отправку — Render тихо подставляет locale.Default (см. doc-комментарий
// пакета).
func TestRender_FallsBackToDefaultLocaleWhenUnsupported(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, fallbackBody, err := r.Render(
		templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE,
		deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS,
		"fr",
		map[string]string{"code": "777"},
	)
	if err != nil {
		t.Fatalf("Render (fr, unsupported): %v", err)
	}
	_, defaultBody, err := r.Render(
		templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE,
		deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS,
		locale.Default,
		map[string]string{"code": "777"},
	)
	if err != nil {
		t.Fatalf("Render (default): %v", err)
	}
	if fallbackBody != defaultBody {
		t.Fatalf("fallback body = %q, want it to match the default-locale body %q", fallbackBody, defaultBody)
	}
}

func TestRender_UnknownTemplateKey(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, _, err = r.Render(templatesv1.TemplateKey_TEMPLATE_KEY_UNSPECIFIED, deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL, locale.Default, nil)
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("err = %v, want ErrTemplateNotFound", err)
	}
}

func TestRender_MissingChannelTemplate(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// otp_code не завели для MAX — должна быть явная ошибка, не тихий
	// пустой результат.
	_, _, err = r.Render(templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE, deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_MAX, locale.Default, map[string]string{"code": "1"})
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("err = %v, want ErrTemplateNotFound", err)
	}
}
