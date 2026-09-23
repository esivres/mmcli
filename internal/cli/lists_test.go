package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/esivres/mmcli/internal/mm"
)

var listDay = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

// listServer has two teams; the direct channel with alice, like on the live
// server, is returned by both.
func listServer(t *testing.T, threads map[string][]mm.Thread) (*httptest.Server, *[]string) {
	t.Helper()
	var threadQueries []string
	dm := mm.Channel{ID: "dm1", Name: "me1__alice1", Type: "D", LastPostAt: listDay.UnixMilli(), TotalMsgCount: 7}
	channels := map[string][]mm.Channel{
		"t1": {dm, {ID: "c-old", Name: "old", Type: "O", LastPostAt: listDay.AddDate(0, 0, -30).UnixMilli(), TotalMsgCount: 3},
			{ID: "c-read", Name: "read", Type: "O", LastPostAt: listDay.Add(-time.Hour).UnixMilli(), TotalMsgCount: 4}},
		"t2": {dm, {ID: "c-t2", Name: "other-team", Type: "P", LastPostAt: listDay.Add(time.Hour).UnixMilli(), TotalMsgCount: 2}},
	}
	members := []mm.ChannelMember{{ChannelID: "dm1", MsgCount: 5, MentionCount: 2},
		{ChannelID: "c-old", MsgCount: 0}, {ChannelID: "c-read", MsgCount: 4}, {ChannelID: "c-t2", MsgCount: 2, MentionCount: 1}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"me1","username":"me"}`))
		case p == "/api/v4/teams/name/two":
			_, _ = w.Write([]byte(`{"id":"t2","name":"two"}`))
		case p == "/api/v4/users/me/teams":
			_, _ = w.Write([]byte(`[{"id":"t1","name":"one"},{"id":"t2","name":"two"}]`))
		case p == "/api/v4/users/ids":
			_, _ = w.Write([]byte(`[{"id":"me1","username":"me"},{"id":"alice1","username":"alice"},{"id":"u1","username":"bob"}]`))
		case strings.HasPrefix(p, "/api/v4/channels/"):
			id := strings.TrimPrefix(p, "/api/v4/channels/")
			_ = json.NewEncoder(w).Encode(mm.Channel{ID: id, Name: "chan-" + id, Type: "O"})
		case strings.HasSuffix(p, "/channels/members"):
			_ = json.NewEncoder(w).Encode(members)
		case strings.HasSuffix(p, "/channels"):
			_ = json.NewEncoder(w).Encode(channels[strings.Split(p, "/")[6]])
		case strings.HasSuffix(p, "/threads"):
			threadQueries = append(threadQueries, r.URL.RawQuery)
			q := r.URL.Query()
			all := threads[strings.Split(p, "/")[6]]
			start := 0
			if b := q.Get("before"); b != "" {
				for i, th := range all {
					if th.ID == b {
						start = i + 1
					}
				}
			}
			// The live server's since matches the last update, a superset of
			// the last reply; the worst case is that it filters nothing.
			var page []mm.Thread
			for _, th := range all[start:] {
				if len(page) == mm.ThreadsPerPage {
					break
				}
				if q.Get("extended") != "true" {
					th.Participants = []mm.User{{ID: th.Participants[0].ID}}
				}
				page = append(page, th)
			}
			_ = json.NewEncoder(w).Encode(mm.ThreadList{Threads: page})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &threadQueries
}

// makeThreads returns n threads, one reply per hour back from listDay, newest first.
func makeThreads(prefix string, n int) []mm.Thread {
	out := make([]mm.Thread, n)
	for i := range out {
		id := fmt.Sprintf("%s%03d", prefix, i)
		at := listDay.Add(-time.Duration(i) * time.Hour).UnixMilli()
		out[i] = mm.Thread{ID: id, LastReplyAt: at, ReplyCount: 1, Participants: []mm.User{{ID: "u1", Username: "bob"}},
			Post: &mm.Post{ID: id, UserID: "u1", ChannelID: "c-" + prefix, CreateAt: at - 1000, Message: "root " + id}}
	}
	return out
}

func runJSON[T any](t *testing.T, d deps, out, errOut *bytes.Buffer, args ...string) T {
	t.Helper()
	out.Reset()
	if code := run(d, args); code != 0 {
		t.Fatalf("%v exit %d: %s", args, code, errOut)
	}
	var v T
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("%v: %v: %s", args, err, out.Bytes())
	}
	return v
}

// The digest finds conversations through channels: a direct message must be
// there once and named after the other person whatever team it came from,
// and the filters must drop exactly what was read or is too old.
func TestChannelsCoverEveryTeam(t *testing.T) {
	srv, _ := listServer(t, nil)
	d, out, errOut := loggedIn(t, srv.URL)

	all := runJSON[[]RenderedChannel](t, d, out, errOut, "channels")
	var names []string
	for _, c := range all {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "other-team,@alice,read,old" {
		t.Fatalf("want each channel once, newest post first, got %s", got)
	}
	if dm := all[1]; dm.Unread != 2 || dm.Mentions != 2 || dm.Type != "D" {
		t.Fatalf("direct channel read state: %+v", dm)
	}

	recent := runJSON[[]RenderedChannel](t, d, out, errOut, "channels", "--since", "2026-09-20", "--unread")
	if len(recent) != 2 || recent[0].Name != "other-team" || recent[1].Name != "@alice" {
		t.Fatalf("--since must drop the old channel and --unread the read one: %+v", recent)
	}
}

// Following threads is the only way to see a thread where the user was just
// mentioned; the list must reach past a page, stop at --since, and keep the
// newest threads when cut at --limit across teams.
func TestThreadsPagesAndStops(t *testing.T) {
	srv, queries := listServer(t, map[string][]mm.Thread{"t1": makeThreads("a", 450), "t2": makeThreads("b", 3)})
	d, out, errOut := loggedIn(t, srv.URL)

	all := runJSON[[]RenderedThread](t, d, out, errOut, "threads", "--all")
	if len(all) != 453 {
		t.Fatalf("--all must page past %d, got %d", mm.ThreadsPerPage, len(all))
	}
	if all[0].Participants[0] != "bob" || all[0].Channel != "chan-c-a" || all[0].User != "bob" || all[0].Message != "root a000" {
		t.Fatalf("thread must carry its root post and named participants: %+v", all[0])
	}
	for i := 1; i < len(all); i++ {
		if all[i].LastReplyAt > all[i-1].LastReplyAt {
			t.Fatalf("threads must be newest reply first at %d", i)
		}
	}

	*queries = nil
	since := listDay.Add(-10 * time.Hour).Format(time.RFC3339)
	recent := runJSON[[]RenderedThread](t, d, out, errOut, "threads", "--since", since, "--all")
	if len(recent) != 11+3 {
		t.Fatalf("--since %s: want 14 threads, got %d", since, len(recent))
	}
	if len(*queries) != 2 || !strings.Contains((*queries)[0], "since=") {
		t.Fatalf("--since must narrow the server listing and not page through older threads: %v", *queries)
	}

	// Team "two" has 3 threads: a limit that fits is silent, one short warns.
	errOut.Reset()
	if exact := runJSON[[]RenderedThread](t, d, out, errOut, "threads", "--team", "two", "--limit", "3"); len(exact) != 3 || errOut.Len() != 0 {
		t.Fatalf("a list that fits --limit is complete and must not warn: %d, %s", len(exact), errOut)
	}
	if strings.Contains((*queries)[len(*queries)-1], "unread=") {
		t.Fatalf("unread must only be asked for with --unread: %v", *queries)
	}
	if short := runJSON[[]RenderedThread](t, d, out, errOut, "threads", "--team", "two", "--limit", "2"); len(short) != 2 || !strings.Contains(errOut.String(), "stopped at --limit 2") {
		t.Fatalf("one thread over --limit must be reported: %d, %s", len(short), errOut)
	}
	runJSON[[]RenderedThread](t, d, out, errOut, "threads", "--unread", "--limit", "1")
	if !strings.Contains((*queries)[len(*queries)-1], "unread=true") {
		t.Fatalf("--unread must reach the server: %v", *queries)
	}

	top := runJSON[[]RenderedThread](t, d, out, errOut, "threads", "--limit", "4")
	var ids []string
	for _, th := range top {
		ids = append(ids, th.ID)
	}
	if got := strings.Join(ids, ","); got != "a000,b000,a001,b001" {
		t.Fatalf("--limit must keep the newest threads over all teams, got %s", got)
	}
	if !strings.Contains(errOut.String(), "stopped at --limit 4") {
		t.Fatalf("a cut-off list must warn, stderr: %s", errOut.String())
	}
}
