package validation

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	common "dmatech.ru/internal/pkg/contracts/webapi/v1/common"
	authv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/auth"
	registrationv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/registration"
)

// noopNext — UnaryFunc, подставляемый за Interceptor: если он вызван,
// значит валидация пропустила запрос.
func noopNext(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
	return connect.NewResponse(&authv1.InitializeSessionResponse{}), nil
}

func newTestInterceptor(t *testing.T) connect.UnaryInterceptorFunc {
	t.Helper()
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return Interceptor(v)
}

func TestInterceptor_RejectsEmptyRequiredField(t *testing.T) {
	next := newTestInterceptor(t)(noopNext)

	req := connect.NewRequest(&authv1.InitializeSessionRequest{Identifier: ""})
	_, err := next(context.Background(), req)
	if err == nil {
		t.Fatal("expected a validation error for an empty identifier")
	}

	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected a *connect.Error, got %T: %v", err, err)
	}
	if cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want CodeInvalidArgument", cerr.Code())
	}

	details := cerr.Details()
	if len(details) != 1 {
		t.Fatalf("len(details) = %d, want 1", len(details))
	}
	msg, derr := details[0].Value()
	if derr != nil {
		t.Fatalf("decode detail: %v", derr)
	}
	payload, ok := msg.(*common.ErrorPayload)
	if !ok {
		t.Fatalf("detail type = %T, want *common.ErrorPayload", msg)
	}
	if len(payload.GetFields()) != 1 {
		t.Fatalf("len(fields) = %d, want 1", len(payload.GetFields()))
	}
	field := payload.GetFields()[0]
	if field.GetName() != "identifier" {
		t.Fatalf("field name = %q, want %q", field.GetName(), "identifier")
	}
	if field.GetKey() != "identity.identifier.required" {
		t.Fatalf("field key = %q, want %q", field.GetKey(), "identity.identifier.required")
	}
}

func TestInterceptor_RejectsWhitespaceOnlyRequiredField(t *testing.T) {
	next := newTestInterceptor(t)(noopNext)

	req := connect.NewRequest(&authv1.InitializeSessionRequest{Identifier: "   "})
	_, err := next(context.Background(), req)
	if err == nil {
		t.Fatal("expected a validation error for a whitespace-only identifier")
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected a *connect.Error, got %T: %v", err, err)
	}
	if cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want CodeInvalidArgument", cerr.Code())
	}
}

func TestInterceptor_RejectsBoolRuleViolation(t *testing.T) {
	next := newTestInterceptor(t)(noopNext)

	req := connect.NewRequest(&registrationv1.StartPhoneVerificationRequest{
		Phone: "+70000000000", PersonalDataConsent: false,
	})
	_, err := next(context.Background(), req)
	if err == nil {
		t.Fatal("expected a validation error for personal_data_consent = false")
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected a *connect.Error, got %T: %v", err, err)
	}
	if cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want CodeInvalidArgument", cerr.Code())
	}
	details := cerr.Details()
	if len(details) != 1 {
		t.Fatalf("len(details) = %d, want 1", len(details))
	}
	msg, derr := details[0].Value()
	if derr != nil {
		t.Fatalf("decode detail: %v", derr)
	}
	payload := msg.(*common.ErrorPayload)
	if got := payload.GetFields()[0].GetKey(); got != "identity.personal_data_consent.required" {
		t.Fatalf("field key = %q, want %q", got, "identity.personal_data_consent.required")
	}
}

func TestInterceptor_AllowsValidRequest(t *testing.T) {
	next := newTestInterceptor(t)(noopNext)

	req := connect.NewRequest(&authv1.InitializeSessionRequest{Identifier: "alice@example.com"})
	_, err := next(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error for a valid request: %v", err)
	}
}
