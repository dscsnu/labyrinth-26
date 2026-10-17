package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/httpx"
	"labyrinth/internal/platform/identity"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/user"
)

const (
	// stateCookie is the name of the OAuth state cookie.
	stateCookie = "labyrinth_oauth_state"
	// stateCookiePath limits the state cookie to the sign-in routes.
	stateCookiePath = "/auth/google"
)

// API serves the sign-in flow and provides the auth middleware.
type API struct {
	cfg    *config.Config
	google *Google
	tokens *Tokens
	codes  *LoginCodes
	users  *user.Users
	log    *slog.Logger
	// onSignIn is told about every successful sign-in, after the profile is
	// saved, so in-memory copies of names and avatars (teams) can refresh.
	onSignIn func(user.User)
}

// NewAPI returns the auth API. onSignIn, if not nil, is called with the user
// after every successful sign-in.
func NewAPI(cfg *config.Config, google *Google, tokens *Tokens, codes *LoginCodes, users *user.Users, onSignIn func(user.User), log *slog.Logger) *API {
	return &API{cfg: cfg, google: google, tokens: tokens, codes: codes, users: users, onSignIn: onSignIn, log: log}
}

// Routes registers the sign-in routes on mux. They're public: they're how a
// user gets a token in the first place.
func (a *API) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/google/login", a.login)
	mux.HandleFunc("GET /auth/google/callback", a.callback)
	mux.HandleFunc("POST /auth/token", a.token)
}

// errorCodes maps rule violations to an HTTP status and a stable code the
// frontend can switch on. Only POST /auth/token answers with these; the
// callback's failures are redirect reasons, not envelope errors, and the
// middleware's missing_token and invalid_token are written by RequireAuth.
var errorCodes = []struct {
	err    error
	status int
	code   string
}{
	{ErrInvalidCode, http.StatusBadRequest, "invalid_code"},
	{errUserGone, http.StatusNotFound, "user_not_found"},
}

