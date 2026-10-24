package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Principal is the authenticated caller established server-side from a valid
// access token. Client-supplied roles/IDs are never trusted.
type Principal struct {
	UserID int64
	Email  string
	Roles  []string
}

// Store abstracts persistence for the identity service.
type Store interface {
	GetUserByEmail(ctx context.Context, email string) (*UserRecord, error)
	GetUserByID(ctx context.Context, userID int64) (*UserRecord, error)
	GetProfileImageKey(ctx context.Context, userID int64) (string, error)
	CreateUser(ctx context.Context, tx pgx.Tx, email string, phone *string, passwordHash string) (int64, error)
	AddRole(ctx context.Context, tx pgx.Tx, userID int64, role string) error
	FindRefreshToken(ctx context.Context, tokenHash string) (*RefreshRow, error)
	FindRefreshTokenForUpdate(ctx context.Context, tx pgx.Tx, tokenHash string) (*RefreshRow, error)
	InsertRefreshToken(ctx context.Context, tx pgx.Tx, userID int64, tokenHash string, expiresAt time.Time) error
	RevokeRefreshToken(ctx context.Context, tx pgx.Tx, tokenHash string, replacedBy string) (int64, error)
	RevokeAllUserTokens(ctx context.Context, tx pgx.Tx, userID int64) error
	BeginTx(ctx context.Context) (pgx.Tx, error)
	ListAdminUsers(ctx context.Context, after int64, limit int, query, role string) ([]AdminUser, bool, error)
	GetUserForUpdate(ctx context.Context, tx pgx.Tx, userID int64) (*UserRecord, error)
	SetRole(ctx context.Context, tx pgx.Tx, userID int64, role string, granted bool) (bool, error)
	EnsureProviderProfile(ctx context.Context, tx pgx.Tx, userID int64) (bool, error)
	LockAdminRoleChanges(ctx context.Context, tx pgx.Tx) error
	CountOtherActiveAdmins(ctx context.Context, tx pgx.Tx, userID int64) (int, error)
	InsertAuditEvent(ctx context.Context, tx pgx.Tx, actorID, targetID int64, action, requestID string, metadata map[string]any) error
}

// AdminUser is the administrative view of an account.
type AdminUser struct {
	ID          int64     `json:"id"`
	Email       string    `json:"email"`
	Phone       *string   `json:"phone"`
	DisplayName string    `json:"display_name"`
	Roles       []string  `json:"roles"`
	IsActive    bool      `json:"is_active"`
	CreatedAt   time.Time `json:"created_at"`
}

// User is a public user profile returned to callers.
type User struct {
	ID              int64
	Email           string
	Phone           *string
	IsActive        bool
	Roles           []string
	ProfileImageURL string
}

// UserRecord is the internal row representation including the password hash.
type UserRecord struct {
	ID           int64
	Email        string
	Phone        *string
	PasswordHash string
	IsActive     bool
	Roles        []string
	DisplayName  string
	CreatedAt    time.Time
}

// RefreshRow represents a refresh token record.
type RefreshRow struct {
	ID         int64
	UserID     int64
	TokenHash  string
	ExpiresAt  time.Time
	RevokedAt  sql.NullTime
	ReplacedBy sql.NullString
}

// pgxStore implements Store using pgx.
type pgxStore struct {
	pool *pgxpool.Pool
}

func NewPgxStore(pool *pgxpool.Pool) *pgxStore {
	return &pgxStore{pool: pool}
}

func (s *pgxStore) BeginTx(ctx context.Context) (pgx.Tx, error) {
	return s.pool.Begin(ctx)
}

func (s *pgxStore) CreateUser(ctx context.Context, tx pgx.Tx, email string, phone *string, passwordHash string) (int64, error) {
	var id int64
	row := tx.QueryRow(ctx, `
		INSERT INTO users (email, phone, password_hash)
		VALUES ($1, $2, $3)
		RETURNING id
	`, email, phone, passwordHash)
	if err := row.Scan(&id); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, ErrUserAlreadyExists
		}
		return 0, fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

