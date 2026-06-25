package auth

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/azrtydxb/novagrade/internal/domain"
)

// Principal represents an authenticated caller.
type Principal struct {
	ID       string
	TenantID string
	Roles    []domain.Role
}

// Claims are the JWT payload fields.
type Claims struct {
	TenantID string   `json:"tenant_id"`
	Roles    []string `json:"roles"`
	jwt.RegisteredClaims
}

// signingKey reads JWT_SIGNING_KEY from the environment.
func signingKey() ([]byte, error) {
	k := os.Getenv("JWT_SIGNING_KEY")
	if k == "" {
		return nil, errors.New("auth: JWT_SIGNING_KEY not set")
	}
	return []byte(k), nil
}

// IssueToken creates a signed HS256 JWT for the given principal, valid for ttl.
func IssueToken(p Principal, ttl time.Duration) (string, error) {
	key, err := signingKey()
	if err != nil {
		return "", err
	}
	roleStrs := make([]string, len(p.Roles))
	for i, r := range p.Roles {
		roleStrs[i] = string(r)
	}
	claims := Claims{
		TenantID: p.TenantID,
		Roles:    roleStrs,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.ID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString(key)
}

// VerifyToken validates a signed JWT and returns the Principal.
func VerifyToken(tokenStr string) (Principal, error) {
	key, err := signingKey()
	if err != nil {
		return Principal{}, err
	}
	t, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
		}
		return key, nil
	})
	if err != nil {
		return Principal{}, fmt.Errorf("auth: invalid token: %w", err)
	}
	claims, ok := t.Claims.(*Claims)
	if !ok || !t.Valid {
		return Principal{}, errors.New("auth: invalid claims")
	}
	roles := make([]domain.Role, len(claims.Roles))
	for i, r := range claims.Roles {
		roles[i] = domain.Role(r)
	}
	return Principal{
		ID:       claims.Subject,
		TenantID: claims.TenantID,
		Roles:    roles,
	}, nil
}
