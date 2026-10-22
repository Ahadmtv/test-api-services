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

	"github.com/jackc/pgx/v5/pgxpool"
)

var roleTestCounter atomic.Uint64

func roleTestEmail(prefix string) string {
	return fmt.Sprintf("roles-%s-%d-%d@example.test", prefix, time.Now().UnixNano()%1_000_000_000, roleTestCounter.Add(1))
}

func TestAdminSetRoleProviderProfileLifecycle(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping identity role/profile integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	store := NewPgxStore(pool)
	service := NewService(Config{
		Store:             store,
		PasswordParams:    Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16},
		AccessTokenSecret: "01234567890123456789012345678901",
		AccessTokenTTL:    15 * time.Minute,
		RefreshTokenTTL:   time.Hour,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	adminEmail := roleTestEmail("admin")
	targetEmail := roleTestEmail("provider")

	if _, err := service.Register(ctx, adminEmail, "", "secret1234"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	adminRecord, err := store.GetUserByEmail(ctx, adminEmail)
	if err != nil {
		t.Fatalf("load admin: %v", err)
	}
	targetUser, err := service.Register(ctx, targetEmail, "", "secret1234")
	if err != nil {
		t.Fatalf("register target: %v", err)
	}
	targetRecord, err := store.GetUserByEmail(ctx, targetEmail)
	if err != nil {
		t.Fatalf("load target: %v", err)
	}
	_ = targetUser
	cleanup := func(ids ...int64) {
		for _, id := range ids {
			for _, query := range []string{
				`DELETE FROM provider_profiles WHERE provider_id = $1`,
				`DELETE FROM refresh_tokens WHERE user_id = $1`,
				`DELETE FROM audit_events WHERE actor_user_id = $1 OR target_id = $1`,
				`DELETE FROM users WHERE id = $1`,
			} {
				if _, err := pool.Exec(ctx, query, id); err != nil {
					t.Fatalf("cleanup %d with %s: %v", id, query, err)
				}
			}
		}
	}
	defer cleanup(adminRecord.ID, targetRecord.ID)

	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin')`, adminRecord.ID); err != nil {
		t.Fatalf("grant admin role to actor: %v", err)
	}

	countProfiles := func(userID int64) int {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_profiles WHERE provider_id = $1`, userID).Scan(&count); err != nil {
			t.Fatalf("count profiles: %v", err)
		}
		return count
	}

	firstGrant, err := service.AdminSetRole(ctx, adminRecord.ID, targetRecord.ID, "provider", true, "req-grant-1")
	if err != nil {
		t.Fatalf("grant provider role: %v", err)
	}
	if !containsRole(firstGrant.Roles, "provider") {
		t.Fatalf("granted roles=%+v, want provider inside", firstGrant.Roles)
	}
	if countProfiles(targetRecord.ID) != 1 {
		t.Fatalf("profile count after first grant = %d, want 1", countProfiles(targetRecord.ID))
	}
	var (
		displayName string
		timeZone    string
		isActive    bool
		status      string
	)
	if err := pool.QueryRow(ctx, `SELECT display_name, time_zone, is_active, verification_status FROM provider_profiles WHERE provider_id = $1`, targetRecord.ID).Scan(&displayName, &timeZone, &isActive, &status); err != nil {
		t.Fatalf("load created profile: %v", err)
	}
	if displayName != "" || timeZone != "Asia/Tehran" || isActive || status != "pending" {
		t.Fatalf("created profile=(%q,%q,%v,%q), want ('','Asia/Tehran',false,pending)", displayName, timeZone, isActive, status)
	}

	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE target_id = $1 AND action = 'provider.profile.created'`, targetRecord.ID).Scan(&auditCount); err != nil {
		t.Fatalf("count profile audit events: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("profile creation audit events = %d, want 1", auditCount)
	}

	if _, err := pool.Exec(ctx, `UPDATE provider_profiles SET display_name = ' salon independently ' WHERE provider_id = $1`, targetRecord.ID); err != nil {
		t.Fatalf("complete profile: %v", err)
	}

	reGrant, err := service.AdminSetRole(ctx, adminRecord.ID, targetRecord.ID, "provider", true, "req-grant-2")
	if err != nil {
		t.Fatalf("re-grant provider role: %v", err)
	}
	if !containsRole(reGrant.Roles, "provider") {
		t.Fatalf("re-granted roles=%+v", reGrant.Roles)
	}
	if countProfiles(targetRecord.ID) != 1 {
		t.Fatalf("profile count after re-grant = %d, want 1 (idempotent)", countProfiles(targetRecord.ID))
	}
	var completedName string
	if err := pool.QueryRow(ctx, `SELECT display_name FROM provider_profiles WHERE provider_id = $1`, targetRecord.ID).Scan(&completedName); err != nil {
		t.Fatalf("load profile after re-grant: %v", err)
	}
	if completedName != " salon independently " {
		t.Fatalf("re-grant must not overwrite profile data, display_name=%q", completedName)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE target_id = $1 AND action = 'provider.profile.created'`, targetRecord.ID).Scan(&auditCount); err != nil {
		t.Fatalf("count audit events after re-grant: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("profile audit events after re-grant = %d, want 1", auditCount)
	}

	revoked, err := service.AdminSetRole(ctx, adminRecord.ID, targetRecord.ID, "provider", false, "req-revoke-1")
	if err != nil {
		t.Fatalf("revoke provider role: %v", err)
	}
	if containsRole(revoked.Roles, "provider") {
		t.Fatalf("revoked roles=%+v, want provider removed", revoked.Roles)
	}
	if countProfiles(targetRecord.ID) != 1 {
		t.Fatalf("profile count after revoke = %d, want 1 (revoke keeps profile)", countProfiles(targetRecord.ID))
	}

	if _, err := service.AdminSetRole(ctx, adminRecord.ID, targetRecord.ID, "provider", true, "req-grant-3"); err != nil {
		t.Fatalf("granted provider role after revoke: %v", err)
	}
	if countProfiles(targetRecord.ID) != 1 {
		t.Fatalf("profile count after grant-revoke-grant = %d, want 1", countProfiles(targetRecord.ID))
	}
	if err := pool.QueryRow(ctx, `SELECT display_name FROM provider_profiles WHERE provider_id = $1`, targetRecord.ID).Scan(&completedName); err != nil {
		t.Fatalf("load profile after final grant: %v", err)
	}
	if completedName != " salon independently " {
		t.Fatalf("final grant must not reset profile data, display_name=%q", completedName)
	}
}