// fail writes the response for err: its status and code from errorCodes, or
// a 500 for anything unexpected (which is logged).
func (a *API) fail(w http.ResponseWriter, err error) {
	for _, e := range errorCodes {
		if errors.Is(err, e.err) {
			httpx.ErrorCode(w, e.status, e.code, e.err.Error())
			return
		}
	}
	a.log.Error("redeem login code", "error", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// login handles GET /auth/google/login?next=/path. It sets a signed state
// cookie and redirects to Google with PKCE.
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	state, err := randomToken()
	if err != nil {
		a.log.Error("generate oauth state", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	verifier := oauth2.GenerateVerifier()

	signed, err := a.tokens.issueState(oauthState{
		State:    state,
		Verifier: verifier,
		Next:     SafeNext(r.URL.Query().Get("next")),
	}, a.cfg.Settings.Auth.OAuthStateTTL.Duration)
	if err != nil {
		a.log.Error("sign oauth state", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    signed,
		Path:     stateCookiePath,
		MaxAge:   int(a.cfg.Settings.Auth.OAuthStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   a.cfg.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.google.AuthCodeURL(state, verifier), http.StatusFound)
}

// callback handles GET /auth/google/callback, where Google sends the user
// back. It verifies the state and Google's ID token, signs the user in, and
// redirects to the frontend with a single-use login code, or to
// /auth/error?reason=… on any failure.
func (a *API) callback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(stateCookie)
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Path:     stateCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.cfg.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
	if err != nil {
		a.redirectError(w, r, "state_missing")
		return
	}
	st, err := a.tokens.verifyState(cookie.Value)
	if err != nil {
		a.redirectError(w, r, "state_invalid")
		return
	}

	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		a.redirectError(w, r, "state_mismatch")
		return
	}
	if e := q.Get("error"); e != "" {
		// e.g. access_denied when the user cancels on Google's screen.
		a.redirectError(w, r, "google_"+e)
		return
	}
	code := q.Get("code")
	if code == "" {
		a.redirectError(w, r, "code_missing")
		return
	}

	id, err := a.google.Exchange(r.Context(), code, st.Verifier)
	if err != nil {
		a.log.Error("google exchange", "error", err)
		a.redirectError(w, r, "google_exchange_failed")
		return
	}
	if !id.EmailVerified || id.Email == "" {
		a.redirectError(w, r, "email_unverified")
		return
	}
	if !a.emailAllowed(id.Email) {
		a.redirectError(w, r, "email_domain_not_allowed")
		return
	}

	u, err := a.users.SignInWithGoogle(r.Context(), user.GoogleProfile{
		Subject:   id.Subject,
		Email:     id.Email,
		Name:      id.Name,
		AvatarURL: id.Picture,
	})
	if errors.Is(err, user.ErrEmailInUse) {
		a.redirectError(w, r, "email_in_use")
		return
	}
	if err != nil {
		a.log.Error("sign in user", "error", err)
		a.redirectError(w, r, "internal_error")
		return
	}
	if a.onSignIn != nil {
		a.onSignIn(u)
	}

	loginCode, err := a.codes.Create(r.Context(), u.ID)
	if err != nil {
		a.log.Error("create login code", "error", err)
		a.redirectError(w, r, "internal_error")
		return
	}

	a.log.Info("login", "user_id", u.ID)
	a.redirectFrontend(w, r, "/auth/callback", url.Values{"code": {loginCode}, "next": {st.Next}})
}

// tokenResponse is the body of a successful POST /auth/token.
type tokenResponse struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresAt   time.Time `json:"expires_at"`
	User        user.User `json:"user"`
}

// token handles POST /auth/token {"code"}: the SvelteKit server redeems a
// login code for an access token, server to server.
func (a *API) token(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil || req.Code == "" {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_body", "body must be {\"code\": string}")
		return
	}

	// The code is used up only if the user loads and the token signs, so a
	// failure here doesn't force a fresh Google sign-in.
	var res tokenResponse
	err := a.codes.Redeem(r.Context(), req.Code, func(userID uuid.UUID) error {
		u, err := a.users.Get(r.Context(), userID)
		if errors.Is(err, postgres.ErrNotFound) {
			return errUserGone // distinct from the code's own ErrInvalidCode
		}
		if err != nil {
			return fmt.Errorf("load user: %w", err)
		}
		tok, exp, err := a.tokens.IssueAccess(u)
		if err != nil {
			return fmt.Errorf("sign access token: %w", err)
		}
		res = tokenResponse{AccessToken: tok, TokenType: "Bearer", ExpiresAt: exp, User: u}
		return nil
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, res)
}

// RequireAuth rejects requests without a valid "Authorization: Bearer <jwt>"
// and puts the caller's identity on the request for other use cases.
func (a *API) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.ErrorCode(w, http.StatusUnauthorized, "missing_token", "missing bearer token")
			return
		}
		claims, err := a.tokens.VerifyAccess(raw)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			httpx.ErrorCode(w, http.StatusUnauthorized, "invalid_token", "invalid or expired token")
			return
		}
		userID, _ := claims.UserID() // VerifyAccess checked it parses
		next.ServeHTTP(w, r.WithContext(identity.With(r.Context(), identity.Identity{
			UserID: userID,
			Email:  claims.Email,
			Name:   claims.Name,
			Role:   string(claims.Role),
		})))
	})
}

// Authed adapts RequireAuth for route registration.
func (a *API) Authed(h http.HandlerFunc) http.Handler { return a.RequireAuth(h) }

// emailAllowed reports whether email's domain is in ALLOWED_EMAIL_DOMAINS, or
// true when that list is empty.
func (a *API) emailAllowed(email string) bool {
	if len(a.cfg.AllowedEmailDomains) == 0 {
		return true
	}
	_, domain, _ := strings.Cut(strings.ToLower(email), "@")
	for _, d := range a.cfg.AllowedEmailDomains {
		if domain == d {
			return true
		}
	}
	return false
}

// redirectError sends the browser to the frontend's error page with reason.
func (a *API) redirectError(w http.ResponseWriter, r *http.Request, reason string) {
	a.log.Warn("login failed", "reason", reason)
	a.redirectFrontend(w, r, "/auth/error", url.Values{"reason": {reason}})
}

// redirectFrontend sends the browser to path on the frontend with query q.
func (a *API) redirectFrontend(w http.ResponseWriter, r *http.Request, path string, q url.Values) {
	u := *a.cfg.FrontendURL
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// SafeNext returns next if it's a same-site path like "/team?x=1", else "/".
// Rejects "//host", "/\host" and absolute URLs to prevent open redirects.
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n\t") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}
