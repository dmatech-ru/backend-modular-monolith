package pow

import "testing"

func TestNewChallenge_ReturnsDistinctValues(t *testing.T) {
	c1, err := NewChallenge()
	if err != nil {
		t.Fatalf("NewChallenge: %v", err)
	}
	c2, err := NewChallenge()
	if err != nil {
		t.Fatalf("NewChallenge: %v", err)
	}
	if c1 == c2 {
		t.Fatal("two challenges must not be equal")
	}
	if len(c1) == 0 {
		t.Fatal("challenge must not be empty")
	}
}

func TestVerify_ZeroDifficultyAlwaysPasses(t *testing.T) {
	if !Verify("anything", "whatever-nonce", 0) {
		t.Fatal("difficulty <= 0 must always pass")
	}
	if !Verify("anything", "whatever-nonce", -1) {
		t.Fatal("negative difficulty must always pass")
	}
}

func TestVerify_ImpossibleDifficultyAlwaysFails(t *testing.T) {
	if Verify("anything", "whatever-nonce", 1000) {
		t.Fatal("difficulty beyond hash size must never pass")
	}
}

func TestSolveThenVerify_RoundTrip(t *testing.T) {
	challenge, err := NewChallenge()
	if err != nil {
		t.Fatalf("NewChallenge: %v", err)
	}
	const difficulty = 12 // низкая, но не нулевая — чтобы тест не был тавтологией

	nonce, ok := Solve(challenge, difficulty, 1_000_000)
	if !ok {
		t.Fatal("expected Solve to find a nonce within the attempt budget")
	}
	if !Verify(challenge, nonce, difficulty) {
		t.Fatal("the solved nonce must verify against the same challenge/difficulty")
	}
}

func TestVerify_RejectsWrongChallengeOrNonce(t *testing.T) {
	challenge, err := NewChallenge()
	if err != nil {
		t.Fatalf("NewChallenge: %v", err)
	}
	const difficulty = 12

	nonce, ok := Solve(challenge, difficulty, 1_000_000)
	if !ok {
		t.Fatal("expected a solution within the attempt budget")
	}

	otherChallenge, err := NewChallenge()
	if err != nil {
		t.Fatalf("NewChallenge: %v", err)
	}
	if Verify(otherChallenge, nonce, difficulty) {
		t.Fatal("a solution must not verify against a different challenge")
	}
	if Verify(challenge, nonce+"x", difficulty) {
		t.Fatal("a tampered nonce must not verify")
	}
}

func TestSolve_GivesUpWithinBudget(t *testing.T) {
	challenge, err := NewChallenge()
	if err != nil {
		t.Fatalf("NewChallenge: %v", err)
	}
	if _, ok := Solve(challenge, 20, 3); ok {
		t.Fatal("expected Solve to fail with an unreasonably small attempt budget (astronomically unlikely to succeed in 3 tries)")
	}
}

func TestVerify_DeterministicForSameInputs(t *testing.T) {
	if Verify("c", "n", 8) != Verify("c", "n", 8) {
		t.Fatal("Verify must be deterministic for the same inputs")
	}
}