func (s *pgxStore) GetUserByEmail(ctx context.Context, email string) (*UserRecord, error) {
	var (
		id       int64
		phone    sql.NullString
		password string
		isActive bool
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, phone, password_hash, is_active
		FROM users WHERE email = $1
	`, email).Scan(&id, &phone, &password, &isActive)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	record := &UserRecord{
		ID:           id,
		Email:        email,
		PasswordHash: password,
		IsActive:     isActive,
	}
	if phone.Valid {
		record.Phone = &phone.String
	}

	roles, err := s.getUserRoles(ctx, id)
	if err != nil {
		return nil, err
	}
	record.Roles = roles
	return record, nil
}

func (s *pgxStore) GetUserByID(ctx context.Context, userID int64) (*UserRecord, error) {
	var (
		id       int64
		email    string
		phone    sql.NullString
		password string
		isActive bool
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, phone, password_hash, is_active
		FROM users WHERE id = $1
	`, userID).Scan(&id, &email, &phone, &password, &isActive)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	record := &UserRecord{
		ID:           id,
		Email:        email,
		PasswordHash: password,
		IsActive:     isActive,
	}
	if phone.Valid {
		record.Phone = &phone.String
	}

	roles, err := s.getUserRoles(ctx, id)
	if err != nil {
		return nil, err
	}
	record.Roles = roles
	return record, nil
}

func (s *pgxStore) GetProfileImageKey(ctx context.Context, userID int64) (string, error) {
	var storageKey string
	err := s.pool.QueryRow(ctx, `
		SELECT storage_key FROM user_images WHERE user_id = $1 AND purpose = 'profile'
	`, userID).Scan(&storageKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get profile image key: %w", err)
	}
	return storageKey, nil
}

func (s *pgxStore) getUserRoles(ctx context.Context, userID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT role FROM user_roles WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("get user roles: %w", err)
	}
	defer rows.Close()

	var roles []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	if roles == nil {
		roles = []string{}
	}
	return roles, rows.Err()
}

func (s *pgxStore) AddRole(ctx context.Context, tx pgx.Tx, userID int64, role string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO user_roles (user_id, role) VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, userID, role)
	return err
}

func (s *pgxStore) FindRefreshToken(ctx context.Context, tokenHash string) (*RefreshRow, error) {
	var (
		id         int64
		userID     int64
		tokenHashS string
		expiresAt  time.Time
		revokedAt  sql.NullTime
		replacedBy sql.NullString
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, token_hash, expires_at, revoked_at, replaced_by
		FROM refresh_tokens WHERE token_hash = $1
	`, tokenHash).Scan(&id, &userID, &tokenHashS, &expiresAt, &revokedAt, &replacedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRefreshTokenNotFound
		}
		return nil, fmt.Errorf("find refresh token: %w", err)
	}
	return &RefreshRow{
		ID:         id,
		UserID:     userID,
		TokenHash:  tokenHashS,
		ExpiresAt:  expiresAt,
		RevokedAt:  revokedAt,
		ReplacedBy: replacedBy,
	}, nil
}

func (s *pgxStore) FindRefreshTokenForUpdate(ctx context.Context, tx pgx.Tx, tokenHash string) (*RefreshRow, error) {
	var (
		id         int64
		userID     int64
		tokenHashS string
		expiresAt  time.Time
		revokedAt  sql.NullTime
		replacedBy sql.NullString
	)
	err := tx.QueryRow(ctx, `
		SELECT id, user_id, token_hash, expires_at, revoked_at, replaced_by
		FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE
	`, tokenHash).Scan(&id, &userID, &tokenHashS, &expiresAt, &revokedAt, &replacedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRefreshTokenNotFound
		}
		return nil, fmt.Errorf("lock refresh token: %w", err)
	}
	return &RefreshRow{
		ID:         id,
		UserID:     userID,
		TokenHash:  tokenHashS,
		ExpiresAt:  expiresAt,
		RevokedAt:  revokedAt,
		ReplacedBy: replacedBy,
	}, nil
}

func (s *pgxStore) InsertRefreshToken(ctx context.Context, tx pgx.Tx, userID int64, tokenHash string, expiresAt time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
	`, userID, tokenHash, expiresAt)
	return err
}

func (s *pgxStore) RevokeRefreshToken(ctx context.Context, tx pgx.Tx, tokenHash string, replacedBy string) (int64, error) {
	arg := sql.NullString{}
	if replacedBy != "" {
		arg = sql.NullString{String: replacedBy, Valid: true}
	}
	result, err := tx.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = now(), replaced_by = $2
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, tokenHash, arg)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

