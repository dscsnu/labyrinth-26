package tests

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// nextQTE returns the next qte event, failing after 3 s.
func (s *stream) nextQTE(t *testing.T, what string) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-s.qtes:
		if !ok {
			t.Fatalf("%s: stream closed", what)
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: no qte within 3s", what)
	}
	return sseEvent{}
}

// quizBody is an admin's quiz: right option 1 of 3, earning powerup.
func quizBody(powerup string, start bool) map[string]any {
	return map[string]any{"kind": "quiz", "powerup": powerup, "start": start,
		"quiz": map[string]any{"question": "Which?", "options": []string{"A", "B", "C"}, "correct_option": 1}}
}

// correctOption returns a qte message's quiz.correct_option (nil while
// open).
func correctOption(msg map[string]any) any { return msg["quiz"].(map[string]any)["correct_option"] }

// TestQTEsOverHTTP walks a quiz through the admin and player routes with
// every error code: drafts, validation, one at a time, a start, answers
// (right, a resend, every refusal), the admin list with the answers counted,
// an early end revealing the answer on every stream (a teamless one too),
// late answers refused, and a 1 s quiz closed by its timer.
func TestQTEsOverHTTP(t *testing.T) {
	e := setup(t)
	admin := e.newAdmin("admin")
	ps := e.startedTeam("qz")
	other := e.startedTeam("qy")[0]
	create := func(body any) apiResult { return e.call("POST", "/api/admin/qtes", admin, body) }
	answer := func(p player, id any, body any) apiResult {
		return e.call("POST", fmt.Sprintf("/api/teams/me/state/qte/%v/answer", id), p, body)
	}
	s := e.openStream(ps[1])
	if ev := s.nextQTE(t, "connect"); ev.typ != "snapshot" || ev.data != nil {
		t.Fatalf("connect with no quiz: %s %v", ev.typ, ev.data)
	}
	loner := e.openStream(e.newPlayer("loner"))
	loner.nextQTE(t, "loner connect")

	expect(t, "a player creates", e.call("POST", "/api/admin/qtes", ps[0], quizBody("heart", false)), http.StatusForbidden, "not_admin")
	expect(t, "not an object", create("quiz"), http.StatusBadRequest, "invalid_body")
	for _, c := range []struct {
		code string
		edit func(b map[string]any)
	}{
		{"invalid_kind", func(b map[string]any) { b["kind"] = "poll" }},
		{"invalid_powerup", func(b map[string]any) { b["powerup"] = "shield" }},
		{"invalid_question", func(b map[string]any) { b["quiz"].(map[string]any)["question"] = "  " }},
		{"invalid_options", func(b map[string]any) { b["quiz"].(map[string]any)["options"] = []string{"A", "A"} }},
		{"invalid_correct_option", func(b map[string]any) { b["quiz"].(map[string]any)["correct_option"] = 3 }},
		{"invalid_window", func(b map[string]any) { b["window_s"] = 0 }},
		{"invalid_window", func(b map[string]any) { b["window_s"] = 301 }},
	} {
		b := quizBody("heart", true)
		c.edit(b)
		expect(t, c.code, create(b), http.StatusBadRequest, c.code)
	}

	r := create(quizBody("heart", false))
	expect(t, "a draft", r, http.StatusOK, "")
	if r.payload["status"] != "draft" || r.payload["started_at"] != nil || r.payload["created_by"] != admin.id {
		t.Fatalf("the draft: %v", r.payload)
	}
	q1 := r.payload["id"]
	if g := e.call("GET", "/api/qte", ps[0], nil); g.status != http.StatusOK || g.payload != nil {
		t.Fatalf("GET /api/qte with only a draft: %d %v", g.status, g.payload)
	}
	expect(t, "answering a draft", answer(ps[0], q1, map[string]int{"option": 1}), http.StatusNotFound, "unknown_qte")
	expect(t, "ending a draft", e.call("POST", fmt.Sprintf("/api/admin/qtes/%v/end", q1), admin, nil), http.StatusConflict, "qte_not_started")
	expect(t, "a bad id", e.call("POST", "/api/admin/qtes/abc/start", admin, nil), http.StatusBadRequest, "invalid_qte_id")
	expect(t, "a bad id to end", e.call("POST", "/api/admin/qtes/0/end", admin, nil), http.StatusBadRequest, "invalid_qte_id")
	expect(t, "no such qte", e.call("POST", "/api/admin/qtes/999999/start", admin, nil), http.StatusNotFound, "unknown_qte")

	r = e.call("POST", fmt.Sprintf("/api/admin/qtes/%v/start", q1), admin, nil)
	expect(t, "start the draft", r, http.StatusOK, "")
	if r.payload["status"] != "open" || correctOption(r.payload) != float64(1) {
		t.Fatalf("started (admins see the answer): %v", r.payload)
	}
	for _, st := range []*stream{s, loner} {
		ev := st.nextQTE(t, "started")
		if ev.typ != "started" || ev.data["id"] != q1 || ev.data["status"] != "open" || correctOption(ev.data) != nil {
			t.Fatalf("started event (no answer while open): %s %v", ev.typ, ev.data)
		}
	}
	if g := e.call("GET", "/api/qte", ps[0], nil); g.payload["id"] != q1 || correctOption(g.payload) != nil {
		t.Fatalf("GET /api/qte while open: %v", g.payload)
	}
	expect(t, "start it again", e.call("POST", fmt.Sprintf("/api/admin/qtes/%v/start", q1), admin, nil), http.StatusConflict, "qte_already_run")
	expect(t, "another at once", create(quizBody("heart", true)), http.StatusConflict, "qte_open")
	r = create(map[string]any{"kind": "quiz", "powerup": "instant_cast", "window_s": 1,
		"quiz": map[string]any{"question": "Fast?", "options": []string{"Yes", "No"}, "correct_option": 0}})
	expect(t, "a second draft, 1 s", r, http.StatusOK, "")
	q2 := r.payload["id"]
	expect(t, "start it while one is open", e.call("POST", fmt.Sprintf("/api/admin/qtes/%v/start", q2), admin, nil), http.StatusConflict, "qte_open")

	expect(t, "a bad qte id", answer(ps[0], "abc", map[string]int{"option": 1}), http.StatusBadRequest, "invalid_qte_id")
	expect(t, "no option", answer(ps[0], q1, map[string]any{}), http.StatusBadRequest, "invalid_body")
	expect(t, "a negative option", answer(ps[0], q1, map[string]int{"option": -1}), http.StatusBadRequest, "invalid_body")
	expect(t, "option 3 of 3", answer(ps[0], q1, map[string]int{"option": 3}), http.StatusBadRequest, "invalid_option")
	expect(t, "no team", answer(e.newPlayer("teamless"), q1, map[string]int{"option": 1}), http.StatusNotFound, "not_in_team")
	s.nextState(t, "connect")
	r = answer(ps[1], q1, map[string]int{"option": 1})
	expect(t, "a member answers right", r, http.StatusOK, "")
	if a := r.payload["qte"].(map[string]any); a["qte_id"] != q1 || a["correct"] != true || a["granted"] != true || a["answered_by"] != ps[1].id {
		t.Fatalf("the answer: %v", a)
	}
	if ev := s.nextState(t, "answer"); ev.typ != "qte_answered" {
		t.Fatalf("answer event: %s", ev.typ)
	}
	expect(t, "the leader resends", answer(ps[0], q1, map[string]int{"option": 0}), http.StatusConflict, "qte_already_answered")

	status, rows := e.callList("GET", "/api/admin/qtes", admin)
	if status != http.StatusOK || len(rows) != 2 {
		t.Fatalf("admin list: %d %v", status, rows)
	}
	first, second := rows[0].(map[string]any), rows[1].(map[string]any)
	stats := second["stats"].(map[string]any)
	if first["id"] != q2 || first["status"] != "draft" || second["id"] != q1 || second["status"] != "open" ||
		stats["answered"] != float64(1) || stats["granted"] != float64(1) || stats["by_option"].(map[string]any)["1"] != float64(1) {
		t.Fatalf("admin list rows: %v / %v", first, second)
	}

	r = e.call("POST", fmt.Sprintf("/api/admin/qtes/%v/end", q1), admin, nil)
	expect(t, "end it early", r, http.StatusOK, "")
	if r.payload["status"] != "closed" || r.payload["stats"].(map[string]any)["answered"] != float64(1) {
		t.Fatalf("ended: %v", r.payload)
	}
	for _, st := range []*stream{s, loner} {
		if ev := st.nextQTE(t, "closed"); ev.typ != "closed" || ev.data["status"] != "closed" || correctOption(ev.data) != float64(1) {
			t.Fatalf("closed event (the reveal): %s %v", ev.typ, ev.data)
		}
	}
	if g := e.call("GET", "/api/qte", ps[0], nil); g.payload != nil {
		t.Fatalf("GET /api/qte after the close: %v", g.payload)
	}
	expect(t, "a late answer", answer(other, q1, map[string]int{"option": 1}), http.StatusConflict, "qte_closed")
	expect(t, "end it again", e.call("POST", fmt.Sprintf("/api/admin/qtes/%v/end", q1), admin, nil), http.StatusConflict, "qte_closed")

	expect(t, "start the 1 s quiz", e.call("POST", fmt.Sprintf("/api/admin/qtes/%v/start", q2), admin, nil), http.StatusOK, "")
	if ev := s.nextQTE(t, "1 s started"); ev.typ != "started" || ev.data["id"] != q2 {
		t.Fatalf("1 s quiz started: %s %v", ev.typ, ev.data)
	}
	if ev := s.nextQTE(t, "1 s closed by its timer"); ev.typ != "closed" || ev.data["id"] != q2 || correctOption(ev.data) != float64(0) {
		t.Fatalf("1 s quiz closed: %s %v", ev.typ, ev.data)
	}
	expect(t, "answering after the timer", answer(ps[0], q2, map[string]int{"option": 0}), http.StatusConflict, "qte_closed")
}
