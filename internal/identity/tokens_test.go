package identity

import (
	"testing"
	"time"
)

func TestIssueAndParseAccessToken(t *testing.T) {
	cfg := AccessTokenConfig{
		Secret: "01234567890123456789012345678901", // 32 chars
		TTL:    15 * time.Minute,
	}

	token, expiresAt, err := IssueAccessToken(42, "user@example.com", []string{"client", "provider"}, cfg)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if token == "" {
		t.Fatal("token is empty")
	}
	if expiresAt.Before(time.Now().UTC()) {
		t.Fatal("expiresAt is in the past")
	}

	claims, err := ParseAccessToken(token, cfg.Secret)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.UserID != 42 {
		t.Errorf("user_id = %d, want 42", claims.UserID)
	}
	if claims.Email != "user@example.com" {
		t.Errorf("email = %q, want user@example.com", claims.Email)
	}
	if len(claims.Roles) != 2 || claims.Roles[0] != "client" || claims.Roles[1] != "provider" {
		t.Errorf("roles = %v", claims.Roles)
	}
	if claims.Subject != "42" {
		t.Errorf("subject = %q, want 42", claims.Subject)
	}
}

func TestParseAccessTokenWrongSecret(t *testing.T) {
	cfg := AccessTokenConfig{Secret: "01234567890123456789012345678901", TTL: 15 * time.Minute}
	token, _, err := IssueAccessToken(1, "a@b.c", []string{"client"}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	_, err = ParseAccessToken(token, "wrong-secret-0123456789012345678901234")
	if err == nil {
		t.Error("expected error for wrong secret")
	}
}

func TestParseAccessTokenExpired(t *testing.T) {
	cfg := AccessTokenConfig{Secret: "01234567890123456789012345678901", TTL: -1 * time.Second}
	token, _, err := IssueAccessToken(1, "a@b.c", []string{"client"}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	_, err = ParseAccessToken(token, cfg.Secret)
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestGenerateRefreshToken(t *testing.T) {
	plain, hash, err := GenerateRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if plain == "" || hash == "" {
		t.Fatal("empty token or hash")
	}
	if plain == hash {
		t.Fatal("plaintext equals hash")
	}
	if len(plain) != 64 { // 32 bytes hex
		t.Errorf("plaintext len = %d, want 64", len(plain))
	}

	// Two calls must differ.
	plain2, _, err := GenerateRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if plain == plain2 {
		t.Error("two refresh tokens should differ")
	}
}
