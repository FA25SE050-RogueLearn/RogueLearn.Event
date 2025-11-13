package jwt

import (
	jwt "github.com/golang-jwt/jwt/v5"
)

// UserClaims represents the claims extracted from a Supabase JWT token
// This structure matches the format used by Supabase Auth
type UserClaims struct {
	// Sub is the user ID (UUID format) - matches Supabase's "sub" claim
	Sub string `json:"sub"`

	// Email is the user's email address
	Email string `json:"email"`

	// Role is the primary role from Supabase (e.g., "authenticated")
	Role string `json:"role"`

	// Roles is a JSON array of custom roles (e.g., ["Game Master", "Player"])
	// This matches the custom roles claim added by the roguelearn.user service
	Roles []string `json:"roles"`

	// Standard JWT claims (iss, aud, exp, iat, etc.)
	jwt.RegisteredClaims
}

// GetUserID returns the user ID from the sub claim
func (c *UserClaims) GetUserID() string {
	return c.Sub
}
