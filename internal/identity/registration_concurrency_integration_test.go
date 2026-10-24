package identity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type createUserBarrierStore struct {
	Store
	ready chan<- struct{}
	start <-chan struct{}
}

func (s *createUserBarrierStore) CreateUser(ctx context.Context, tx pgx.Tx, email string, phone *string, passwordHash string) (int64, error) {
	s.ready <- struct{}{}
	<-s.start
	return s.Store.CreateUser(ctx, tx, email, phone, passwordHash)
}

func TestRegisterConcurrentDuplicateReturnsUserExists(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping concurrent registration integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	email := fmt.Sprintf("concurrent-register-%d@example.test", time.Now().UnixNano())
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
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	service := NewService(Config{
		Store: &createUserBarrierStore{
			Store: store,
			ready: ready,
			start: start,
		},
		PasswordParams:    Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16},
		AccessTokenSecret: "01234567890123456789012345678901",
		AccessTokenTTL:    15 * time.Minute,
		RefreshTokenTTL:   time.Hour,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	type result struct {
		pair *TokenPair
		err  error
	}
	results := make([]result, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	for i := range results {
		go func() {
			defer workers.Done()
			results[i].pair, results[i].err = service.Register(ctx, email, "", "secret1234")
		}()
	}

	for range results {
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent registrations did not reach the insert barrier")
		}
	}
	close(start)
	workers.Wait()

	successes := 0
	for i, result := range results {
		if result.err == nil {
			successes++
			continue
		}
		if !errors.Is(result.err, ErrUserAlreadyExists) {
			t.Errorf("registration %d error = %v, want ErrUserAlreadyExists", i, result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful registrations = %d, want exactly 1", successes)
	}
	if results[0].pair != nil && results[0].pair.RefreshToken == "" {
		t.Error("successful registration did not issue a refresh token")
	}
	if results[1].pair != nil && results[1].pair.RefreshToken == "" {
		t.Error("successful registration did not issue a refresh token")
	}
}
