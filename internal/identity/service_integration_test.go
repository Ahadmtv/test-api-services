package identity

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestServiceIntegration(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping identity service integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	email := "integration@example.com"
	for _, query := range []string{
		`DELETE FROM refresh_tokens USING users WHERE refresh_tokens.user_id = users.id AND users.email = $1`,
		`DELETE FROM user_roles USING users WHERE user_roles.user_id = users.id AND users.email = $1`,
		`DELETE FROM users WHERE email = $1`,
	} {
		if _, err := pool.Exec(ctx, query, email); err != nil {
			t.Fatalf("clean fixture state: %v", err)
		}
	}

	store := NewPgxStore(pool)
	service := NewService(Config{
		Store:             store,
		PasswordParams:    Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16},
		AccessTokenSecret: "01234567890123456789012345678901",
		AccessTokenTTL:    15 * time.Minute,
		RefreshTokenTTL:   time.Hour,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// Register
	pair, err := service.Register(ctx, email, "+989120000000", "secret1234")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("registration should issue tokens")
	}

	// Login
	loginPair, err := service.Login(ctx, email, "secret1234")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if loginPair.AccessToken == "" {
		t.Fatal("login should issue access token")
	}

	// Wrong password
	_, err = service.Login(ctx, email, "wrongpassword")
	if err != ErrInvalidCredentials {
		t.Errorf("wrong password: got %v, want ErrInvalidCredentials", err)
	}

	// Me via access token
	claims, err := ParseAccessToken(loginPair.AccessToken, "01234567890123456789012345678901")
	if err != nil {
		t.Fatalf("parse access token: %v", err)
	}
	user, err := service.Me(ctx, claims.UserID)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if user.Email != email {
		t.Errorf("me email = %q, want %q", user.Email, email)
	}
	foundClient := false
	for _, r := range user.Roles {
		if r == "client" {
			foundClient = true
		}
	}
	if !foundClient {
		t.Error("expected client role")
	}

	// Refresh
	refreshPair, err := service.Refresh(ctx, loginPair.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshPair.AccessToken == "" || refreshPair.RefreshToken == "" {
		t.Fatal("refresh should issue new pair")
	}
	if refreshPair.RefreshToken == loginPair.RefreshToken {
		t.Error("refresh token should rotate")
	}
	rotatedClaims, err := ParseAccessToken(refreshPair.AccessToken, "01234567890123456789012345678901")
	if err != nil {
		t.Fatalf("parse rotated access token: %v", err)
	}
	if rotatedClaims.Email != email || len(rotatedClaims.Roles) == 0 {
		t.Errorf("rotated access token lost identity: email=%q roles=%v", rotatedClaims.Email, rotatedClaims.Roles)
	}

	// Replaying the rotated token is detected and revokes the whole family.
	_, err = service.Refresh(ctx, loginPair.RefreshToken)
	if err != ErrTokenReplayDetected {
		t.Errorf("reused refresh token: got %v, want ErrTokenReplayDetected", err)
	}
	_, err = service.Refresh(ctx, refreshPair.RefreshToken)
	if err != ErrInvalidCredentials {
		t.Errorf("family after replay: got %v, want ErrInvalidCredentials", err)
	}

	// Logout is idempotent for an already-revoked token.
	if err := service.Logout(ctx, refreshPair.RefreshToken); err != nil {
		t.Fatalf("logout: %v", err)
	}
	_, err = service.Refresh(ctx, refreshPair.RefreshToken)
	if err != ErrInvalidCredentials {
		t.Errorf("revoked refresh: got %v, want ErrInvalidCredentials", err)
	}
}
