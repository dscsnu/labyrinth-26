// Package config reads and validates the backend's environment variables and
// manages its Ed25519 signing key.
package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config is the backend's configuration: secrets and URLs from environment
// variables, and rules and timings from config.toml.
type Config struct {
	Port        string
	DatabaseURL string

	// JWTKey signs access tokens and the OAuth state cookie. The SvelteKit app
	// verifies tokens with the matching public key.
	JWTKey ed25519.PrivateKey

	// QRSecret signs and verifies the short-lived waypoint QR tokens
	// (HMAC-SHA256). It never leaves the backend: the /qr display fetches a key
	// derived from it for one waypoint and signs its own tokens with that.
	QRSecret []byte

	GoogleClientID     string
	GoogleClientSecret string

	// PublicURL is where this backend is reachable from the browser; the Google
	// redirect URI is PublicURL + "/auth/google/callback".
	PublicURL *url.URL
	// FrontendURL is the SvelteKit app; after login the browser is sent to
	// FrontendURL + "/auth/callback".
	FrontendURL *url.URL

	// AllowedEmailDomains restricts sign-in to these domains when non-empty.
	AllowedEmailDomains []string

	// Settings are the tunable rules and timings from config.toml.
	Settings Settings
}

// Load reads and validates the environment and config.toml.
//
// It returns every problem at once, joined, so a misconfigured deploy shows all
// missing or invalid values in one go.
func Load() (*Config, error) {
	var errs []error
	require := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
		return v
	}
	parseURL := func(key string) *url.URL {
		raw := require(key)
		if raw == "" {
			return nil
		}
		u, err := url.Parse(strings.TrimRight(raw, "/"))
		if err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s must be an absolute URL", key))
			return nil
		}
		return u
	}

	cfg := &Config{
		Port:               getenv("PORT", "3000"),
		DatabaseURL:        require("DATABASE_URL"),
		GoogleClientID:     require("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: require("GOOGLE_CLIENT_SECRET"),
		PublicURL:          parseURL("PUBLIC_URL"),
		FrontendURL:        parseURL("FRONTEND_URL"),
	}

	if raw := require("JWT_PRIVATE_KEY"); raw != "" {
		key, err := ParsePrivateKey(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("JWT_PRIVATE_KEY: %w", err))
		}
		cfg.JWTKey = key
	}

	if raw := require("QR_SECRET"); raw != "" {
		secret, err := ParseQRSecret(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("QR_SECRET: %w", err))
		}
		cfg.QRSecret = secret
	}

	settings, err := LoadSettings(SettingsPath())
	if err != nil {
		errs = append(errs, err)
	}
	cfg.Settings = settings

	for _, d := range strings.Split(os.Getenv("ALLOWED_EMAIL_DOMAINS"), ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			cfg.AllowedEmailDomains = append(cfg.AllowedEmailDomains, d)
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// SecureCookies reports whether cookies set by the backend should be Secure.
func (c *Config) SecureCookies() bool {
	return c.PublicURL.Scheme == "https"
}

// ParsePrivateKey decodes a base64 PKCS#8 DER Ed25519 private key, the
// format printed by `labyrinth keygen`.
func ParsePrivateKey(b64 string) (ed25519.PrivateKey, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, errors.New("must be base64 (generate with: go run . keygen)")
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("not a PKCS#8 private key: %w", err)
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("must be an Ed25519 key")
	}
	return ed, nil
}

// ParseQRSecret decodes a base64 QR signing secret and rejects one shorter
// than 32 bytes. Generate with: go run . keygen (prints QR_SECRET too).
func ParseQRSecret(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, errors.New("must be base64 (generate with: go run . keygen)")
	}
	if len(raw) < 32 {
		return nil, fmt.Errorf("must be at least 32 bytes, got %d", len(raw))
	}
	return raw, nil
}

// GenerateQRSecret returns a base64 32-byte QR signing secret.
func GenerateQRSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// GenerateKeyPair returns a new Ed25519 keypair as base64 PKCS#8 (private)
// and base64 SPKI (public) DER.
func GenerateKeyPair() (privateB64, publicB64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", "", err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(privDER), base64.StdEncoding.EncodeToString(pubDER), nil
}

// PublicKeyBase64 returns the base64 SPKI DER of key's public half.
func PublicKeyBase64(key ed25519.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// getenv returns the environment variable key, or fallback if it's unset or
// empty.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
