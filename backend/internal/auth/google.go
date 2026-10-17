package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GoogleEndpoints are Google's OAuth/OIDC endpoints; tests point them at a
// fake provider.
type GoogleEndpoints struct {
	OAuth   oauth2.Endpoint
	Issuer  string
	JWKSURL string
}

// DefaultGoogleEndpoints are Google's real OAuth and OIDC endpoints.
var DefaultGoogleEndpoints = GoogleEndpoints{
	OAuth:   google.Endpoint,
	Issuer:  "https://accounts.google.com",
	JWKSURL: "https://www.googleapis.com/oauth2/v3/certs",
}

// Google is the OAuth client for Google sign-in.
type Google struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// GoogleIdentity is the verified content of Google's ID token.
type GoogleIdentity struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

// NewGoogle uses Google's fixed endpoints rather than OIDC discovery, so the
// server can start without reaching Google. Signing keys are fetched lazily.
func NewGoogle(clientID, clientSecret, redirectURL string, ep GoogleEndpoints) *Google {
	keySet := oidc.NewRemoteKeySet(context.Background(), ep.JWKSURL)
	return &Google{
		oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     ep.OAuth,
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier: oidc.NewVerifier(ep.Issuer, keySet, &oidc.Config{ClientID: clientID}),
	}
}

// AuthCodeURL returns the Google consent URL for state, with a PKCE challenge
// derived from verifier.
func (g *Google) AuthCodeURL(state, verifier string) string {
	return g.oauth.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "select_account"),
	)
}

// Exchange trades the authorization code for tokens and returns the verified
// identity from Google's ID token.
func (g *Google) Exchange(ctx context.Context, code, verifier string) (GoogleIdentity, error) {
	tok, err := g.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return GoogleIdentity{}, fmt.Errorf("exchange code: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return GoogleIdentity{}, errors.New("google response has no id_token")
	}
	idToken, err := g.verifier.Verify(ctx, rawID)
	if err != nil {
		return GoogleIdentity{}, fmt.Errorf("verify id_token: %w", err)
	}
	var id GoogleIdentity
	if err := idToken.Claims(&id); err != nil {
		return GoogleIdentity{}, fmt.Errorf("decode id_token claims: %w", err)
	}
	return id, nil
}
