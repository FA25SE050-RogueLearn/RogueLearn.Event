package jwt

// import (
// 	"io"
// 	"log/slog"
// 	"testing"
// 	"time"

// 	"github.com/golang-jwt/jwt/v5"
// 	"github.com/stretchr/testify/assert"
// 	"github.com/stretchr/testify/require"
// )

// // Helper function to generate a token for testing
// func generateTestToken(claims *UserClaims, secretKey string) (string, error) {
// 	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
// 	return token.SignedString([]byte(secretKey))
// }

// func TestGetUserClaimsFromToken(t *testing.T) {
// 	secretKey := "qwertyuiopasdfghjklzxcvbnm123456"

// 	// Use a "discard" logger that doesn't print anything during tests
// 	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

// 	parser := NewJWTParser(secretKey, logger)

// 	t.Run("Successful Parse of Valid Token", func(t *testing.T) {
// 		// 1. Define the claims we want to encode in our test token.
// 		// These fields MUST match your UserClaims struct.
// 		wantClaims := &UserClaims{
// 			Sub:   "asd",
// 			Email: "jrocket@example.com",
// 			Roles: []string{"Manager", "Project Administrator"},
// 			RegisteredClaims: jwt.RegisteredClaims{
// 				ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
// 				IssuedAt:  jwt.NewNumericDate(time.Now()),
// 				Issuer:    "test-issuer",
// 				Subject:   "auth-token",
// 			},
// 		}

// 		// 2. Generate a valid token string from these claims
// 		tokenString, err := generateTestToken(wantClaims, secretKey)
// 		require.NoError(t, err, "Failed to generate test token")

// 		// 3. Call the function we are testing
// 		gotClaims, err := parser.GetUserClaimsFromToken(tokenString)

// 		// 4. Assert the results
// 		require.NoError(t, err, "GetUserClaimsFromToken should not return an error for a valid token")
// 		require.NotNil(t, gotClaims, "GetUserClaimsFromToken should return non-nil claims")

// 		assert.Equal(t, wantClaims.ID, gotClaims.ID)
// 		assert.Equal(t, wantClaims.Username, gotClaims.Username)
// 		assert.Equal(t, wantClaims.Email, gotClaims.Email)
// 		assert.Equal(t, wantClaims.Roles, gotClaims.Roles)
// 		assert.Equal(t, wantClaims.Issuer, gotClaims.Issuer)
// 	})

// 	t.Run("Fail on Invalid Signature", func(t *testing.T) {
// 		claims := &UserClaims{
// 			ID: "user-id",
// 			RegisteredClaims: jwt.RegisteredClaims{
// 				ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
// 			},
// 		}

// 		// Generate a token with the correct secret
// 		tokenString, err := generateTestToken(claims, secretKey)
// 		require.NoError(t, err)

// 		// Create a new parser with the WRONG secret key
// 		badParser := NewJWTParser("this-is-the-wrong-secret-key", logger)

// 		// Try to parse the token. It should fail.
// 		_, err = badParser.GetUserClaimsFromToken(tokenString)
// 		require.Error(t, err, "Expected an error for an invalid signature")
// 		assert.ErrorIs(t, err, jwt.ErrSignatureInvalid, "Error should be of type ErrSignatureInvalid")
// 	})

// 	t.Run("Successful Parse of Expired Token", func(t *testing.T) {
// 		// Your GetUserClaimsFromToken function is specifically designed to parse claims
// 		// even if the token is expired. This test verifies that behavior.
// 		claims := &UserClaims{
// 			ID: "expired-user-id",
// 			RegisteredClaims: jwt.RegisteredClaims{
// 				ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)), // Expired 1 hour ago
// 			},
// 		}

// 		tokenString, err := generateTestToken(claims, secretKey)
// 		require.NoError(t, err)

// 		gotClaims, err := parser.GetUserClaimsFromToken(tokenString)

// 		// We expect NO error from GetUserClaimsFromToken because it ignores expiration
// 		assert.NoError(t, err, "GetUserClaimsFromToken should not return an error for an expired token")
// 		require.NotNil(t, gotClaims)
// 		assert.Equal(t, "expired-user-id", gotClaims.ID)

// 		// In contrast, VerifyToken should fail for the same token
// 		_, err = parser.VerifyToken(tokenString)
// 		require.Error(t, err, "VerifyToken should fail for an expired token")
// 		assert.ErrorIs(t, err, jwt.ErrTokenExpired)
// 	})

// 	t.Run("Fail on Malformed Token", func(t *testing.T) {
// 		malformedToken := "this.is.not.a.valid.jwt"

// 		_, err := parser.GetUserClaimsFromToken(malformedToken)
// 		require.Error(t, err, "Expected an error for a malformed token")
// 	})
// }
