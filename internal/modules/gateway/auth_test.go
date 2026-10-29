package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"dmatech.ru/internal/pkg/authctx"
	optionsv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/options"
	"dmatech.ru/internal/pkg/natsdiscovery"
	"dmatech.ru/internal/pkg/natsrpc"
)

var testAuthSecret = []byte("test-jwt-signing-secret-32-bytes!!")

// descriptorWithMethods собирает синтетический FileDescriptorSet с одним
// сервисом и указанными методами (имя -> public) — без protoc/buf, только
// для проверки того, что gateway правильно читает
// (webapi.options.v1.public) из дескриптора (тот же приём, что
// buildDescriptorSet в server_test.go, но с методами, не только сервисами).
func descriptorWithMethods(t *testing.T, pkg, service string, methods map[string]bool) []byte {
	t.Helper()
	var methodProtos []*descriptorpb.MethodDescriptorProto
	for name, public := range methods {
		m := &descriptorpb.MethodDescriptorProto{Name: proto.String(name)}
		if public {
			opts := &descriptorpb.MethodOptions{}
			proto.SetExtension(opts, optionsv1.E_Public, true)
			m.Options = opts
		}
		methodProtos = append(methodProtos, m)
	}
	set := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Package: proto.String(pkg),
				Service: []*descriptorpb.ServiceDescriptorProto{
					{Name: proto.String(service), Method: methodProtos},
				},
			},
		},
	}
	data, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	return data
}

func TestExtractServiceInfo_ReadsPublicMethodOption(t *testing.T) {
	descriptor := descriptorWithMethods(t, "pkg", "Svc", map[string]bool{
		"Public":    true,
		"Protected": false,
	})
	procedures, public, err := extractServiceInfo(descriptor)
	if err != nil {
		t.Fatalf("extractServiceInfo: %v", err)
	}
	if len(procedures) != 1 || procedures[0] != "pkg.Svc" {
		t.Fatalf("procedures = %v, want [pkg.Svc]", procedures)
	}
	if !public["/pkg.Svc/Public"] {
		t.Fatal("expected /pkg.Svc/Public to be marked public")
	}
	if public["/pkg.Svc/Protected"] {
		t.Fatal("expected /pkg.Svc/Protected to not be marked public")
	}
}

func withAccessTokenCookie(req *http.Request, token string) *http.Request {
	req.AddCookie(&http.Cookie{Name: authctx.CookieAccessToken, Value: token})
	return req
}

func TestHandleProxy_ProtectedMethodRejectsWithoutToken(t *testing.T) {
	nc := connectTestNATS(t)

	verifier := authctx.NewVerifier(testAuthSecret)
	s, routes := testServerWithVerifier(HttpConfig{BaseUrl: "/api", ProxyRequestTimeout: time.Second}, nc, verifier)
	routes.Apply(natsdiscovery.RouteUpdate{
		ServiceName: "identity", Version: "1_0_0",
		Descriptor: descriptorWithMethods(t, "pkg", "Svc", map[string]bool{"Protected": false}),
	})

	// Намеренно без подписчика на subject: если gateway по ошибке всё же
	// проксирует запрос, это дало бы 503 (no responders), а не 401 — тест
	// различает эти два случая по коду, не только по факту ошибки.
	rec := serve(s, http.MethodPost, "/api/pkg.Svc/Protected", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no access_token cookie at all)", rec.Code)
	}
}

func TestHandleProxy_ProtectedMethodRejectsInvalidToken(t *testing.T) {
	nc := connectTestNATS(t)

	verifier := authctx.NewVerifier(testAuthSecret)
	s, routes := testServerWithVerifier(HttpConfig{BaseUrl: "/api", ProxyRequestTimeout: time.Second}, nc, verifier)
	routes.Apply(natsdiscovery.RouteUpdate{
		ServiceName: "identity", Version: "1_0_0",
		Descriptor: descriptorWithMethods(t, "pkg", "Svc", map[string]bool{"Protected": false}),
	})

	req := withAccessTokenCookie(httptest.NewRequest(http.MethodPost, "/api/pkg.Svc/Protected", nil), "not-a-valid-jwt")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (garbage access_token cookie)", rec.Code)
	}
}

func TestHandleProxy_ProtectedMethodAcceptsValidTokenAndForwardsClaims(t *testing.T) {
	nc := connectTestNATS(t)

	issuer := authctx.NewIssuer(testAuthSecret, time.Hour)
	verifier := authctx.NewVerifier(testAuthSecret)
	token, _, err := issuer.Issue(authctx.Claims{UserID: "user-1", Permissions: "AQ==", SessionID: "session-1"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	s, routes := testServerWithVerifier(HttpConfig{BaseUrl: "/api", ProxyRequestTimeout: time.Second}, nc, verifier)
	routes.Apply(natsdiscovery.RouteUpdate{
		ServiceName: "identity", Version: "1_0_0",
		Descriptor: descriptorWithMethods(t, "pkg", "Svc", map[string]bool{"Protected": false}),
	})

	const methodPath = "/pkg.Svc/Protected"
	subject := natsrpc.RPCSubject(testEnv, "identity", "1_0_0", methodPath)

	var gotUserID, gotPerms, gotSessionID string
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		gotUserID = msg.Header.Get(authctx.HeaderUserID)
		gotPerms = msg.Header.Get(authctx.HeaderPermissions)
		gotSessionID = msg.Header.Get(authctx.HeaderSessionID)
		reply := nats.NewMsg(msg.Reply)
		reply.Header.Set(natsrpc.RPCStatusHeader, "200")
		_ = msg.RespondMsg(reply)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	req := withAccessTokenCookie(httptest.NewRequest(http.MethodPost, "/api"+methodPath, nil), token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%q", rec.Code, rec.Body.String())
	}
	if gotUserID != "user-1" || gotPerms != "AQ==" || gotSessionID != "session-1" {
		t.Fatalf("forwarded claims = (%q, %q, %q), want (user-1, AQ==, session-1)", gotUserID, gotPerms, gotSessionID)
	}
}

// Даже у публичного метода клиент не должен мочь подставить доверенные
// заголовки сам — handleProxy обязан их стереть независимо от того,
// требуется ли токен (см. doc-комментарий там).
func TestHandleProxy_StripsClientSuppliedTrustedHeadersOnPublicMethod(t *testing.T) {
	nc := connectTestNATS(t)

	s, routes := testServerWithVerifier(HttpConfig{BaseUrl: "/api", ProxyRequestTimeout: time.Second}, nc, nil)
	routes.Apply(natsdiscovery.RouteUpdate{
		ServiceName: "identity", Version: "1_0_0",
		Descriptor: descriptorWithMethods(t, "pkg", "Svc", map[string]bool{"Public": true}),
	})

	const methodPath = "/pkg.Svc/Public"
	subject := natsrpc.RPCSubject(testEnv, "identity", "1_0_0", methodPath)

	var gotUserID string
	sawHeader := false
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		gotUserID, sawHeader = msg.Header.Get(authctx.HeaderUserID), msg.Header.Get(authctx.HeaderUserID) != ""
		reply := nats.NewMsg(msg.Reply)
		reply.Header.Set(natsrpc.RPCStatusHeader, "200")
		_ = msg.RespondMsg(reply)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	req := httptest.NewRequest(http.MethodPost, "/api"+methodPath, nil)
	req.Header.Set(authctx.HeaderUserID, "attacker-supplied-user-id")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%q", rec.Code, rec.Body.String())
	}
	if sawHeader {
		t.Fatalf("forwarded %s = %q, want it stripped (client must not be able to set it directly)", authctx.HeaderUserID, gotUserID)
	}
}