func (s *pgxStore) RevokeAllUserTokens(ctx context.Context, tx pgx.Tx, userID int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL
	`, userID)
	return err
}

func (s *pgxStore) ListAdminUsers(ctx context.Context, after int64, limit int, query, role string) ([]AdminUser, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, u.phone, u.is_active, u.created_at,
		       COALESCE(p.display_name, ''),
		       COALESCE(array_agg(ur.role ORDER BY ur.role) FILTER (WHERE ur.role IS NOT NULL), '{}'::text[])
		FROM users u
		LEFT JOIN provider_profiles p ON p.provider_id = u.id
		LEFT JOIN user_roles ur ON ur.user_id = u.id
		WHERE ($1 = 0 OR u.id > $1)
		  AND ($2 = '' OR u.email ILIKE '%' || $2 || '%'
		       OR COALESCE(u.phone, '') ILIKE '%' || $2 || '%'
		       OR COALESCE(p.display_name, '') ILIKE '%' || $2 || '%')
		  AND ($3 = '' OR EXISTS (
		       SELECT 1 FROM user_roles filtered WHERE filtered.user_id = u.id AND filtered.role = $3
		  ))
		GROUP BY u.id, u.email, u.phone, u.is_active, u.created_at, p.display_name
		ORDER BY u.id
		LIMIT $4
	`, after, query, role, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list admin users: %w", err)
	}
	defer rows.Close()

	users := make([]AdminUser, 0, limit+1)
	for rows.Next() {
		var (
			user  AdminUser
			phone sql.NullString
		)
		if err := rows.Scan(&user.ID, &user.Email, &phone, &user.IsActive, &user.CreatedAt, &user.DisplayName, &user.Roles); err != nil {
			return nil, false, fmt.Errorf("scan admin user: %w", err)
		}
		if phone.Valid {
			user.Phone = &phone.String
		}
		if user.Roles == nil {
			user.Roles = []string{}
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate admin users: %w", err)
	}

	hasMore := len(users) > limit
	if hasMore {
		users = users[:limit]
	}
	return users, hasMore, nil
}

func (s *pgxStore) GetUserForUpdate(ctx context.Context, tx pgx.Tx, userID int64) (*UserRecord, error) {
	var (
		record   UserRecord
		phone    sql.NullString
		password string
	)
	err := tx.QueryRow(ctx, `
		SELECT u.id, u.email, u.phone, u.password_hash, u.is_active, u.created_at,
		       COALESCE((SELECT p.display_name FROM provider_profiles p WHERE p.provider_id = u.id), '')
		FROM users u WHERE u.id = $1 FOR UPDATE
	`, userID).Scan(&record.ID, &record.Email, &phone, &password, &record.IsActive, &record.CreatedAt, &record.DisplayName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("lock user: %w", err)
	}
	if phone.Valid {
		record.Phone = &phone.String
	}
	record.PasswordHash = password

	rows, err := tx.Query(ctx, `SELECT role FROM user_roles WHERE user_id = $1 ORDER BY role`, userID)
	if err != nil {
		return nil, fmt.Errorf("load locked user roles: %w", err)
	}
	defer rows.Close()
	record.Roles = []string{}
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("scan locked user role: %w", err)
		}
		record.Roles = append(record.Roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate locked user roles: %w", err)
	}
	return &record, nil
}

func (s *pgxStore) SetRole(ctx context.Context, tx pgx.Tx, userID int64, role string, granted bool) (bool, error) {
	if granted {
		result, err := tx.Exec(ctx, `
			INSERT INTO user_roles (user_id, role) VALUES ($1, $2)
			ON CONFLICT DO NOTHING
		`, userID, role)
		if err != nil {
			return false, fmt.Errorf("grant role: %w", err)
		}
		return result.RowsAffected() == 1, nil
	}
	result, err := tx.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1 AND role = $2`, userID, role)
	if err != nil {
		return false, fmt.Errorf("revoke role: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *pgxStore) EnsureProviderProfile(ctx context.Context, tx pgx.Tx, userID int64) (bool, error) {
	result, err := tx.Exec(ctx, `
		INSERT INTO provider_profiles (provider_id, display_name, is_active, verification_status)
		VALUES ($1, '', false, 'pending')
		ON CONFLICT (provider_id) DO NOTHING
	`, userID)
	if err != nil {
		return false, fmt.Errorf("ensure provider profile: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *pgxStore) LockAdminRoleChanges(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('beautyra.admin.roles'))`)
	if err != nil {
		return fmt.Errorf("lock admin role changes: %w", err)
	}
	return nil
}

func (s *pgxStore) CountOtherActiveAdmins(ctx context.Context, tx pgx.Tx, userID int64) (int, error) {
	var count int
	err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM user_roles ur
		JOIN users u ON u.id = ur.user_id
		WHERE ur.role = 'admin' AND ur.user_id <> $1 AND u.is_active
	`, userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count active admins: %w", err)
	}
	return count, nil
}

func (s *pgxStore) InsertAuditEvent(ctx context.Context, tx pgx.Tx, actorID, targetID int64, action, requestID string, metadata map[string]any) error {
	payload, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode audit metadata: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (actor_user_id, action, target_type, target_id, request_id, metadata)
		VALUES ($1, $2, 'user', $3, $4, $5::jsonb)
	`, actorID, action, targetID, requestID, string(payload))
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}
