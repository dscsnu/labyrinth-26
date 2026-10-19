package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"labyrinth/internal/auth"
	"labyrinth/internal/user"
)

// player is a virtual player with a signed token.
type player struct {
	id    uuid.UUID
	token string
}

// createPlayers inserts n users straight into the database and signs a token
// for each with the backend's key, so no Google login is involved. Users
// aren't part of the backend's in-memory state, so inserting them while it
// runs is safe.
func createPlayers(ctx context.Context, dbURL string, key ed25519.PrivateKey, tokenTTL time.Duration, n int) ([]player, error) {
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)

	tokens := auth.NewTokens(key, tokenTTL)
	players := make([]player, n)
	rows := make([][]any, n)
	for i := range players {
		u := user.User{
			ID:    uuid.New(),
			Email: fmt.Sprintf("bench%05d@bench.local", i),
			Name:  fmt.Sprintf("Bench %05d", i),
			Role:  user.RolePlayer,
		}
		tok, _, err := tokens.IssueAccess(u)
		if err != nil {
			return nil, err
		}
		players[i] = player{id: u.ID, token: tok}
		rows[i] = []any{u.ID, "bench-" + u.ID.String(), u.Email, u.Name}
	}
	_, err = conn.CopyFrom(ctx, pgx.Identifier{"userprofile"},
		[]string{"id", "google_sub", "email", "name"}, pgx.CopyFromRows(rows))
	return players, err
}

// client issues API calls and records each one's latency and errors.
type client struct {
	base string
	http *http.Client // requests
	sse  *http.Client // streams: no overall timeout
	rec  *recorder
}

// Client limits. Generous, so the load generator is never the bottleneck;
// tune the backend through config.toml and the flags, not these.
const (
	// idleConnsPerHost keeps a connection per virtual player alive.
	idleConnsPerHost = 20_000
	// idleConnTimeout closes keep-alive connections idle this long.
	idleConnTimeout = 90 * time.Second
	// requestTimeout fails a request that takes this long.
	requestTimeout = 30 * time.Second
	// firstEventTimeout fails a stream whose first event takes this long.
	firstEventTimeout = 30 * time.Second
)

// newClient returns a client for base that records into rec.
func newClient(base string, rec *recorder) *client {
	tr := func() *http.Transport {
		return &http.Transport{
			MaxIdleConns:        0, // unlimited
			MaxIdleConnsPerHost: idleConnsPerHost,
			IdleConnTimeout:     idleConnTimeout,
			DisableCompression:  true,
		}
	}
	return &client{
		base: base,
		http: &http.Client{Transport: tr(), Timeout: requestTimeout},
		sse:  &http.Client{Transport: tr()},
		rec:  rec,
	}
}

// envelope is the backend's response envelope.
type envelope struct {
	Success bool            `json:"success"`
	Code    string          `json:"code"`
	Payload json.RawMessage `json:"payload"`
}

// call performs one request. ok is false on a transport error or non-2xx
// status, which is recorded as an error for op.
func (c *client) call(op, method, path string, p player, body any) (env envelope, ok bool) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+p.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	res, err := c.http.Do(req)
	if err != nil {
		c.rec.fail(op, transportReason(err))
		return env, false
	}
	data, err := io.ReadAll(res.Body)
	res.Body.Close()
	elapsed := time.Since(start)
	if err != nil {
		c.rec.fail(op, transportReason(err))
		return env, false
	}
	_ = json.Unmarshal(data, &env)
	if res.StatusCode >= 300 {
		c.rec.fail(op, fmt.Sprintf("%d %s", res.StatusCode, env.Code))
		return env, false
	}
	c.rec.observe(op, elapsed)
	return env, true
}

// transportReason shortens a transport error to a groupable reason.
func transportReason(err error) string {
	s := err.Error()
	for _, k := range []string{"connection reset", "connection refused", "timeout", "EOF", "too many open files"} {
		if strings.Contains(s, k) {
			return k
		}
	}
	return "transport error"
}

// teamView is the part of a team event the load test inspects.
type teamView struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Members []struct {
		ID      uuid.UUID `json:"id"`
		IsReady bool      `json:"is_ready"`
	} `json:"members"`
}

// fanout tracks when actions were sent, so streams can measure how long each
// took to reach teammates.
type fanout struct {
	mu        sync.Mutex
	readySent map[uuid.UUID]time.Time // member → when their ready request was sent
	startSent map[string]time.Time    // team → when the leader's start was sent
}

// newFanout returns an empty fanout.
func newFanout() *fanout {
	return &fanout{readySent: map[uuid.UUID]time.Time{}, startSent: map[string]time.Time{}}
}

// markReady records that member id's ready request is being sent now.
func (f *fanout) markReady(id uuid.UUID) {
	f.mu.Lock()
	f.readySent[id] = time.Now()
	f.mu.Unlock()
}

// markStart records that team teamID's start request is being sent now.
func (f *fanout) markStart(teamID string) {
	f.mu.Lock()
	f.startSent[teamID] = time.Now()
	f.mu.Unlock()
}

// stream is one player's open event stream.
type stream struct {
	cancel  context.CancelFunc
	started chan struct{} // closed when this stream sees its team IN_GAME
	closed  chan struct{} // closed when the stream ends
}

// openStream connects p's event stream and, in the background, records
// delivery latency for teammates' ready-ups and the leader's start.
func (c *client) openStream(p player, f *fanout) (*stream, error) {
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+p.token)
	res, err := c.sse.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		cancel()
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}

	s := &stream{cancel: cancel, started: make(chan struct{}), closed: make(chan struct{})}
	first := make(chan struct{})
	go func() {
		defer close(s.closed)
		defer res.Body.Close()
		seenReady := map[uuid.UUID]bool{}
		sawStart := false
		gotFirst := false
		sc := bufio.NewScanner(res.Body)
		var name string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				now := time.Now()
				if !gotFirst {
					gotFirst = true
					close(first)
				}
				if name != "team" {
					continue
				}
				var t teamView
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &t) != nil {
					continue
				}
				f.mu.Lock()
				for _, m := range t.Members {
					if m.IsReady && !seenReady[m.ID] {
						seenReady[m.ID] = true
						if sent, ok := f.readySent[m.ID]; ok {
							c.rec.observe("sse ready → teammates", now.Sub(sent))
						}
					}
				}
				if t.Status == "IN_GAME" && !sawStart {
					sawStart = true
					if sent, ok := f.startSent[t.ID]; ok {
						c.rec.observe("sse start → team", now.Sub(sent))
					}
					close(s.started)
				}
				f.mu.Unlock()
			}
		}
	}()

	select {
	case <-first:
		return s, nil
	case <-s.closed:
		return nil, fmt.Errorf("stream closed before first event")
	case <-time.After(firstEventTimeout):
		cancel()
		return nil, fmt.Errorf("no first event within %v", firstEventTimeout)
	}
}
