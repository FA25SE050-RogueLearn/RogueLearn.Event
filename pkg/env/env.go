// This package help us to get environment variable with fallbacks
// It supports both .env files (development) and Docker secrets (production)
package env

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const dockerSecretsPath = "/run/secrets"

// GetString retrieves an environment variable value.
// It first checks standard environment variables, then Docker secrets if not found.
// Docker secrets are expected to be in /run/secrets/<key_in_lowercase>
func GetString(key, defaultValue string) string {
	value, exists := os.LookupEnv(key)
	if exists {
		return value
	}

	// Try to read from Docker secrets
	secretValue, err := readDockerSecret(key)
	if err == nil && secretValue != "" {
		return secretValue
	}

	return defaultValue
}

// readDockerSecret attempts to read a secret from Docker Swarm secrets
// Docker secrets are stored in /run/secrets/<secret_name>
// The secret name is the environment variable key converted to lowercase
func readDockerSecret(key string) (string, error) {
	// Convert key to lowercase for Docker secret filename
	secretPath := filepath.Join(dockerSecretsPath, key)

	data, err := os.ReadFile(secretPath)
	if err != nil {
		return "", err
	}

	// Trim any trailing whitespace/newlines that might be in the secret file
	return strings.TrimSpace(string(data)), nil
}

func GetInt(key string, defaultValue int) int {
	value, exists := os.LookupEnv(key)
	if !exists {
		return defaultValue
	}

	intValue, err := strconv.Atoi(value)
	if err != nil {
		panic(err)
	}

	return intValue
}

func GetBool(key string, defaultValue bool) bool {
	value, exists := os.LookupEnv(key)
	if !exists {
		return defaultValue
	}

	boolValue, err := strconv.ParseBool(value)
	if err != nil {
		panic(err)
	}

	return boolValue
}
