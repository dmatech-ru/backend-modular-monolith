package srp

import (
	"bytes"
	"errors"
	"testing"
)

const (
	testIdentifier = "alice@example.com"
	testPassword   = "correct horse battery staple"
)

func registerAndChallenge(t *testing.T) (salt, verifier, serverSecret, serverPublic []byte) {
	t.Helper()

	salt, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	verifier = ComputeVerifier(testIdentifier, testPassword, salt)

	serverSecret, serverPublic, err = NewServerChallenge(verifier)
	if err != nil {
		t.Fatalf("NewServerChallenge: %v", err)
	}
	return salt, verifier, serverSecret, serverPublic
}

func TestRoundTrip_Success(t *testing.T) {
	salt, verifier, serverSecret, serverPublic := registerAndChallenge(t)

	clientSecret, clientPublic, err := NewClientEphemeral()
	if err != nil {
		t.Fatalf("NewClientEphemeral: %v", err)
	}

	clientProof, sessionKey, err := ComputeClientProof(testIdentifier, testPassword, salt, clientSecret, clientPublic, serverPublic)
	if err != nil {
		t.Fatalf("ComputeClientProof: %v", err)
	}

	serverProof, err := VerifyClientProof(verifier, serverSecret, serverPublic, clientPublic, clientProof)
	if err != nil {
		t.Fatalf("VerifyClientProof: %v", err)
	}

	if err := VerifyServerProof(serverProof, clientPublic, clientProof, sessionKey); err != nil {
		t.Fatalf("VerifyServerProof: %v", err)
	}
}

func TestRoundTrip_WrongPasswordIsRejected(t *testing.T) {
	salt, verifier, serverSecret, serverPublic := registerAndChallenge(t)

	clientSecret, clientPublic, err := NewClientEphemeral()
	if err != nil {
		t.Fatalf("NewClientEphemeral: %v", err)
	}

	clientProof, _, err := ComputeClientProof(testIdentifier, "wrong password", salt, clientSecret, clientPublic, serverPublic)
	if err != nil {
		t.Fatalf("ComputeClientProof: %v", err)
	}

	if _, err := VerifyClientProof(verifier, serverSecret, serverPublic, clientPublic, clientProof); !errors.Is(err, ErrProofMismatch) {
		t.Fatalf("VerifyClientProof error = %v, want ErrProofMismatch", err)
	}
}

func TestRoundTrip_TamperedProofIsRejected(t *testing.T) {
	salt, verifier, serverSecret, serverPublic := registerAndChallenge(t)

	clientSecret, clientPublic, err := NewClientEphemeral()
	if err != nil {
		t.Fatalf("NewClientEphemeral: %v", err)
	}
	clientProof, _, err := ComputeClientProof(testIdentifier, testPassword, salt, clientSecret, clientPublic, serverPublic)
	if err != nil {
		t.Fatalf("ComputeClientProof: %v", err)
	}
	clientProof[0] ^= 0xFF // портим доказательство

	if _, err := VerifyClientProof(verifier, serverSecret, serverPublic, clientPublic, clientProof); !errors.Is(err, ErrProofMismatch) {
		t.Fatalf("VerifyClientProof error = %v, want ErrProofMismatch", err)
	}
}

func TestRoundTrip_FakeServerProofIsRejected(t *testing.T) {
	salt, verifier, serverSecret, serverPublic := registerAndChallenge(t)

	clientSecret, clientPublic, err := NewClientEphemeral()
	if err != nil {
		t.Fatalf("NewClientEphemeral: %v", err)
	}
	clientProof, sessionKey, err := ComputeClientProof(testIdentifier, testPassword, salt, clientSecret, clientPublic, serverPublic)
	if err != nil {
		t.Fatalf("ComputeClientProof: %v", err)
	}
	if _, err := VerifyClientProof(verifier, serverSecret, serverPublic, clientPublic, clientProof); err != nil {
		t.Fatalf("VerifyClientProof: %v", err)
	}

	fakeServerProof := bytes.Repeat([]byte{0x42}, 32)
	if err := VerifyServerProof(fakeServerProof, clientPublic, clientProof, sessionKey); !errors.Is(err, ErrProofMismatch) {
		t.Fatalf("VerifyServerProof error = %v, want ErrProofMismatch", err)
	}
}

func TestVerifyClientProof_RejectsZeroClientPublic(t *testing.T) {
	_, verifier, serverSecret, serverPublic := registerAndChallenge(t)

	zero := make([]byte, nLen)
	if _, err := VerifyClientProof(verifier, serverSecret, serverPublic, zero, zero); !errors.Is(err, ErrInvalidPublicValue) {
		t.Fatalf("VerifyClientProof error = %v, want ErrInvalidPublicValue", err)
	}
}

