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
	noHello  bool // a successful resume: the server sends no hello
	delay    time.Duration
	events   []map[string]any
	hold     bool
	deaf     bool // never reads, so pings go unanswered
	revoke   bool // the token stops working once this connection closes
}

type wsServer struct {
	t            *testing.T
	channelDelay time.Duration // slow REST lookups
	dialHang     bool          // never answer the websocket upgrade
	mu           sync.Mutex
	users        map[string]string // token -> user id
	scripts      map[string][]wsScript
	conns        map[string]int
	queries      map[string][]string
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
	ws.mu.Lock()
	uid, ok := ws.users[tok]
	ws.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"id":"api.context.session_expired.app_error","message":"Invalid or expired session","status_code":401}`))
		return
	}
	switch {
	case r.URL.Path == "/api/v4/users/me":
		fmt.Fprintf(w, `{"id":%q,"username":%q}`, uid, strings.TrimSuffix(uid, "1"))
	case r.URL.Path == "/api/v4/users/ids":
		_, _ = w.Write([]byte(`[{"id":"alice1","username":"alice"}]`))
	case strings.HasPrefix(r.URL.Path, "/api/v4/channels/"):
		time.Sleep(ws.channelDelay)
		id := strings.TrimPrefix(r.URL.Path, "/api/v4/channels/")
		fmt.Fprintf(w, `{"id":%q,"name":%q,"type":"O"}`, id, "name-"+id)
	case r.URL.Path == "/api/v4/websocket":
		if ws.dialHang {
			<-r.Context().Done()
			return
		}
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
		if !sc.deaf {
			ctx = c.CloseRead(ctx)
		}
		send := func(v any) {
			b, _ := json.Marshal(v)
			_ = c.Write(ctx, websocket.MessageText, b)
		}
		if !sc.noHello {
			send(map[string]any{"event": "hello", "seq": sc.helloSeq, "data": map[string]any{"connection_id": sc.helloID}})
		}
		time.Sleep(sc.delay)
		for _, ev := range sc.events {
			send(ev)
		}
		if sc.hold {
			<-ctx.Done()
		}
		if sc.revoke {
			ws.mu.Lock()
			delete(ws.users, tok)
			ws.mu.Unlock()
		}
		_ = c.Close(websocket.StatusGoingAway, "")
	default:
		http.NotFound(w, r)
	}
}

// deletedEvent builds a "post_deleted" event: the server sends only the post.
func deletedEvent(seq int64, id, channel string) map[string]any {
	post, _ := json.Marshal(map[string]any{"id": id, "user_id": "alice1", "channel_id": channel, "message": "", "create_at": 1, "update_at": 3})
	return map[string]any{"event": "post_deleted", "seq": seq, "data": map[string]any{"post": string(post)}}
}

// editedEvent builds a "post_edited" event: the server sends only the post.
func editedEvent(seq int64, id, channel string) map[string]any {
	post, _ := json.Marshal(map[string]any{"id": id, "user_id": "alice1", "channel_id": channel, "message": "edited", "create_at": 1, "update_at": 2})
	return map[string]any{"event": "post_edited", "seq": seq, "data": map[string]any{"post": string(post)}}
}

// renamedPost is a posted event whose channel was renamed after the REST
// lookup would have cached it.
func renamedPost(seq int64, id, channel, newName string) map[string]any {
	ev := postedEvent(seq, id, channel)
	ev["data"].(map[string]any)["channel_name"] = newName
	return ev
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
	lines, code, stderr := runStreamExit(t, srvURL, ctxs, args, until, nil)
	if code != 0 {
		t.Fatalf("stream exit %d: %s", code, stderr)
	}
	return lines
}

// runStreamExit is runStream that also reports the exit code and stderr;
// beforeStream runs after login.
func runStreamExit(t *testing.T, srvURL string, ctxs []string, args []string, until func([]map[string]any) bool, beforeStream func()) ([]map[string]any, int, string) {
	t.Helper()
	oldMin, oldFlush, oldGrace := reconnectMin, flushEvery, resumeGrace
	reconnectMin, flushEvery, resumeGrace = 10*time.Millisecond, 10*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { reconnectMin, flushEvery, resumeGrace = oldMin, oldFlush, oldGrace })

	d, _, errOut := newDeps(t, secrets.NewMemory(), "")
	for _, name := range ctxs {
		d.stdin = strings.NewReader(name + "-tok\n")
		if code := run(d, []string{"login", "--context", name, "--url", srvURL, "--token-stdin"}); code != 0 {
			t.Fatalf("login %s exit %d: %s", name, code, errOut)
		}
	}
	if beforeStream != nil {
		beforeStream()
	}
	out := &syncBuffer{}
	d.stdout = out
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	done := make(chan int)
	go func() { done <- run(d, append([]string{"stream", "--merge-window", "150ms"}, args...)) }()
	deadline := time.Now().Add(5 * time.Second)
	code := -1
	for code == -1 && !until(out.lines()) && time.Now().Before(deadline) {
		select {
		case code = <-done:
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	if code == -1 {
		code = <-done
	}
	return out.lines(), code, errOut.String()
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

func events(lines []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["event"] == kind {
			out = append(out, l)
		}
	}
	return out
}

func sequence(lines []map[string]any) string {
	var parts []string
	for _, l := range lines {
		if id, ok := l["id"]; ok {
			parts = append(parts, fmt.Sprint(l["event"], ":", id))
		} else {
			parts = append(parts, fmt.Sprint(l["event"]))
		}
	}
	return strings.Join(parts, " ")
}

// A dropped connection must be resumed from the next sequence number; the
// server sends no hello then. If it cannot resume, the loss must be reported.
func TestStreamResumeAndGap(t *testing.T) {
	ws, srv := newWSServer(t)
	ws.scripts["bot-tok"] = []wsScript{
		{helloID: "c1", events: []map[string]any{postedEvent(1, "p1", "c1")}},
		{noHello: true, events: []map[string]any{postedEvent(2, "p2", "c1")}},
		{helloID: "c2", hold: true},
	}
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot"}, func(l []map[string]any) bool {
		return len(events(l, "connected")) == 3
	})
	ws.mu.Lock()
	q := ws.queries["bot-tok"]
	ws.mu.Unlock()
	if len(q) < 3 || q[0] != "" || q[1] != "connection_id=c1&sequence_number=2" || q[2] != "connection_id=c1&sequence_number=3" {
		t.Fatalf("reconnect queries %q", q)
	}
	want := "connected posted:p1 disconnected connected posted:p2 disconnected gap connected"
	if got := sequence(lines); got != want {
		t.Fatalf("lines\n got %s\nwant %s", got, want)
	}
	if d := events(lines, "gap")[0]["detail"]; !strings.Contains(fmt.Sprint(d), "could not resume") {
		t.Fatalf("gap detail %v", d)
	}
}

// Missing sequence numbers inside a live connection are lost events too.
func TestStreamSeqGapInConnection(t *testing.T) {
	ws, srv := newWSServer(t)
	// The replayed p1 must not rewind the sequence and fake a gap before p5.
	ws.scripts["bot-tok"] = []wsScript{{helloID: "c1", hold: true, events: []map[string]any{
		postedEvent(1, "p1", "c1"), postedEvent(4, "p4", "c1"), postedEvent(1, "p1", "c1"), postedEvent(5, "p5", "c1")}}}
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot"}, func(l []map[string]any) bool {
		return len(posts(l)) == 3
	})
	if got, want := sequence(lines), "connected posted:p1 gap posted:p4 posted:p5"; got != want {
		t.Fatalf("lines\n got %s\nwant %s", got, want)
	}
	if d := events(lines, "gap")[0]["detail"]; d != "missed events: 2" {
		t.Fatalf("gap detail %v", d)
	}
}

// A copy arriving after the merge window must not produce a second line.
func TestStreamDropsLateDuplicate(t *testing.T) {
	ws, srv := newWSServer(t)
	ws.scripts["work-tok"] = []wsScript{{helloID: "w", hold: true, events: []map[string]any{postedEvent(1, "p1", "c1")}}}
	ws.scripts["bot-tok"] = []wsScript{{helloID: "b", hold: true, delay: 500 * time.Millisecond,
		events: []map[string]any{postedEvent(1, "p1", "c1"), postedEvent(2, "p2", "c1")}}}
	lines := runStream(t, srv.URL, []string{"work", "bot"}, []string{"--context", "work", "--context", "bot"},
		func(l []map[string]any) bool { return len(posts(l)) == 2 })
	if n := len(events(lines, "posted")); n != 2 {
		t.Fatalf("want p1 once and p2 once, got %s", sequence(lines))
	}
}

// Edits carry no channel data; they must follow the post through filters.
func TestStreamEditFollowsFilteredPost(t *testing.T) {
	ws, srv := newWSServer(t)
	ws.scripts["bot-tok"] = []wsScript{{helloID: "b", hold: true, events: []map[string]any{
		postedEvent(1, "p1", "c1"), postedEvent(2, "p2", "c2"), editedEvent(3, "p1", "c1"), editedEvent(4, "p2", "c2")}}}
	var firstSeen time.Time
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot", "--channel", "name-c1"}, func(l []map[string]any) bool {
		if len(events(l, "post_edited")) == 0 {
			return false
		}
		if firstSeen.IsZero() {
			firstSeen = time.Now()
		}
		return time.Since(firstSeen) > 300*time.Millisecond
	})
	if got, want := sequence(lines), "connected posted:p1 post_edited:p1"; got != want {
		t.Fatalf("lines\n got %s\nwant %s", got, want)
	}
	if ct := events(lines, "post_edited")[0]["channel_type"]; ct != "O" {
		t.Fatalf("edit channel_type %v", ct)
	}
}

// Lines leave in arrival order even when many ripen in one flush.
func TestStreamKeepsArrivalOrder(t *testing.T) {
	ws, srv := newWSServer(t)
	var evs []map[string]any
	var want []string
	for i := 1; i <= 30; i++ {
		id := fmt.Sprintf("p%02d", i)
		evs = append(evs, postedEvent(int64(i), id, "c1"))
		want = append(want, "posted:"+id)
	}
	ws.scripts["bot-tok"] = []wsScript{{helloID: "b", hold: true, events: evs}}
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot"}, func(l []map[string]any) bool {
		return len(posts(l)) == 30
	})
	if got := sequence(lines); got != "connected "+strings.Join(want, " ") {
		t.Fatalf("order broken: %s", got)
	}
}

// A connection that stops answering pings must be dropped, not trusted.
func TestStreamDetectsHalfOpenConnection(t *testing.T) {
	oldEvery, oldTimeout := pingEvery, pingTimeout
	pingEvery, pingTimeout = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { pingEvery, pingTimeout = oldEvery, oldTimeout })
	ws, srv := newWSServer(t)
	ws.scripts["bot-tok"] = []wsScript{{helloID: "b", hold: true, deaf: true}}
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot"}, func(l []map[string]any) bool {
		return len(events(l, "disconnected")) > 0
	})
	d := events(lines, "disconnected")
	if len(d) == 0 || !strings.Contains(fmt.Sprint(d[0]["detail"]), "no pong") {
		t.Fatalf("half-open connection not detected: %s", sequence(lines))
	}
}

// An edit carries no mentions; it must follow a post that passed --mention.
func TestStreamEditFollowsMentionedPost(t *testing.T) {
	ws, srv := newWSServer(t)
	ws.scripts["bot-tok"] = []wsScript{{helloID: "b", hold: true, events: []map[string]any{
		postedEvent(1, "p1", "c1", "bot1"), editedEvent(2, "p1", "c1"), deletedEvent(3, "p1", "c1")}}}
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot", "--mention"}, func(l []map[string]any) bool {
		return len(events(l, "post_deleted")) == 1
	})
	if got, want := sequence(lines), "connected posted:p1 post_edited:p1 post_deleted:p1"; got != want {
		t.Fatalf("lines\n got %s\nwant %s", got, want)
	}
}

func never([]map[string]any) bool { return false }

// A context rejected before it ever connected is a setup error: stop at once.
func TestStreamFailsFastOnRejectedToken(t *testing.T) {
	ws, srv := newWSServer(t)
	revoke := func() { ws.mu.Lock(); delete(ws.users, "bot-tok"); ws.mu.Unlock() }
	_, code, stderr := runStreamExit(t, srv.URL, []string{"work", "bot"}, []string{"--context", "work", "--context", "bot"}, never, revoke)
	if code != 1 || !strings.Contains(stderr, `context "bot"`) {
		t.Fatalf("want exit 1 naming bot, got %d: %s", code, stderr)
	}
}

// Losing one of several contexts is reported and the rest keep streaming;
// losing the last one ends the process with an error.
func TestStreamReportsFailedContext(t *testing.T) {
	ws, srv := newWSServer(t)
	ws.scripts["bot-tok"] = []wsScript{{helloID: "b", revoke: true}}
	ws.scripts["work-tok"] = []wsScript{{helloID: "w", hold: true, delay: 600 * time.Millisecond,
		events: []map[string]any{postedEvent(1, "p1", "c1")}}}
	lines, code, stderr := runStreamExit(t, srv.URL, []string{"work", "bot"}, []string{"--context", "work", "--context", "bot"},
		func(l []map[string]any) bool { return len(posts(l)) == 1 }, nil)
	if code != 0 {
		t.Fatalf("stream with a live context exited %d: %s", code, stderr)
	}
	if f := events(lines, "failed"); len(f) != 1 || f[0]["context"] != "bot" {
		t.Fatalf("want one failed line for bot: %s", sequence(lines))
	}

	ws2, srv2 := newWSServer(t)
	ws2.scripts["bot-tok"] = []wsScript{{helloID: "b", revoke: true}}
	lines, code, _ = runStreamExit(t, srv2.URL, []string{"bot"}, []string{"--context", "bot"}, never, nil)
	if code != 1 || len(events(lines, "failed")) != 1 {
		t.Fatalf("last context lost: want exit 1 after a failed line, got %d: %s", code, sequence(lines))
	}
}

// Slow REST lookups must not starve pong handling and drop a live connection.
func TestStreamSlowLookupsKeepConnection(t *testing.T) {
	oldEvery, oldTimeout := pingEvery, pingTimeout
	pingEvery, pingTimeout = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { pingEvery, pingTimeout = oldEvery, oldTimeout })
	ws, srv := newWSServer(t)
	ws.channelDelay = 400 * time.Millisecond
	ws.scripts["bot-tok"] = []wsScript{{helloID: "b", hold: true, events: []map[string]any{
		postedEvent(1, "p1", "c1"), postedEvent(2, "p2", "c2")}}}
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot"}, func(l []map[string]any) bool {
		return len(posts(l)) == 2
	})
	if got, want := sequence(lines), "connected posted:p1 posted:p2"; got != want {
		t.Fatalf("lines\n got %s\nwant %s", got, want)
	}
}

// A server that never completes the websocket handshake must surface as a
// disconnect, not a silent hang.
func TestStreamDialTimeout(t *testing.T) {
	oldRest := restTimeout
	restTimeout = 100 * time.Millisecond
	t.Cleanup(func() { restTimeout = oldRest })
	ws, srv := newWSServer(t)
	ws.dialHang = true
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot"}, func(l []map[string]any) bool {
		return len(events(l, "disconnected")) > 0
	})
	if len(events(lines, "disconnected")) == 0 {
		t.Fatalf("hanging dial not reported: %s", sequence(lines))
	}
}

// A resume that replays nothing is still a working connection.
func TestStreamResumeWithoutEventsConnects(t *testing.T) {
	ws, srv := newWSServer(t)
	ws.scripts["bot-tok"] = []wsScript{
		{helloID: "c1", events: []map[string]any{postedEvent(1, "p1", "c1")}},
		{noHello: true, hold: true},
	}
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot"}, func(l []map[string]any) bool {
		return len(events(l, "connected")) == 2
	})
	if got, want := sequence(lines), "connected posted:p1 disconnected connected"; got != want {
		t.Fatalf("lines\n got %s\nwant %s", got, want)
	}
}

// Posts read before the server closes the socket must all come out before
// the disconnect; dropping them would also fake a gap.
func TestStreamDrainsBeforeDisconnect(t *testing.T) {
	ws, srv := newWSServer(t)
	var evs []map[string]any
	var want []string
	for i := 1; i <= 20; i++ {
		id := fmt.Sprintf("p%02d", i)
		evs = append(evs, postedEvent(int64(i), id, "c1"))
		want = append(want, "posted:"+id)
	}
	ws.scripts["bot-tok"] = []wsScript{{helloID: "b", events: evs}}
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot"}, func(l []map[string]any) bool {
		return len(events(l, "disconnected")) > 0
	})
	got := sequence(lines)
	if !strings.HasPrefix(got, "connected "+strings.Join(want, " ")+" disconnected") {
		t.Fatalf("posts lost or reordered around the disconnect: %s", got)
	}
}

// A renamed channel is matched and labeled by its new name, and later edits
// in it pass the channel filter from the refreshed cache.
func TestStreamFollowsChannelRename(t *testing.T) {
	ws, srv := newWSServer(t)
	ws.scripts["bot-tok"] = []wsScript{{helloID: "b", hold: true, events: []map[string]any{
		renamedPost(1, "p1", "c1", "renamed"), editedEvent(2, "p9", "c1")}}}
	lines := runStream(t, srv.URL, []string{"bot"}, []string{"--context", "bot", "--channel", "renamed"}, func(l []map[string]any) bool {
		return len(events(l, "post_edited")) == 1
	})
	if got, want := sequence(lines), "connected posted:p1 post_edited:p9"; got != want {
		t.Fatalf("lines\n got %s\nwant %s", got, want)
	}
	if ch := posts(lines)["p1"]["channel"]; ch != "renamed" {
		t.Fatalf("renamed channel labeled %v", ch)
	}
}
