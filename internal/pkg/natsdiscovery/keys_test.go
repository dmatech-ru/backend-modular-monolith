package natsdiscovery

import "testing"

func TestSanitizeToken(t *testing.T) {
	cases := map[string]string{
		"1.0.0":            "1_0_0",
		"1.0.0-rc.1":       "1_0_0-rc_1",
		"identity-service": "identity-service",
		" dev ":            "dev",
		"a*b>c d":          "a_b_c_d",
		"host.example.com": "host_example_com",
	}
	for in, want := range cases {
		if got := sanitizeToken(in); got != want {
			t.Errorf("sanitizeToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRouteKey(t *testing.T) {
	env, svc, ver, ok := parseRouteKey(routeKey("dev", "identity", "1_0_0"))
	if !ok || env != "dev" || svc != "identity" || ver != "1_0_0" {
		t.Fatalf("unexpected parse result: %q %q %q %v", env, svc, ver, ok)
	}

	for _, key := range []string{"dev.identity", "dev.identity.1_0_0.extra", ""} {
		if _, _, _, ok := parseRouteKey(key); ok {
			t.Errorf("parseRouteKey(%q) should fail", key)
		}
	}
}

func TestRequireTokens(t *testing.T) {
	if err := requireTokens(map[string]string{"env": "dev", "version": "1.0.0"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := requireTokens(map[string]string{"env": " ", "version": "1.0.0"}); err == nil {
		t.Fatal("expected error for empty env")
	}
}
