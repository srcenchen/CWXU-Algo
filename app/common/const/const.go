package _const

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"sync"
)

var (
	jwtSecretMu sync.RWMutex
	jwtSecret   string

	jwtKeysMu     sync.RWMutex
	jwtPrivateKey *rsa.PrivateKey
	jwtPublicKey  *rsa.PublicKey
)

// ConfigureJWTSecret sets the config.yaml value. A non-empty environment
// variable takes precedence so orchestrators can still inject secrets safely.
func ConfigureJWTSecret(value string) error {
	if env := strings.TrimSpace(os.Getenv("CWXU_JWT_SECRET")); env != "" {
		value = env
	}
	value = strings.TrimSpace(value)
	if len(value) < 32 {
		return fmt.Errorf("server.jwt_secret must contain at least 32 characters (got %d)", len(value))
	}
	jwtSecretMu.Lock()
	jwtSecret = value
	jwtSecretMu.Unlock()
	return nil
}

// JWTSecret returns the deployment JWT secret. Authentication must fail closed:
// silently falling back to a public value would allow anyone to forge tokens.
func JWTSecret() string {
	jwtSecretMu.RLock()
	value := jwtSecret
	jwtSecretMu.RUnlock()
	if value == "" {
		value = strings.TrimSpace(os.Getenv("CWXU_JWT_SECRET"))
	}
	if len(value) < 32 {
		panic(fmt.Sprintf("server.jwt_secret must be configured with at least 32 characters (got %d)", len(value)))
	}
	return value
}

// ConfigureJWTKeys sets the RSA key pair from config.yaml values. A non-empty
// environment variable takes precedence so orchestrators can still inject
// secrets safely. Fail closed: any error aborts startup instead of leaving a
// forged-token hole.
func ConfigureJWTKeys(privatePEM, publicPEM string) error {
	if env := strings.TrimSpace(os.Getenv("CWXU_JWT_PRIVATE_KEY")); env != "" {
		privatePEM = env
	}
	if env := strings.TrimSpace(os.Getenv("CWXU_JWT_PUBLIC_KEY")); env != "" {
		publicPEM = env
	}
	privatePEM = strings.TrimSpace(privatePEM)
	publicPEM = strings.TrimSpace(publicPEM)

	var priv *rsa.PrivateKey
	if privatePEM != "" {
		block, _ := pem.Decode([]byte(privatePEM))
		if block == nil {
			return fmt.Errorf("server.jwt_private_key is not valid PEM")
		}
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			if k2, err2 := x509.ParsePKCS8PrivateKey(block.Bytes); err2 == nil {
				if rk, ok := k2.(*rsa.PrivateKey); ok {
					key = rk
					err = nil
				}
			}
			if err != nil {
				return fmt.Errorf("server.jwt_private_key: parse RSA private key: %w", err)
			}
		}
		priv = key
	}

	var pub *rsa.PublicKey
	if publicPEM != "" {
		block, _ := pem.Decode([]byte(publicPEM))
		if block == nil {
			return fmt.Errorf("server.jwt_public_key is not valid PEM")
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return fmt.Errorf("server.jwt_public_key: parse RSA public key: %w", err)
		}
		rk, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("server.jwt_public_key: not an RSA public key")
		}
		pub = rk
	}

	jwtKeysMu.Lock()
	jwtPrivateKey = priv
	jwtPublicKey = pub
	jwtKeysMu.Unlock()
	return nil
}

// JWTPrivateKey returns the RSA private key used to sign JWTs. Only services
// that sign (user / agent) configure it. Panics when unset: fail closed.
func JWTPrivateKey() *rsa.PrivateKey {
	jwtKeysMu.RLock()
	key := jwtPrivateKey
	jwtKeysMu.RUnlock()
	if key == nil {
		panic("server.jwt_private_key must be configured (RSA private key PEM)")
	}
	return key
}

// JWTPublicKey returns the RSA public key used to verify JWTs. Panics when
// unset: fail closed.
func JWTPublicKey() *rsa.PublicKey {
	jwtKeysMu.RLock()
	key := jwtPublicKey
	jwtKeysMu.RUnlock()
	if key == nil {
		panic("server.jwt_public_key must be configured (RSA public key PEM)")
	}
	return key
}
