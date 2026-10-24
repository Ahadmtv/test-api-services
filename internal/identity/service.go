package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"test-api/internal/platform"
)

// Service performs authentication operations.
type Service struct {
	store             Store
	passwordParams    Params
	accessTokenSecret string
	accessTokenTTL    time.Duration
	refreshTokenTTL   time.Duration
	logger            *slog.Logger
}

// Config for the identity service.
type Config struct {
	Store             Store
	PasswordParams    Params
	AccessTokenSecret string
	AccessTokenTTL    time.Duration
	RefreshTokenTTL   time.Duration
	Logger            *slog.Logger
}

// NewService builds an identity service.
func NewService(cfg Config) *Service {
	return &Service{
		store:             cfg.Store,
		passwordParams:    cfg.PasswordParams,
		accessTokenSecret: cfg.AccessTokenSecret,
		accessTokenTTL:    cfg.AccessTokenTTL,
		refreshTokenTTL:   cfg.RefreshTokenTTL,
		logger:            cfg.Logger,
	}
}

// Service errors.
var (
	ErrUserNotFound         = errors.New("user not found")
	ErrInvalidCredentials   = errors.New("invalid credentials")
	ErrUserAlreadyExists    = errors.New("user already exists")
	ErrRefreshTokenNotFound = errors.New("refresh token not found")
	ErrRefreshTokenExpired  = errors.New("refresh token expired")
	ErrRefreshTokenRevoked  = errors.New("refresh token revoked")
	ErrTokenReplayDetected  = errors.New("token replay detected; all sessions revoked")
	ErrRefreshInProgress    = errors.New("refresh token rotation in progress")
	ErrInvalidInput         = errors.New("invalid input")
	ErrUserInactive         = errors.New("user inactive")
	ErrInvalidRole          = errors.New("invalid role")
	ErrSelfRoleChange       = errors.New("cannot change own roles")
	ErrLastAdmin            = errors.New("cannot remove the last active admin")
)

// AdminUserPage is a cursor page of administrative user records.
type AdminUserPage struct {
	Items      []AdminUser `json:"items"`
	NextCursor *int64      `json:"next_cursor"`
}

// Register creates a new account. The default role "client" is assigned
// server-side; additional roles (provider, admin) are granted by an
// administrator, never by self-registration.
func (s *Service) Register(ctx context.Context, email, phone, password string) (*TokenPair, error) {
	_, err := s.store.GetUserByEmail(ctx, email)
	if err == nil {
		return nil, ErrUserAlreadyExists
	}
	if !errors.Is(err, ErrUserNotFound) {
		return nil, fmt.Errorf("check existing user: %w", err)
	}

	passwordHash, err := Hash(password, s.passwordParams)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	var phonePtr *string
	if phone != "" {
		phonePtr = &phone
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	userID, err := s.store.CreateUser(ctx, tx, email, phonePtr, passwordHash)
	if err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			s.logger.Error("rollback failed", "error", rbErr)
		}
		return nil, fmt.Errorf("create user: %w", err)
	}
	if err := s.store.AddRole(ctx, tx, userID, "client"); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			s.logger.Error("rollback failed", "error", rbErr)
		}
		return nil, fmt.Errorf("assign default role: %w", err)
	}
	pair, err := s.issueTokenPairTx(ctx, tx, userID, email, []string{"client"})
	if err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			s.logger.Error("rollback failed", "error", rbErr)
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit registration: %w", err)
	}

	return pair, nil
}

// Login verifies credentials and issues a token pair. It returns the same
// generic error for unknown users and wrong passwords to avoid account
// enumeration.
func (s *Service) Login(ctx context.Context, email, password string) (*TokenPair, error) {
	record, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		// Perform a dummy hash so timing is similar whether or not the user
		// exists, mitigating enumeration via response timing.
		if _, err := Hash("x", s.passwordParams); err != nil {
			return nil, fmt.Errorf("hash dummy: %w", err)
		}
		return nil, ErrInvalidCredentials
	}

	if !record.IsActive {
		return nil, ErrInvalidCredentials
	}

	ok, err := Verify(password, record.PasswordHash)
	if err != nil || !ok {
		return nil, ErrInvalidCredentials
	}

	roles := record.Roles
	if roles == nil {
		roles = []string{}
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	pair, err := s.issueTokenPairTx(ctx, tx, record.ID, record.Email, roles)
	if err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			s.logger.Error("rollback failed", "error", rbErr)
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit login: %w", err)
	}
	return pair, nil
}

