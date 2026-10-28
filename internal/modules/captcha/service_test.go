package captcha

import (
	"context"
	"sync"
	"testing"
	"time"

	"dmatech.ru/internal/pkg/pow"
)

// fakeStore — challengeStore в памяти для тестов, без реального NATS.
type fakeStore struct {
	mu         sync.Mutex
	difficulty map[string]int32
}

func newFakeStore() *fakeStore {
	return &fakeStore{difficulty: map[string]int32{}}
}

func (f *fakeStore) put(_ context.Context, challenge string, difficulty int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.difficulty[challenge] = difficulty
	return nil
}

func (f *fakeStore) take(_ context.Context, challenge string) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.difficulty[challenge]
	if !ok {
		return 0, ErrChallengeNotFound
	}
	delete(f.difficulty, challenge)
	return d, nil
}

func TestIssueChallenge_UsesDefaultDifficultyWhenNotRequested(t *testing.T) {
	svc := NewService(newFakeStore(), 12, time.Minute)

	challenge, difficulty, expiresAt, err := svc.IssueChallenge(context.Background(), 0)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	if challenge == "" {
		t.Fatal("expected a non-empty challenge")
	}
	if difficulty != 12 {
		t.Fatalf("difficulty = %d, want default 12", difficulty)
	}
	if !expiresAt.After(time.Now()) {
		t.Fatal("expiresAt must be in the future")
	}
}

func TestIssueChallenge_HonoursRequestedDifficulty(t *testing.T) {
	svc := NewService(newFakeStore(), 12, time.Minute)

	_, difficulty, _, err := svc.IssueChallenge(context.Background(), 20)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	if difficulty != 20 {
		t.Fatalf("difficulty = %d, want requested 20", difficulty)
	}
}

func TestVerifyChallenge_AcceptsCorrectSolution(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newFakeStore(), 12, time.Minute)

	challenge, difficulty, _, err := svc.IssueChallenge(ctx, 0)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	nonce, ok := pow.Solve(challenge, int(difficulty), 1_000_000)
	if !ok {
		t.Fatal("expected to find a solution within the attempt budget")
	}

	valid, err := svc.VerifyChallenge(ctx, challenge, nonce)
	if err != nil {
		t.Fatalf("VerifyChallenge: %v", err)
	}
	if !valid {
		t.Fatal("expected the correct solution to be accepted")
	}
}

func TestVerifyChallenge_RejectsWrongSolution(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newFakeStore(), 12, time.Minute)

	challenge, _, _, err := svc.IssueChallenge(ctx, 0)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}

	valid, err := svc.VerifyChallenge(ctx, challenge, "definitely-not-a-solution")
	if err != nil {
		t.Fatalf("VerifyChallenge: %v", err)
	}
	if valid {
		t.Fatal("expected a wrong solution to be rejected")
	}
}

func TestVerifyChallenge_OneShot(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newFakeStore(), 12, time.Minute)

	challenge, difficulty, _, err := svc.IssueChallenge(ctx, 0)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}
	nonce, ok := pow.Solve(challenge, int(difficulty), 1_000_000)
	if !ok {
		t.Fatal("expected to find a solution within the attempt budget")
	}

	first, err := svc.VerifyChallenge(ctx, challenge, nonce)
	if err != nil || !first {
		t.Fatalf("first verify: valid=%v err=%v, want true, nil", first, err)
	}

	second, err := svc.VerifyChallenge(ctx, challenge, nonce)
	if err != nil {
		t.Fatalf("second verify: %v", err)
	}
	if second {
		t.Fatal("the same challenge/nonce must not verify twice")
	}
}

func TestVerifyChallenge_UnknownChallengeIsRejectedNotErrored(t *testing.T) {
	svc := NewService(newFakeStore(), 12, time.Minute)

	valid, err := svc.VerifyChallenge(context.Background(), "never-issued", "whatever")
	if err != nil {
		t.Fatalf("expected a plain rejection, not an error: %v", err)
	}
	if valid {
		t.Fatal("an unknown challenge must never verify")
	}
}
