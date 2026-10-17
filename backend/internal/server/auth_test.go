package server_test

// Integration tests: the real router and database, with a fake Google OIDC
// provider. Requires TEST_DATABASE_URL pointing at a disposable Postgres; the
// public schema is wiped by setup, at the start of every test.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"labyrinth/internal/auth"
	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/server"
	"labyrinth/internal/user"
)

const (
	// clientID is the fake Google client ID.
	clientID = "test-client.apps.googleusercontent.com"
	// frontendURL is where the backend sends the browser after sign-in.
	frontendURL = "http://frontend.test"
)

// env is a running backend on a fresh database with a fake Google.
type env struct {
	t        *testing.T
	db       *postgres.DB
	app      *server.App
	users    *user.Users
	dbURL    string
	backend  *httptest.Server
	client   *http.Client
	tokens   *auth.Tokens
	identity map[string]any // claims the fake Google puts in the next id_token

	cfg       *config.Config
	endpoints auth.GoogleEndpoints
	log       *slog.Logger
	handler   atomic.Pointer[http.Handler] // swapped by restart
}

// setup starts a backend against TEST_DATABASE_URL, wiping its public schema.
// It skips the test when TEST_DATABASE_URL is unset.
func setup(t *testing.T, allowedDomains ...string) *env {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	for range 2 { // second run must be a no-op
		if err := db.Migrate(ctx, log); err != nil {
			t.Fatal(err)
		}
	}

	e := &env{t: t, db: db, dbURL: dbURL, identity: map[string]any{
		"sub": "google-sub-1", "email": "player@snu.edu.in", "email_verified": true,
		"name": "Player One", "picture": "https://example.com/p.png",
	}}

	google := e.fakeGoogle()
	e.backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*e.handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(e.backend.Close)

	publicURL, _ := url.Parse(e.backend.URL)
	feURL, _ := url.Parse(frontendURL)
	_, jwtKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		JWTKey: jwtKey, Settings: configtest.Settings(t),
		GoogleClientID: clientID, GoogleClientSecret: "secret",
		PublicURL: publicURL, FrontendURL: feURL, AllowedEmailDomains: allowedDomains,
	}
	e.cfg, e.log = cfg, log
	e.endpoints = auth.GoogleEndpoints{
		OAuth:   oauth2.Endpoint{AuthURL: google.URL + "/auth", TokenURL: google.URL + "/token", AuthStyle: oauth2.AuthStyleInParams},
		Issuer:  google.URL,
		JWKSURL: google.URL + "/jwks",
	}
	e.users = user.New(db)
	e.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e.restart()
}

// restart builds a fresh app on the same database and serves it at the same
// URL, as a backend restart would. Signed tokens stay valid: the key is
// unchanged.
func (e *env) restart() *env {
	e.t.Helper()
	app, err := server.Build(context.Background(), e.cfg, e.db, e.endpoints, e.log)
	if err != nil {
		e.t.Fatal(err)
	}
	if e.app != nil {
		e.app.Hub.Close() // end the old app's streams, as a real restart would
	}
	e.app, e.tokens = app, app.Tokens
	h := app.Handler
	e.handler.Store(&h)
	return e
}

// fakeGoogle serves a token endpoint that returns an RS256 id_token built from
// e.identity, and the matching JWKS.
func (e *env) fakeGoogle() *httptest.Server {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		e.t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
	if err != nil {
		e.t.Fatal(err)
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jwks":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			r.ParseForm()
			if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			claims := map[string]any{"iss": srv.URL, "aud": clientID, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
			for k, v := range e.identity {
				claims[k] = v
			}
			payload, _ := json.Marshal(claims)
			jws, _ := signer.Sign(payload)
			idToken, _ := jws.CompactSerialize()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "x", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
		default:
			http.NotFound(w, r)
		}
	}))
	e.t.Cleanup(srv.Close)
	return srv
}

// do sends req without following redirects.
func (e *env) do(req *http.Request) *http.Response {
	e.t.Helper()
	res, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { res.Body.Close() })
	return res
}

