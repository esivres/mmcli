package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/esivres/mmcli/internal/secrets"
)

// wsScript is what the fake server sends on the n-th connection of a token:
// events after hello, then the connection is closed unless hold is set.
type wsScript struct {
	helloID  string
	helloSeq int64
	events   []map[string]any
	hold     bool
}

type wsServer struct {
	t       *testing.T
	mu      sync.Mutex
	users   map[string]string // token -> user id
	scripts map[string][]wsScript
	conns   map[string]int
	queries map[string][]string
}

func newWSServer(t *testing.T) (*wsServer, *httptest.Server) {
	ws := &wsServer{t: t, users: map[string]string{"work-tok": "usov1", "bot-tok": "bot1"},
		scripts: map[string][]wsScript{}, conns: map[string]int{}, queries: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(ws.handle))
	t.Cleanup(srv.Close)
	return ws, srv
}

func (ws *wsServer) handle(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	uid, ok := ws.users[tok]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/api/v4/users/me":
		fmt.Fprintf(w, `{"id":%q,"username":%q}`, uid, strings.TrimSuffix(uid, "1"))
	case r.URL.Path == "/api/v4/users/ids":
		_, _ = w.Write([]byte(`[{"id":"alice1","username":"alice"}]`))
	case strings.HasPrefix(r.URL.Path, "/api/v4/channels/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v4/channels/")
		fmt.Fprintf(w, `{"id":%q,"name":%q,"type":"O"}`, id, "name-"+id)
	case r.URL.Path == "/api/v4/websocket":
		ws.mu.Lock()
		n := ws.conns[tok]
		ws.conns[tok]++
		ws.queries[tok] = append(ws.queries[tok], r.URL.RawQuery)
		var sc wsScript
		if n < len(ws.scripts[tok]) {
			sc = ws.scripts[tok][n]
		} else {
			sc = wsScript{helloID: "idle", hold: true}
		}
		ws.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		send := func(v any) {
			b, _ := json.Marshal(v)
			_ = c.Write(ctx, websocket.MessageText, b)
		}
		send(map[string]any{"event": "hello", "seq": sc.helloSeq, "data": map[string]any{"connection_id": sc.helloID}})
		for _, ev := range sc.events {
			send(ev)
		}
		if sc.hold {
			<-ctx.Done()
		}
		_ = c.Close(websocket.StatusGoingAway, "")
	default:
		http.NotFound(w, r)
	}
}

// postedEvent builds a "posted" websocket event as the server sends it.
func postedEvent(seq int64, id, channel string, mentions ...string) map[string]any {
	post, _ := json.Marshal(map[string]any{"id": id, "user_id": "alice1", "channel_id": channel, "message": "hi " + id, "create_at": 1, "update_at": 1})
	m, _ := json.Marshal(mentions)
	return map[string]any{"event": "posted", "seq": seq, "data": map[string]any{
		"post": string(post), "channel_type": "O", "channel_name": "name-" + channel, "mentions": string(m)}}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) lines() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(l), &m)
		out = append(out, m)
	}
	return out
}

// runStream logs in the given contexts, runs stream until until() holds or
// the deadline passes, and returns the emitted lines.
func runStream(t *testing.T, srvURL string, ctxs []string, args []string, until func([]map[string]any) bool) []map[string]any {
	t.Helper()
	oldMin, oldFlush := reconnectMin, flushEvery
	reconnectMin, flushEvery = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { reconnectMin, flushEvery = oldMin, oldFlush })

	d, _, errOut := newDeps(t, secrets.NewMemory(), "")
	for _, name := range ctxs {
		d.stdin = strings.NewReader(name + "-tok\n")
		if code := run(d, []string{"login", "--context", name, "--url", srvURL, "--token-stdin"}); code != 0 {
			t.Fatalf("login %s exit %d: %s", name, code, errOut)
		}
	}
	out := &syncBuffer{}
	d.stdout = out
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	done := make(chan int)
	go func() { done <- run(d, append([]string{"stream", "--merge-window", "150ms"}, args...)) }()
	deadline := time.Now().Add(5 * time.Second)
	for !until(out.lines()) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("stream exit %d: %s", code, errOut)
	}
	return out.lines()
}

func posts(lines []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, l := range lines {
		if l["event"] == "posted" {
			out[l["id"].(string)] = l
		}
	}
	return out
}

