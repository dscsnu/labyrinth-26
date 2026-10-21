package httpx

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// statusRecorder is a ResponseWriter that remembers the status and counts
// the body bytes, for the access log. It passes Unwrap through, so
// http.ResponseController (the event stream's Flush and write deadlines)
// still reaches the real writer.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

// WriteHeader records the status and passes it on.
func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write counts the bytes (a write before WriteHeader means 200).
func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Unwrap returns the real ResponseWriter, for http.ResponseController.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// AccessLog logs one line per request when it finishes: method, the route
// pattern that matched (or the path, for an unrouted one), status, bytes
// and duration. A long-lived event stream is logged when it closes. Paths
// in skip (e.g. /healthz, polled by monitors) aren't logged.
func AccessLog(log *slog.Logger, next http.Handler, skip ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range skip {
			if r.URL.Path == p {
				next.ServeHTTP(w, r)
				return
			}
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		route := r.Pattern // set by the mux on this request
		if route == "" {
			route = r.Method + " " + r.URL.Path
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		log.Info("request", "route", route, "status", status, "bytes", rec.bytes,
			"ms", time.Since(start).Milliseconds())
	})
}

// Recover turns a panic in next into a logged error with its stack and a
// 500 in the JSON envelope (if nothing was written yet), so one bad request
// can't take the server down or leave the client without an answer.
// http.ErrAbortHandler is re-panicked: net/http uses it to abort a response
// on purpose.
func Recover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			log.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
			if rec.status == 0 {
				Error(rec, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(rec, r)
	})
}
