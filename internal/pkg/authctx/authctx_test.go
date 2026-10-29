package authctx

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
)

var testSecret = []byte("test-jwt-signing-secret-32-bytes!")

func TestIssueThenVerify_RoundTrips(t *testing.T) {
	issuer := NewIssuer(testSecret, time.Hour)
	verifier := NewVerifier(testSecret)

	token, expiresAt, err := issuer.Issue(Claims{UserID: "user-1", Permissions: "AQ==", SessionID: "session-1"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if time.Until(expiresAt) <= 0 {
		t.Fatalf("expiresAt = %v, want a future time", expiresAt)
	}

	got, err := verifier.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.UserID != "user-1" || got.Permissions != "AQ==" || got.SessionID != "session-1" {
		t.Fatalf("Verify = %+v, want {user-1 AQ== session-1}", got)
	}
}

func TestVerify_RejectsWrongSecret(t *testing.T) {
	issuer := NewIssuer(testSecret, time.Hour)
	token, _, err := issuer.Issue(Claims{UserID: "user-1"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	verifier := NewVerifier([]byte("a-completely-different-secret!!"))
	if _, err := verifier.Verify(token); err != ErrInvalidToken {
		t.Fatalf("Verify err = %v, want ErrInvalidToken", err)
	}
}

func TestVerify_RejectsExpiredToken(t *testing.T) {
	issuer := NewIssuer(testSecret, -time.Minute) // уже просрочен в момент выпуска
	token, _, err := issuer.Issue(Claims{UserID: "user-1"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	verifier := NewVerifier(testSecret)
	if _, err := verifier.Verify(token); err != ErrInvalidToken {
		t.Fatalf("Verify err = %v, want ErrInvalidToken", err)
	}
}

// Защита от алгоритмической подмены: токен, подписанный "none" (без
// подписи вообще), не должен проходить проверку только потому, что сам
// заголовок токена заявляет alg=none.
func TestVerify_RejectsNoneAlgorithm(t *testing.T) {
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{Subject: "user-1"})
	token, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("build alg=none token: %v", err)
	}

	verifier := NewVerifier(testSecret)
	if _, err := verifier.Verify(token); err != ErrInvalidToken {
		t.Fatalf("Verify err = %v, want ErrInvalidToken", err)
	}
}

func TestVerify_RejectsGarbage(t *testing.T) {
	verifier := NewVerifier(testSecret)
	if _, err := verifier.Verify("not-a-jwt-at-all"); err != ErrInvalidToken {
		t.Fatalf("Verify err = %v, want ErrInvalidToken", err)
	}
}

func TestServerInterceptor_AnonymousWhenHeaderAbsent(t *testing.T) {
	var sawClaims bool
	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		_, sawClaims = FromContext(ctx)
		return nil, nil
	}

	wrapped := ServerInterceptor()(next)
	req := connect.NewRequest(&struct{}{})
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sawClaims {
		t.Fatal("expected no claims in context when the trusted header is absent")
	}
}

func TestServerInterceptor_PutsClaimsInContext(t *testing.T) {
	var got Claims
	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		got, _ = FromContext(ctx)
		return nil, nil
	}

	wrapped := ServerInterceptor()(next)
	req := connect.NewRequest(&struct{}{})
	req.Header().Set(HeaderUserID, "user-1")
	req.Header().Set(HeaderPermissions, "AQ==")
	req.Header().Set(HeaderSessionID, "session-1")
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.UserID != "user-1" || got.Permissions != "AQ==" || got.SessionID != "session-1" {
		t.Fatalf("claims = %+v, want {user-1 AQ== session-1}", got)
	}
}