func strs(v any) string {
	var parts []string
	for _, x := range asSlice(v) {
		parts = append(parts, x.(string))
	}
	return strings.Join(parts, ",")
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

// One post seen by both contexts must come out once, naming both, so the
// consumer knows the bot may answer there; a post only the bot sees must not
// claim the user can.
func TestStreamMergesContexts(t *testing.T) {
	ws, srv := newWSServer(t)
	ws.scripts["work-tok"] = []wsScript{{helloID: "w", hold: true, events: []map[string]any{postedEvent(1, "p1", "c1", "bot1")}}}
	ws.scripts["bot-tok"] = []wsScript{{helloID: "b", hold: true, events: []map[string]any{
		postedEvent(1, "p1", "c1", "bot1"), postedEvent(2, "p2", "c2")}}}

	lines := runStream(t, srv.URL, []string{"work", "bot"}, []string{"--context", "work", "--context", "bot"},
		func(l []map[string]any) bool { return len(posts(l)) == 2 })
	got := posts(lines)
	if n := strings.Count(fmt.Sprint(lines), "id:p1"); n != 1 {
		t.Fatalf("p1 emitted %d times: %v", n, lines)
	}
	if c := strs(got["p1"]["contexts"]); c != "work,bot" {
		t.Errorf("p1 contexts %q, want work,bot", c)
	}
	if m := strs(got["p1"]["mentions"]); m != "bot" {
		t.Errorf("p1 mentions %q, want bot", m)
	}
	if c := strs(got["p2"]["contexts"]); c != "bot" {
		t.Errorf("p2 contexts %q, want bot", c)
	}
	if got["p1"]["channel"] != "name-c1" {
		t.Errorf("p1 channel label %v", got["p1"]["channel"])
	}
}

// --mention keeps a post if any context is mentioned, and still lists every
// context that can see it.
func TestStreamMentionFilter(t *testing.T) {
	ws, srv := newWSServer(t)
	ws.scripts["work-tok"] = []wsScript{{helloID: "w", hold: true, events: []map[string]any{
		postedEvent(1, "p1", "c1", "bot1"), postedEvent(2, "p3", "c1")}}}
	ws.scripts["bot-tok"] = []wsScript{{helloID: "b", hold: true, events: []map[string]any{postedEvent(1, "p1", "c1", "bot1")}}}

	// Keep streaming well past the merge window so a wrongly passing p3 shows up.
	var firstSeen time.Time
	lines := runStream(t, srv.URL, []string{"work", "bot"}, []string{"--context", "work", "--context", "bot", "--mention"},
		func(l []map[string]any) bool {
			if len(posts(l)) == 0 {
				return false
			}
			if firstSeen.IsZero() {
				firstSeen = time.Now()
			}
			return time.Since(firstSeen) > 500*time.Millisecond
		})
	got := posts(lines)
	if _, ok := got["p3"]; ok {
		t.Fatalf("p3 mentions nobody but was emitted")
	}
	if c := strs(got["p1"]["contexts"]); c != "work,bot" {
		t.Errorf("p1 contexts %q, want work,bot", c)
	}
}

// A dropped connection must be resumed from the next sequence number without
// a gap; if the server cannot resume, the loss must be reported.
func TestStreamResumeAndGap(t *testing.T) {
	ws, srv := newWSServer(t)
	ws.scripts["bot-tok"] = []wsScript{
		{helloID: "c1", events: []map[string]any{postedEvent(1, "p1", "c1")}},
		{helloID: "c1", helloSeq: 2, events: []map[string]any{postedEvent(3, "p2", "c1")}},
		{helloID: "c2", hold: true},
	}
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot"}, func(l []map[string]any) bool {
		return len(posts(l)) == 2 && strings.Count(fmt.Sprint(l), "event:gap") >= 1
	})
	ws.mu.Lock()
	q := ws.queries["bot-tok"]
	ws.mu.Unlock()
	if len(q) < 3 || q[0] != "" || q[1] != "connection_id=c1&sequence_number=2" || q[2] != "connection_id=c1&sequence_number=4" {
		t.Fatalf("reconnect queries %q", q)
	}
	var gaps int
	for _, l := range lines {
		if l["event"] == "gap" {
			gaps++
		}
	}
	if gaps != 1 {
		t.Fatalf("want exactly one gap (the unresumable reconnect), got %d: %v", gaps, lines)
	}
	if len(posts(lines)) != 2 {
		t.Fatalf("posts %v", lines)
	}
}
