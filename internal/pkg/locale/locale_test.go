package locale

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
)

func header(pairs ...string) http.Header {
	h := make(http.Header)
	for i := 0; i < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

func TestParse_XLocaleTakesPriority(t *testing.T) {
	got := Parse(header(HeaderLocale, "en", HeaderAcceptLanguage, "ru-RU,ru;q=0.9"))
	if got != "en" {
		t.Fatalf("Parse = %q, want %q", got, "en")
	}
}

func TestParse_FallsBackToAcceptLanguageWhenXLocaleUnset(t *testing.T) {
	got := Parse(header(HeaderAcceptLanguage, "fr-FR,fr;q=0.9,en;q=0.8"))
	if got != "en" {
		t.Fatalf("Parse = %q, want %q (fr is unsupported, en is the next supported tag by q)", got, "en")
	}
}

func TestParse_FallsBackToAcceptLanguageWhenXLocaleUnsupported(t *testing.T) {
	got := Parse(header(HeaderLocale, "fr", HeaderAcceptLanguage, "ru"))
	if got != "ru" {
		t.Fatalf("Parse = %q, want %q (unsupported X-Locale must not win)", got, "ru")
	}
}

func TestParse_RespectsQWeightOrder(t *testing.T) {
	got := Parse(header(HeaderAcceptLanguage, "en;q=0.5,ru;q=0.9"))
	if got != "ru" {
		t.Fatalf("Parse = %q, want %q (higher q must win regardless of header order)", got, "ru")
	}
}

func TestParse_DefaultsWhenNothingRecognized(t *testing.T) {
	got := Parse(header(HeaderAcceptLanguage, "fr-FR,de-DE"))
	if got != Default {
		t.Fatalf("Parse = %q, want Default %q", got, Default)
	}
	got = Parse(make(http.Header))
	if got != Default {
		t.Fatalf("Parse (no headers) = %q, want Default %q", got, Default)
	}
}

func TestServerInterceptor_PutsLocaleInContext(t *testing.T) {
	var gotLocale Locale
	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		gotLocale = FromContext(ctx)
		return nil, nil
	}

	wrapped := ServerInterceptor()(next)
	req := connect.NewRequest(&struct{}{})
	req.Header().Set(HeaderLocale, "en")
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotLocale != "en" {
		t.Fatalf("locale in context = %q, want %q", gotLocale, "en")
	}
}

// ServerInterceptor должен эхом вернуть фактически применённую локаль в
// ответе — иначе клиент не узнает, что запрошенная им локаль не
// поддерживалась и сервер молча взял Default (см. doc-комментарий).
func TestServerInterceptor_EchoesResolvedLocaleInResponseHeader(t *testing.T) {
	next := func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&struct{}{}), nil
	}

	wrapped := ServerInterceptor()(next)
	req := connect.NewRequest(&struct{}{})
	req.Header().Set(HeaderLocale, "fr") // неподдерживаемая -> должна откатиться на Default

	resp, err := wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := resp.Header().Get(HeaderLocale); got != string(Default) {
		t.Fatalf("echoed %s = %q, want the resolved Default %q", HeaderLocale, got, Default)
	}
}

func TestClientInterceptor_SetsHeaderFromContext(t *testing.T) {
	var gotHeader string
	next := func(_ context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		gotHeader = req.Header().Get(HeaderLocale)
		return nil, nil
	}

	wrapped := ClientInterceptor()(next)
	req := connect.NewRequest(&struct{}{})
	ctx := NewContext(context.Background(), "en")
	if _, err := wrapped(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotHeader != "en" {
		t.Fatalf("outgoing %s header = %q, want %q", HeaderLocale, gotHeader, "en")
	}
}