// Refresh rotates a refresh token atomically. The presented plaintext token is
// hashed, locked, and revoked while a replacement token is inserted and linked
// in the same transaction. If the token was already rotated (a replay), every
// active token for the user is revoked and ErrTokenReplayDetected is returned.
// The new access token carries the user's current email and roles so downstream
// role checks keep working after rotation.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	refreshHash := hashToken(refreshToken)
	now := time.Now().UTC()

	refreshPlain, replacementHash, err := GenerateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}
	expiresAt := now.Add(s.refreshTokenTTL)

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			s.logger.Error("rollback failed", "error", rbErr)
		}
	}()

	row, err := s.store.FindRefreshTokenForUpdate(ctx, tx, refreshHash)
	if err != nil {
		if errors.Is(err, ErrRefreshTokenNotFound) {
			return nil, ErrInvalidCredentials
		}
		if errors.Is(err, ErrRefreshInProgress) {
			return nil, ErrRefreshInProgress
		}
		return nil, fmt.Errorf("find refresh token: %w", err)
	}

	if row.RevokedAt.Valid {
		// Token already revoked. Replay detection: if it was replaced by
		// another token, treat reuse as an attack and revoke the whole family.
		if row.ReplacedBy.Valid && row.ReplacedBy.String != "" {
			if err := s.store.RevokeAllUserTokens(ctx, tx, row.UserID); err != nil {
				return nil, fmt.Errorf("revoke user tokens: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, fmt.Errorf("commit replay revocation: %w", err)
			}
			return nil, ErrTokenReplayDetected
		}
		return nil, ErrInvalidCredentials
	}

	if row.ExpiresAt.Before(now) {
		return nil, ErrInvalidCredentials
	}

	affected, err := s.store.RevokeRefreshToken(ctx, tx, refreshHash, replacementHash)
	if err != nil {
		return nil, fmt.Errorf("revoke refresh token: %w", err)
	}
	if affected != 1 {
		// A concurrent request rotated this token first; reject the loser.
		return nil, ErrInvalidCredentials
	}
	if err := s.store.InsertRefreshToken(ctx, tx, row.UserID, replacementHash, expiresAt); err != nil {
		return nil, fmt.Errorf("store refresh token: %w", err)
	}

	record, err := s.store.GetUserByID(ctx, row.UserID)
	if err != nil {
		return nil, fmt.Errorf("load user for refresh: %w", err)
	}

	accessToken, _, err := IssueAccessToken(record.ID, record.Email, record.Roles, AccessTokenConfig{
		Secret: s.accessTokenSecret,
		TTL:    s.accessTokenTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("issue access token: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit rotation: %w", err)
	}

	return &TokenPair{AccessToken: accessToken, RefreshToken: refreshPlain}, nil
}

// Logout revokes the presented refresh token.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	refreshHash := hashToken(refreshToken)
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if _, err := s.store.RevokeRefreshToken(ctx, tx, refreshHash, ""); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			s.logger.Error("rollback failed", "error", rbErr)
		}
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	return tx.Commit(ctx)
}

// ResolvePrincipal reloads the caller's current identity from PostgreSQL so
// role changes and account deactivation take effect without waiting for the
// access token to expire.
func (s *Service) ResolvePrincipal(ctx context.Context, userID int64) (Principal, error) {
	record, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return Principal{}, ErrUserNotFound
		}
		return Principal{}, fmt.Errorf("resolve principal: %w", err)
	}
	if !record.IsActive {
		return Principal{}, ErrUserInactive
	}
	return Principal{UserID: record.ID, Email: record.Email, Roles: record.Roles}, nil
}

// AdminListUsers returns a cursor page of accounts for administrators.
func (s *Service) AdminListUsers(ctx context.Context, after int64, limit int, query, role string) (*AdminUserPage, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalidInput
	}
	if role != "" && role != "client" && role != "provider" && role != "admin" {
		return nil, ErrInvalidRole
	}
	users, hasMore, err := s.store.ListAdminUsers(ctx, after, limit, query, role)
	if err != nil {
		return nil, err
	}
	page := &AdminUserPage{Items: users}
	if hasMore && len(users) > 0 {
		next := users[len(users)-1].ID
		page.NextCursor = &next
	}
	return page, nil
}

