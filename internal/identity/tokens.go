package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims are the JWT claims embedded in access tokens.
type Claims struct {
	UserID int64    `json:"user_id"`
	Email  string   `json:"email"`
	Roles  []string `json:"roles"`
	jwt.RegisteredClaims
}

// TokenPair holds a freshly minted access token and refresh token.
type TokenPair struct {
	AccessToken  string
	RefreshToken string // plaintext, only returned to caller once
}

// AccessTokenConfig holds JWT access token settings.
type AccessTokenConfig struct {
	Secret string
	TTL    time.Duration
}

// IssueAccessToken signs a JWT access token with the given claims and config.
func IssueAccessToken(userID int64, email string, roles []string, cfg AccessTokenConfig) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(cfg.TTL)

	claims := Claims{
		UserID: userID,
		Email:  email,
		Roles:  roles,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("%d", userID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(cfg.Secret))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// ParseAccessToken validates and parses a signed JWT access token.
func ParseAccessToken(tokenString string, secret string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid access token claims")
	}
	return claims, nil
}

// GenerateRefreshToken creates a cryptographically random opaque refresh token
// plaintext and its SHA-256 hash (base64) for storage. Never store the
// plaintext refresh token.
func GenerateRefreshToken() (plaintext, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plaintext = hex.EncodeToString(buf)

	sum := sha256.Sum256([]byte(plaintext))
	hash = base64.RawStdEncoding.EncodeToString(sum[:])
	return plaintext, hash, nil
}
