package identity

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type refreshLockBarrierStore struct {
	Store
	calls        atomic.Int32
	firstLocked  chan struct{}
	releaseFirst chan struct{}
}

func (s *refreshLockBarrierStore) FindRefreshTokenForUpdate(ctx context.Context, tx pgx.Tx, tokenHash string) (*RefreshRow, error) {
	row, err := s.Store.FindRefreshTokenForUpdate(ctx, tx, tokenHash)
	if s.calls.Add(1) == 1 {
		close(s.firstLocked)
		<-s.releaseFirst
	}
	return row, err
}

func TestRefreshConcurrentRotationDoesNotRevokeFamily(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping concurrent refresh integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	email := fmt.Sprintf("concurrent-refresh-%d@example.test", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, query := range []string{
			`DELETE FROM refresh_tokens USING users WHERE refresh_tokens.user_id = users.id AND users.email = $1`,
			`DELETE FROM user_roles USING users WHERE user_roles.user_id = users.id AND users.email = $1`,
			`DELETE FROM users WHERE email = $1`,
		} {
			if _, err := pool.Exec(context.Background(), query, email); err != nil {
				t.Errorf("cleanup with %s: %v", query, err)
			}
		}
	})

	store := NewPgxStore(pool)
	barrier := &refreshLockBarrierStore{
		Store:        store,
		firstLocked:  make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	service := NewService(Config{
		Store:             barrier,
		PasswordParams:    Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16},
		AccessTokenSecret: "01234567890123456789012345678901",
		AccessTokenTTL:    15 * time.Minute,
		RefreshTokenTTL:   time.Hour,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	firstPair, err := service.Register(ctx, email, "", "secret1234")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	type result struct {
		pair *TokenPair
		err  error
	}
	winnerCh := make(chan result, 1)
	go func() {
		pair, err := service.Refresh(ctx, firstPair.RefreshToken)
		winnerCh <- result{pair: pair, err: err}
	}()

	select {
	case <-barrier.firstLocked:
	case <-time.After(5 * time.Second):
		t.Fatal("first refresh did not acquire the token lock")
	}

	loserCh := make(chan result, 1)
	go func() {
		pair, err := service.Refresh(ctx, firstPair.RefreshToken)
		loserCh <- result{pair: pair, err: err}
	}()

	var loser result
	select {
	case loser = <-loserCh:
	case <-time.After(5 * time.Second):
		close(barrier.releaseFirst)
		t.Fatal("concurrent refresh did not return while the token lock was held")
	}
	close(barrier.releaseFirst)

	winner := <-winnerCh
	if winner.err != nil {
		t.Fatalf("winning refresh: %v", winner.err)
	}
	if loser.err != ErrRefreshInProgress {
		t.Fatalf("losing refresh error = %v, want ErrRefreshInProgress", loser.err)
	}
	if loser.pair != nil {
		t.Fatal("losing refresh unexpectedly returned tokens")
	}

	rotated, err := service.Refresh(ctx, winner.pair.RefreshToken)
	if err != nil {
		t.Fatalf("refresh with winning replacement token: %v", err)
	}
	if rotated.RefreshToken == "" {
		t.Fatal("rotation after concurrent refresh did not issue a replacement")
	}

	_, err = service.Refresh(ctx, firstPair.RefreshToken)
	if err != ErrTokenReplayDetected {
		t.Errorf("sequential replay after concurrent refresh: got %v, want ErrTokenReplayDetected", err)
	}
}
