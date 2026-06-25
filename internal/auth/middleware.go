package auth

import (
	"context"
	"net/http"
	"strings"
)

type contextKey string

const principalKey contextKey = "principal"

// Middleware returns an HTTP middleware that extracts a Principal from:
//  1. Authorization: Bearer <JWT>
//  2. X-API-Key: <key>
//
// Returns 401 if neither is valid.
func Middleware(resolver *APIKeyResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var p Principal
			var err error

			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				token := strings.TrimPrefix(auth, "Bearer ")
				p, err = VerifyToken(token)
			} else if key := r.Header.Get("X-API-Key"); key != "" {
				p, err = resolver.Resolve(key)
			} else {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), principalKey, p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// PrincipalFromContext extracts the authenticated principal from a request context.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}
