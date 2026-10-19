package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
	CreateUser(ctx context.Context, tx pgx.Tx, email string, phone *string, passwordHash string) (int64, error)
	AddRole(ctx context.Context, tx pgx.Tx, userID int64, role string) error
	FindRefreshToken(ctx context.Context, tokenHash string) (*RefreshRow, error)
	FindRefreshTokenForUpdate(ctx context.Context, tx pgx.Tx, tokenHash string) (*RefreshRow, error)
	InsertRefreshToken(ctx context.Context, tx pgx.Tx, userID int64, tokenHash string, expiresAt time.Time) error
	RevokeRefreshToken(ctx context.Context, tx pgx.Tx, tokenHash string, replacedBy string) (int64, error)
	RevokeAllUserTokens(ctx context.Context, tx pgx.Tx, userID int64) error
	BeginTx(ctx context.Context) (pgx.Tx, error)
}

// User is a public user profile returned to callers.
type User struct {
	ID       int64
	Email    string
	Phone    *string
	IsActive bool
	Roles    []string
}

// UserRecord is the internal row representation including the password hash.
type UserRecord struct {
	ID           int64
	Email        string
	Phone        *string
	PasswordHash string
	IsActive     bool
	Roles        []string
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