// get sends a GET to path with cookies.
func (e *env) get(path string, cookies ...*http.Cookie) *http.Response {
	req, _ := http.NewRequest("GET", e.backend.URL+path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return e.do(req)
}

// login runs /auth/google/login and returns the state and the state cookie.
func (e *env) login(next string) (string, *http.Cookie) {
	e.t.Helper()
	res := e.get("/auth/google/login?next=" + url.QueryEscape(next))
	if res.StatusCode != http.StatusFound {
		e.t.Fatalf("login status %d", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		e.t.Fatalf("missing PKCE in %s", loc)
	}
	if q.Get("client_id") != clientID || q.Get("redirect_uri") != e.backend.URL+"/auth/google/callback" {
		e.t.Fatalf("bad auth URL %s", loc)
	}
	cookies := res.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly {
		e.t.Fatalf("expected one httpOnly state cookie, got %+v", cookies)
	}
	return q.Get("state"), cookies[0]
}

// callback hits /auth/google/callback and returns the frontend redirect.
func (e *env) callback(query url.Values, cookie *http.Cookie) *url.URL {
	e.t.Helper()
	var res *http.Response
	if cookie != nil {
		res = e.get("/auth/google/callback?"+query.Encode(), cookie)
	} else {
		res = e.get("/auth/google/callback?" + query.Encode())
	}
	if res.StatusCode != http.StatusFound {
		e.t.Fatalf("callback status %d", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), frontendURL+"/auth/") {
		e.t.Fatalf("callback redirected outside frontend: %s", loc)
	}
	return loc
}

// redeem posts a login code to /auth/token.
func (e *env) redeem(code string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.backend.URL+"/auth/token", strings.NewReader(`{"code":"`+code+`"}`))
	req.Header.Set("Content-Type", "application/json")
	res := e.do(req)
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// me calls GET /api/me with token.
func (e *env) me(token string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.backend.URL+"/api/me", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res := e.do(req)
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// TestFullLoginFlow walks sign-in end to end: login, Google callback, code
// redemption, /api/me, a role change, and signing in again.
func TestFullLoginFlow(t *testing.T) {
	e := setup(t)

	state, cookie := e.login("/team")
	loc := e.callback(url.Values{"state": {state}, "code": {"good-code"}}, cookie)
	if loc.Path != "/auth/callback" || loc.Query().Get("next") != "/team" || loc.Query().Get("code") == "" {
		t.Fatalf("unexpected frontend redirect %s", loc)
	}
	code := loc.Query().Get("code")

	status, body := e.redeem(code)
	if status != http.StatusOK {
		t.Fatalf("token status %d: %v", status, body)
	}
	payload := body["payload"].(map[string]any)
	token := payload["access_token"].(string)
	if payload["token_type"] != "Bearer" || payload["user"].(map[string]any)["email"] != "player@snu.edu.in" {
		t.Fatalf("unexpected token payload %v", payload)
	}

	if status, _ := e.redeem(code); status != http.StatusBadRequest {
		t.Fatalf("code reuse: status %d, want 400", status)
	}

	status, body = e.me(token)
	user := body["payload"].(map[string]any)
	if status != http.StatusOK || user["role"] != "PLAYER" || user["name"] != "Player One" {
		t.Fatalf("/api/me %d %v", status, body)
	}

	// Role changes show up on /api/me without a new token.
	conn, _ := pgx.Connect(context.Background(), e.dbURL)
	conn.Exec(context.Background(), "UPDATE userprofile SET role = 'ADMIN'")
	conn.Close(context.Background())
	if _, body := e.me(token); body["payload"].(map[string]any)["role"] != "ADMIN" {
		t.Fatalf("role change not reflected: %v", body)
	}

	// Logging in again updates the profile but keeps the same user and role.
	e.identity["name"] = "Renamed"
	state, cookie = e.login("/")
	loc = e.callback(url.Values{"state": {state}, "code": {"good-code"}}, cookie)
	_, body = e.redeem(loc.Query().Get("code"))
	again := body["payload"].(map[string]any)["user"].(map[string]any)
	if again["id"] != user["id"] || again["name"] != "Renamed" || again["role"] != "ADMIN" {
		t.Fatalf("second login: %v (first id %v)", again, user["id"])
	}
}

// TestCallbackFailures checks each way the Google callback can fail and that
// each lands on the frontend's error page with the right reason.
func TestCallbackFailures(t *testing.T) {
	e := setup(t, "snu.edu.in")

	reason := func(loc *url.URL) string {
		if loc.Path != "/auth/error" {
			t.Fatalf("expected /auth/error, got %s", loc)
		}
		return loc.Query().Get("reason")
	}

	t.Run("no state cookie", func(t *testing.T) {
		state, _ := e.login("/")
		if r := reason(e.callback(url.Values{"state": {state}, "code": {"good-code"}}, nil)); r != "state_missing" {
			t.Fatal(r)
		}
	})
	t.Run("state mismatch", func(t *testing.T) {
		_, cookie := e.login("/")
		if r := reason(e.callback(url.Values{"state": {"forged"}, "code": {"good-code"}}, cookie)); r != "state_mismatch" {
			t.Fatal(r)
		}
	})
	t.Run("tampered cookie", func(t *testing.T) {
		state, cookie := e.login("/")
		cookie.Value += "x"
		if r := reason(e.callback(url.Values{"state": {state}, "code": {"good-code"}}, cookie)); r != "state_invalid" {
			t.Fatal(r)
		}
	})
	t.Run("user cancelled", func(t *testing.T) {
		state, cookie := e.login("/")
		if r := reason(e.callback(url.Values{"state": {state}, "error": {"access_denied"}}, cookie)); r != "google_access_denied" {
			t.Fatal(r)
		}
	})
	t.Run("bad code", func(t *testing.T) {
		state, cookie := e.login("/")
		if r := reason(e.callback(url.Values{"state": {state}, "code": {"bad"}}, cookie)); r != "google_exchange_failed" {
			t.Fatal(r)
		}
	})
	t.Run("unverified email", func(t *testing.T) {
		e.identity["email_verified"] = false
		defer func() { e.identity["email_verified"] = true }()
		state, cookie := e.login("/")
		if r := reason(e.callback(url.Values{"state": {state}, "code": {"good-code"}}, cookie)); r != "email_unverified" {
			t.Fatal(r)
		}
	})
	t.Run("domain not allowed", func(t *testing.T) {
		e.identity["email"] = "someone@gmail.com"
		defer func() { e.identity["email"] = "player@snu.edu.in" }()
		state, cookie := e.login("/")
		if r := reason(e.callback(url.Values{"state": {state}, "code": {"good-code"}}, cookie)); r != "email_domain_not_allowed" {
			t.Fatal(r)
		}
	})
	t.Run("open redirect in next", func(t *testing.T) {
		state, cookie := e.login("//evil.com")
		loc := e.callback(url.Values{"state": {state}, "code": {"good-code"}}, cookie)
		if loc.Query().Get("next") != "/" {
			t.Fatalf("next = %q", loc.Query().Get("next"))
		}
	})
}

// TestTokenAndAuthErrors checks expired and unknown login codes, the expired
// code sweep, missing and bad tokens, /healthz, and wrong methods.
func TestTokenAndAuthErrors(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	u, err := e.users.SignInWithGoogle(ctx, user.GoogleProfile{Subject: "s", Email: "x@snu.edu.in", Name: "X"})
	if err != nil {
		t.Fatal(err)
	}
	// Login codes always get the real TTL, so plant an expired one directly.
	hash := sha256.Sum256([]byte("expired-code"))
	if _, err := e.db.Pool().Exec(ctx,
		"INSERT INTO auth_code (code_hash, user_id, expires_at) VALUES ($1, $2, now() - interval '1 second')",
		hash[:], u.ID); err != nil {
		t.Fatal(err)
	}
	code := func(body map[string]any) any { return body["code"] }
	if status, body := e.redeem("expired-code"); status != http.StatusBadRequest || code(body) != "invalid_code" {
		t.Fatalf("expired code: %d %v", status, body)
	}
	if status, body := e.redeem("never-issued"); status != http.StatusBadRequest || code(body) != "invalid_code" {
		t.Fatalf("unknown code: %d %v", status, body)
	}
	if status, body := e.redeem(""); status != http.StatusBadRequest || code(body) != "invalid_body" {
		t.Fatalf("empty code: %d %v", status, body)
	}
	if n, err := e.app.LoginCodes.DeleteExpired(ctx); err != nil || n != 1 {
		t.Fatalf("cleanup removed %d (%v), want 1", n, err)
	}

	if status, body := e.me(""); status != http.StatusUnauthorized || code(body) != "missing_token" {
		t.Fatalf("no token: %d %v", status, body)
	}
	if status, body := e.me("not-a-jwt"); status != http.StatusUnauthorized || code(body) != "invalid_token" {
		t.Fatalf("garbage token: %d %v", status, body)
	}

	if res := e.get("/healthz"); res.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", res.StatusCode)
	}
	req, _ := http.NewRequest("POST", e.backend.URL+"/api/me", nil)
	if res := e.do(req); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/me: %d, want 405", res.StatusCode)
	}
}

// signIn runs the whole Google flow for e.identity and returns the token.
func (e *env) signIn() string {
	e.t.Helper()
	state, cookie := e.login("/")
	loc := e.callback(url.Values{"state": {state}, "code": {"good-code"}}, cookie)
	status, body := e.redeem(loc.Query().Get("code"))
	if status != http.StatusOK {
		e.t.Fatalf("sign in: %d %v (redirect %s)", status, body, loc)
	}
	return body["payload"].(map[string]any)["access_token"].(string)
}

// TestSignInRefreshesTeamProfile checks that signing in again with a new name
// shows up in the team at once, and that a sign-in without a picture keeps
// the avatar.
func TestSignInRefreshesTeamProfile(t *testing.T) {
	e := setup(t)
	p := player{token: e.signIn()}
	expect(t, "create", e.call("POST", "/api/teams", p, map[string]string{"name": "Renamers"}), http.StatusCreated, "")

	e.identity["name"] = "New Name"
	delete(e.identity, "picture")
	e.signIn()
	r := e.call("GET", "/api/teams/me", p, nil)
	m := r.payload["members"].([]any)[0].(map[string]any)
	if m["name"] != "New Name" || m["avatar_url"] != "https://example.com/p.png" {
		t.Fatalf("member after re-sign-in: %v", m)
	}
}

// TestEmailInUse checks that signing in with an email another account
// already has fails with reason email_in_use, not internal_error.
func TestEmailInUse(t *testing.T) {
	e := setup(t)
	if _, err := e.users.SignInWithGoogle(context.Background(), user.GoogleProfile{
		Subject: "someone-else", Email: "player@snu.edu.in", Name: "Other"}); err != nil {
		t.Fatal(err)
	}
	state, cookie := e.login("/")
	loc := e.callback(url.Values{"state": {state}, "code": {"good-code"}}, cookie)
	if loc.Path != "/auth/error" || loc.Query().Get("reason") != "email_in_use" {
		t.Fatalf("redirect %s", loc)
	}
}