func TestVerifyClientProof_RejectsOversizedInput(t *testing.T) {
	_, verifier, serverSecret, serverPublic := registerAndChallenge(t)

	huge := make([]byte, nLen+1)
	if _, err := VerifyClientProof(verifier, serverSecret, serverPublic, huge, huge); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("VerifyClientProof error = %v, want ErrValueTooLarge", err)
	}
}

func TestComputeClientProof_RejectsZeroServerPublic(t *testing.T) {
	salt, _, _, _ := registerAndChallenge(t)
	clientSecret, clientPublic, err := NewClientEphemeral()
	if err != nil {
		t.Fatalf("NewClientEphemeral: %v", err)
	}

	zero := make([]byte, nLen)
	if _, _, err := ComputeClientProof(testIdentifier, testPassword, salt, clientSecret, clientPublic, zero); !errors.Is(err, ErrInvalidPublicValue) {
		t.Fatalf("ComputeClientProof error = %v, want ErrInvalidPublicValue", err)
	}
}

func TestDifferentIdentifiersYieldDifferentVerifiers(t *testing.T) {
	salt, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	v1 := ComputeVerifier("alice@example.com", testPassword, salt)
	v2 := ComputeVerifier("bob@example.com", testPassword, salt)
	if bytes.Equal(v1, v2) {
		t.Fatal("verifiers for different identifiers must differ")
	}
}

func TestEachChallengeUsesFreshEphemeralValues(t *testing.T) {
	_, verifier, _, _ := registerAndChallenge(t)

	_, b1, err := NewServerChallenge(verifier)
	if err != nil {
		t.Fatalf("NewServerChallenge: %v", err)
	}
	_, b2, err := NewServerChallenge(verifier)
	if err != nil {
		t.Fatalf("NewServerChallenge: %v", err)
	}
	if bytes.Equal(b1, b2) {
		t.Fatal("two challenges for the same verifier produced the same server public value")
	}
}

func TestFakeCredentials_DeterministicForSameIdentifier(t *testing.T) {
	key := []byte("test-anti-enumeration-key-32-bytes!")

	salt1, verifier1 := FakeCredentials(key, "unknown@example.com")
	salt2, verifier2 := FakeCredentials(key, "unknown@example.com")

	if !bytes.Equal(salt1, salt2) {
		t.Fatal("fake salt must be stable across calls for the same identifier")
	}
	if !bytes.Equal(verifier1, verifier2) {
		t.Fatal("fake verifier must be stable across calls for the same identifier")
	}
}

func TestFakeCredentials_DiffersByIdentifierAndKey(t *testing.T) {
	key := []byte("test-anti-enumeration-key-32-bytes!")
	otherKey := []byte("a-completely-different-32-byte-key!")

	salt1, verifier1 := FakeCredentials(key, "alice@example.com")
	salt2, verifier2 := FakeCredentials(key, "bob@example.com")
	if bytes.Equal(salt1, salt2) {
		t.Fatal("fake salt must differ between identifiers")
	}
	if bytes.Equal(verifier1, verifier2) {
		t.Fatal("fake verifier must differ between identifiers")
	}

	salt3, verifier3 := FakeCredentials(otherKey, "alice@example.com")
	if bytes.Equal(salt1, salt3) || bytes.Equal(verifier1, verifier3) {
		t.Fatal("fake credentials must differ between keys for the same identifier")
	}
}

func TestFakeCredentials_LooksLikeARealChallenge(t *testing.T) {
	key := []byte("test-anti-enumeration-key-32-bytes!")
	salt, verifier := FakeCredentials(key, "unknown@example.com")

	if len(salt) != 16 {
		t.Fatalf("fake salt length = %d, want 16 (same as GenerateSalt)", len(salt))
	}
	if !ValidVerifier(verifier) {
		t.Fatal("fake verifier must pass the same validity check as a real one")
	}

	// Challenge по фейковому verifier строится так же, как по настоящему, и
	// ни один клиент не сможет подобрать proof под него.
	serverSecret, serverPublic, err := NewServerChallenge(verifier)
	if err != nil {
		t.Fatalf("NewServerChallenge on fake verifier: %v", err)
	}

	clientSecret, clientPublic, err := NewClientEphemeral()
	if err != nil {
		t.Fatalf("NewClientEphemeral: %v", err)
	}
	proof, _, err := ComputeClientProof("unknown@example.com", "any password at all",
		salt, clientSecret, clientPublic, serverPublic)
	if err != nil {
		t.Fatalf("ComputeClientProof: %v", err)
	}

	if _, err := VerifyClientProof(verifier, serverSecret, serverPublic, clientPublic, proof); !errors.Is(err, ErrProofMismatch) {
		t.Fatalf("VerifyClientProof against a fake verifier = %v, want ErrProofMismatch (no password can ever match)", err)
	}
}