// AdminSetRole grants or revokes a provider/admin role. The actor cannot change
// their own roles, the last active administrator cannot be demoted, and both
// the role mutation and its audit event commit atomically.
func (s *Service) AdminSetRole(ctx context.Context, actorID, targetID int64, role string, granted bool, requestID string) (*AdminUser, error) {
	if actorID < 1 || targetID < 1 {
		return nil, ErrInvalidInput
	}
	if role != "provider" && role != "admin" {
		return nil, ErrInvalidRole
	}
	if actorID == targetID {
		return nil, ErrSelfRoleChange
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin role change: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			s.logger.Error("rollback failed", "error", rbErr)
		}
	}()

	if err := s.store.LockAdminRoleChanges(ctx, tx); err != nil {
		return nil, err
	}
	target, err := s.store.GetUserForUpdate(ctx, tx, targetID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	if !target.IsActive {
		return nil, ErrUserInactive
	}
	if !granted && role == "admin" && containsRole(target.Roles, "admin") {
		otherAdmins, err := s.store.CountOtherActiveAdmins(ctx, tx, targetID)
		if err != nil {
			return nil, err
		}
		if otherAdmins == 0 {
			return nil, ErrLastAdmin
		}
	}

	oldRoles := append([]string(nil), target.Roles...)
	changed, err := s.store.SetRole(ctx, tx, targetID, role, granted)
	if err != nil {
		return nil, err
	}
	newRoles := replaceRole(oldRoles, role, granted)
	if changed {
		action := "user.role.revoked"
		if granted {
			action = "user.role.granted"
		}
		if err := s.store.InsertAuditEvent(ctx, tx, actorID, targetID, action, requestID, map[string]any{
			"role":      role,
			"granted":   granted,
			"old_roles": oldRoles,
			"new_roles": newRoles,
		}); err != nil {
			return nil, err
		}
	}
	if granted && role == "provider" {
		createdProfile, err := s.store.EnsureProviderProfile(ctx, tx, targetID)
		if err != nil {
			return nil, err
		}
		if createdProfile {
			if err := s.store.InsertAuditEvent(ctx, tx, actorID, targetID, "provider.profile.created", requestID, map[string]any{
				"source": "role_grant",
			}); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit role change: %w", err)
	}

	return &AdminUser{
		ID:          target.ID,
		Email:       target.Email,
		Phone:       target.Phone,
		DisplayName: target.DisplayName,
		Roles:       newRoles,
		IsActive:    target.IsActive,
		CreatedAt:   target.CreatedAt,
	}, nil
}

func containsRole(roles []string, role string) bool {
	for _, current := range roles {
		if current == role {
			return true
		}
	}
	return false
}

func replaceRole(roles []string, role string, granted bool) []string {
	result := make([]string, 0, len(roles)+1)
	for _, current := range roles {
		if current == role {
			continue
		}
		result = append(result, current)
	}
	if granted {
		result = append(result, role)
	}
	sort.Strings(result)
	return result
}

// Me returns the authenticated user's public profile.
func (s *Service) Me(ctx context.Context, userID int64) (*User, error) {
	record, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	profileImageKey, err := s.store.GetProfileImageKey(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &User{
		ID:              record.ID,
		Email:           record.Email,
		Phone:           record.Phone,
		IsActive:        record.IsActive,
		Roles:           record.Roles,
		ProfileImageURL: platform.PublicImageURL(profileImageKey),
	}, nil
}

// issueTokenPairTx mints an access token and persists a new refresh token
// using the caller-provided transaction. The caller owns commit/rollback so
// token issuance stays atomic with the surrounding state change.
func (s *Service) issueTokenPairTx(ctx context.Context, tx pgx.Tx, userID int64, email string, roles []string) (*TokenPair, error) {
	accessToken, _, err := IssueAccessToken(userID, email, roles, AccessTokenConfig{
		Secret: s.accessTokenSecret,
		TTL:    s.accessTokenTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("issue access token: %w", err)
	}

	refreshPlain, refreshHash, err := GenerateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}

	expiresAt := time.Now().UTC().Add(s.refreshTokenTTL)
	if err := s.store.InsertRefreshToken(ctx, tx, userID, refreshHash, expiresAt); err != nil {
		return nil, fmt.Errorf("store refresh token: %w", err)
	}

	return &TokenPair{
		AccessToken:  accessToken,
		RefreshToken: refreshPlain,
	}, nil
}

// hashToken hashes a plaintext refresh token for lookup/revocation.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}
