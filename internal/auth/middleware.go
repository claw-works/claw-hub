package auth

import (
	"context"
	"net/http"

	"github.com/claw-works/pincer/internal/project"
)

type contextKey string

const UserContextKey contextKey = "auth_user"

// Middleware validates X-API-Key header OR ?api_key= query param.
// Query param support is required for browser WebSocket clients which
// cannot send custom headers during the WS upgrade handshake.
// If the key is valid, injects the User into the request context.
// If the key is missing or invalid, returns 401.
// For WebSocket browser clients that cannot set headers, the key may also be
// supplied as the ?api_key=<key> query parameter.
func Middleware(store *project.PGStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := r.Header.Get("X-API-Key")
			if apiKey == "" {
				// Fall back to query parameter (used by browser WebSocket clients).
				apiKey = r.URL.Query().Get("api_key")
			}
			if apiKey == "" {
				http.Error(w, `{"error":"X-API-Key required"}`, http.StatusUnauthorized)
				return
			}
			user, err := store.GetUserByAPIKey(r.Context(), apiKey)
			if err != nil {
				http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// FromContext extracts the authenticated user from context.
// Returns nil if not authenticated.
func FromContext(ctx context.Context) *project.User {
	u, _ := ctx.Value(UserContextKey).(*project.User)
	return u
}
