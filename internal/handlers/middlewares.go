package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/pkg/jwt"
)

type Key string

const (
	UserClaimsKey Key = "user_claims"
)

var (
	ErrUnauthorized  = errors.New("unauthorized: no authentication token provided")
	ErrInvalidClaims = errors.New("unauthorized: invalid user claims")
	ErrMissingRole   = errors.New("forbidden: insufficient permissions")
)

func (hr *HandlerRepo) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		tokenStr := ""

		// 1. Try to get token from Authorization header (standard for most API calls)
		if authHeader != "" {
			headerParts := strings.Split(authHeader, " ")
			if len(headerParts) == 2 && strings.ToLower(headerParts[0]) == "bearer" {
				tokenStr = headerParts[1]
			} else {
				hr.logger.Warn("Malformed Authorization header", "header", authHeader)
				hr.unauthorized(w, r)
				return
			}
		}

		// 2. If not in header, fall back to query parameter (for SSE/EventSource)
		if tokenStr == "" {
			tokenStr = r.URL.Query().Get("auth_token")
			if tokenStr != "" {
				hr.logger.Info("Authenticating via 'auth_token' query parameter (likely SSE).")
			}
		}

		// 3. If still no token, reject the request
		if tokenStr == "" {
			hr.logger.Warn("Missing Authorization token in header or query parameter.")
			hr.unauthorized(w, r)
			return
		}

		// Verify token with full validation (signature, issuer, audience, expiration)
		claims, err := hr.jwtParser.VerifyToken(tokenStr)
		if err != nil {
			hr.logger.Error("Failed to verify token", "error", err)
			hr.unauthorized(w, r)
			return
		}

		hr.logger.Debug("Token verified successfully", "user_id", claims.Sub, "email", claims.Email)

		ctx := context.WithValue(r.Context(), UserClaimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetUserClaims extracts the UserClaims from the request context
// Returns an error if the claims are not found or are invalid
func GetUserClaims(ctx context.Context) (*jwt.UserClaims, error) {
	claims, ok := ctx.Value(UserClaimsKey).(*jwt.UserClaims)
	if !ok || claims == nil {
		return nil, ErrInvalidClaims
	}
	return claims, nil
}

// GetUserID extracts the user ID from the request context
// Returns an error if the claims are not found
func GetUserID(ctx context.Context) (string, error) {
	claims, err := GetUserClaims(ctx)
	if err != nil {
		return "", err
	}
	return claims.Sub, nil
}

// HasRole checks if the user has a specific role
func HasRole(ctx context.Context, role string) bool {
	claims, err := GetUserClaims(ctx)
	if err != nil {
		return false
	}

	for _, r := range claims.Roles {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}

// HasAnyRole checks if the user has any of the specified roles
func HasAnyRole(ctx context.Context, roles ...string) bool {
	for _, role := range roles {
		if HasRole(ctx, role) {
			return true
		}
	}
	return false
}

// RequireRole creates a middleware that requires a specific role
// This middleware should be used after AuthMiddleware
func (hr *HandlerRepo) RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !HasRole(r.Context(), role) {
				hr.logger.Warn("User does not have required role", "required_role", role)
				hr.forbidden(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAnyRole creates a middleware that requires any of the specified roles
// This middleware should be used after AuthMiddleware
func (hr *HandlerRepo) RequireAnyRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !HasAnyRole(r.Context(), roles...) {
				hr.logger.Warn("User does not have any required role", "required_roles", roles)
				hr.forbidden(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AdminOnly is a convenience middleware that requires the "Game Master" role
// This matches the roguelearn.user service's AdminOnly attribute
func (hr *HandlerRepo) AdminOnly(next http.Handler) http.Handler {
	return hr.RequireRole("Game Master")(next)
}
