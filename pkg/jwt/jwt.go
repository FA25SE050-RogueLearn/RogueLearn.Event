// jwt package provides function to `read and parse` token
package jwt

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-jwt/jwt/v5"
)

type JWTParser struct {
	logger       *slog.Logger
	secretKey    string
	issuer       string // Expected issuer (e.g., "https://xxx.supabase.co/auth/v1")
	audience     string // Expected audience (e.g., "authenticated")
	validateTime bool   // Whether to validate token expiration
}

// NewJWTParser creates a new JWT parser with Supabase-compatible validation
// issuer should be in the format: "https://{project-ref}.supabase.co/auth/v1"
// audience should typically be "authenticated"
func NewJWTParser(secretKey, issuer, audience string, logger *slog.Logger) *JWTParser {
	return &JWTParser{
		logger:       logger,
		secretKey:    secretKey,
		issuer:       issuer,
		audience:     audience,
		validateTime: true, // Always validate expiration by default
	}
}

// VerifyToken validates a JWT token and returns the claims if valid
// This method validates:
// - Signature using HMAC-SHA256
// - Issuer matches expected issuer
// - Audience matches expected audience
// - Token expiration time
func (p *JWTParser) VerifyToken(tokenStr string) (*UserClaims, error) {
	p.logger.Info("SECRET_KEY", "sec_key", p.secretKey)
	token, err := jwt.ParseWithClaims(tokenStr, &UserClaims{}, func(token *jwt.Token) (any, error) {
		// Verify signing method is HMAC
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			p.logger.Error("Unexpected signing method", "method", token.Method.Alg())
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(p.secretKey), nil
	})

	if err != nil {
		p.logger.Error("Failed to parse JWT token", "error", err)
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	// Extract and validate claims
	claims, ok := token.Claims.(*UserClaims)
	if !ok {
		return nil, errors.New("invalid token claims type")
	}

	if !token.Valid {
		return nil, errors.New("token is invalid")
	}

	// Validate issuer if configured
	if p.issuer != "" {
		issuer, err := claims.GetIssuer()
		if err != nil || issuer != p.issuer {
			p.logger.Warn("Token issuer validation failed", "expected", p.issuer, "actual", issuer)
			return nil, fmt.Errorf("invalid issuer: expected %s, got %s", p.issuer, issuer)
		}
	}

	// Validate audience if configured
	if p.audience != "" {
		audience, err := claims.GetAudience()
		if err != nil {
			p.logger.Warn("Token audience validation failed", "error", err)
			return nil, fmt.Errorf("invalid audience claim: %w", err)
		}
		// Audience can be a string or array, check if our expected audience is present
		audienceFound := false
		for _, aud := range audience {
			if aud == p.audience {
				audienceFound = true
				break
			}
		}
		if !audienceFound {
			p.logger.Warn("Token audience validation failed", "expected", p.audience, "actual", audience)
			return nil, fmt.Errorf("invalid audience: expected %s", p.audience)
		}
	}

	p.logger.Debug("Token verified successfully", "sub", claims.Sub, "email", claims.Email)
	return claims, nil
}

// GetUserClaimsFromToken is deprecated - use VerifyToken instead
// This method allows expired tokens which is a security risk
// Kept for backwards compatibility but should not be used
func (p *JWTParser) GetUserClaimsFromToken(tokenStr string) (*UserClaims, error) {
	p.logger.Warn("GetUserClaimsFromToken is deprecated - use VerifyToken instead")
	return p.VerifyToken(tokenStr)
}
