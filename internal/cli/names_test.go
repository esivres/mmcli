package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/esivres/mmcli/internal/secrets"
)

// namesServer serves a search over ops (O), two DMs in both name orders, a
// group message and an inaccessible channel, counting requests per path.
// With meFails, /users/me works only once (for login validation).
func namesServer(t *testing.T, meFails bool) (*httptest.Server, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits[r.URL.Path]++
		if r.Header.Get("Authorization") != "Bearer "+botToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v4/users/me":
			if meFails && hits[r.URL.Path] > 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"id":"bot1","username":"digest-bot"}`))
		case "/api/v4/teams/name/myteam":
			_, _ = w.Write([]byte(`{"id":"team1","name":"myteam"}`))
		case "/api/v4/teams/team1/posts/search":
			// The bot authors the DM posts, so only the channel name reveals alice.
			_, _ = w.Write([]byte(`{"order":["a","b","c","e","x1","x2"],"posts":{
				"a":{"id":"a","create_at":1,"user_id":"carol1","channel_id":"ops1"},
				"b":{"id":"b","create_at":2,"user_id":"bot1","channel_id":"dm1"},
				"c":{"id":"c","create_at":3,"user_id":"carol1","channel_id":"gm1"},
				"e":{"id":"e","create_at":4,"user_id":"bot1","channel_id":"dm2"},
				"x1":{"id":"x1","create_at":5,"user_id":"carol1","channel_id":"secret1"},
				"x2":{"id":"x2","create_at":6,"user_id":"carol1","channel_id":"secret1"}}}`))
		case "/api/v4/channels/ops1":
			_, _ = w.Write([]byte(`{"id":"ops1","name":"ops","display_name":"Ops","type":"O"}`))
		case "/api/v4/channels/dm1":
			_, _ = w.Write([]byte(`{"id":"dm1","name":"alice1__bot1","type":"D"}`))
		case "/api/v4/channels/dm2":
			_, _ = w.Write([]byte(`{"id":"dm2","name":"bot1__alice1","type":"D"}`))
		case "/api/v4/channels/gm1":
			_, _ = w.Write([]byte(`{"id":"gm1","name":"f00d","display_name":"alice, bob, digest-bot","type":"G"}`))
		case "/api/v4/channels/secret1":
			w.WriteHeader(http.StatusForbidden)
		case "/api/v4/users/ids":
			known := map[string]string{"alice1": "alice", "bot1": "digest-bot", "carol1": "carol"}
			var ids []string
			_ = json.NewDecoder(r.Body).Decode(&ids)
			type user struct {
				ID       string `json:"id"`
				Username string `json:"username"`
			}
			var found []user
			for _, id := range ids {
				if name, ok := known[id]; ok {
					found = append(found, user{id, name})
				}
			}
			_ = json.NewEncoder(w).Encode(found)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func(path string) int { mu.Lock(); defer mu.Unlock(); return hits[path] }
}

func searchLabels(t *testing.T, meFails bool) (map[string]string, func(string) int) {
	t.Helper()
	srv, hits := namesServer(t, meFails)
	d, out, errOut := newDeps(t, secrets.NewMemory(), botToken+"\n")
	if code := run(d, []string{"login", "--context", "bot", "--url", srv.URL, "--team", "myteam", "--token-stdin"}); code != 0 {
		t.Fatalf("login exit %d: %s", code, errOut)
	}
	out.Reset()
	if code := run(d, []string{"search", "@usov"}); code != 0 {
		t.Fatalf("search exit %d: %s", code, errOut)
	}
	var posts []struct{ ID, Channel string }
	if err := json.Unmarshal(out.Bytes(), &posts); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for _, p := range posts {
		labels[p.ID] = p.Channel
	}
	return labels, hits
}

// Mentions are only actionable when the reader sees where they happened; a
// direct message must name the other person, never the caller.
func TestSearchLabelsChannels(t *testing.T) {
	labels, hits := searchLabels(t, false)
	want := map[string]string{"a": "ops", "b": "@alice", "c": "alice, bob, digest-bot", "e": "@alice", "x1": "", "x2": ""}
	for id, w := range want {
		if labels[id] != w {
			t.Errorf("post %s: channel %q, want %q", id, labels[id], w)
		}
	}
	// A long search over one inaccessible channel must not fan out requests.
	if n := hits("/api/v4/channels/secret1"); n != 1 {
		t.Errorf("inaccessible channel fetched %d times, want 1", n)
	}
	if n := hits("/api/v4/users/me"); n > 2 { // one for login validation, one for labels
		t.Errorf("/users/me called %d times", n)
	}
}

// Without knowing the caller a DM label would be a guess; omit it instead.
func TestDirectLabelOmittedWhenCallerUnknown(t *testing.T) {
	labels, _ := searchLabels(t, true)
	if labels["b"] != "" || labels["e"] != "" {
		t.Fatalf("DM labels must be omitted when /users/me fails, got %q and %q", labels["b"], labels["e"])
	}
	if labels["a"] != "ops" {
		t.Fatalf("non-DM labels must still resolve, got %q", labels["a"])
	}
}
