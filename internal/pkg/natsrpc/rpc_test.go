package natsrpc

import (
	"strings"
	"testing"
)

func TestRPCSubject(t *testing.T) {
	const methodPath = "/webapi.identity.auth.v1.IdentityAuthService/InitializeSession"

	subject := RPCSubject("dev", "identity", "1.0.0", methodPath)
	prefix := RPCSubjectPrefix("dev", "identity", "1.0.0")

	if !strings.HasPrefix(subject, prefix+".") {
		t.Fatalf("subject %q does not start with prefix %q", subject, prefix)
	}

	// На стороне подписчика методPath восстанавливается обрезкой префикса.
	got := strings.TrimPrefix(subject, prefix+".")
	if got != methodPath {
		t.Fatalf("recovered method path = %q, want %q", got, methodPath)
	}

	if want := "dev.identity.1_0_0.rpc./webapi.identity.auth.v1.IdentityAuthService/InitializeSession"; subject != want {
		t.Fatalf("subject = %q, want %q", subject, want)
	}
}
