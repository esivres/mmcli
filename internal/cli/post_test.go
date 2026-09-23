package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/esivres/mmcli/internal/config"
	"github.com/esivres/mmcli/internal/secrets"
)

// postServer is a bot-token server that records where posts land.
func postServer(t *testing.T, postedTo *string, directMembers *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+botToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"bot1","username":"digest-bot"}`))
		case "/api/v4/users/username/alice":
			_, _ = w.Write([]byte(`{"id":"alice1","username":"alice"}`))
		case "/api/v4/channels/direct":
			_ = json.NewDecoder(r.Body).Decode(directMembers)
			_, _ = w.Write([]byte(`{"id":"dm1","name":"bot1__alice1"}`))
		case "/api/v4/teams/name/myteam":
			_, _ = w.Write([]byte(`{"id":"team1","name":"myteam"}`))
		case "/api/v4/teams/team1/channels/name/ops":
			_, _ = w.Write([]byte(`{"id":"ops1","name":"ops"}`))
		case "/api/v4/posts":
			var body map[string]string
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			*postedTo = body["channel_id"]
			_, _ = w.Write([]byte(`{"id":"` + postID + `","user_id":"bot1","channel_id":"` + body["channel_id"] + `","message":"` + body["message"] + `"}`))
		case "/api/v4/users/ids":
			_, _ = w.Write([]byte(`[{"id":"bot1","username":"digest-bot"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func botDeps(t *testing.T, srvURL, team string) (deps, *strings.Builder) {
	t.Helper()
	d, _, errOut := newDeps(t, secrets.NewMemory(), botToken+"\n")
	args := []string{"login", "--context", "bot", "--url", srvURL, "--token-stdin"}
	if team != "" {
		args = append(args, "--team", team)
	}
	if code := run(d, args); code != 0 {
		t.Fatalf("login exit %d: %s", code, errOut)
	}
	var out strings.Builder
	d.stdout = &out
	return d, &out
}

// @user must reach that user's direct channel without any team: that is how
// the bot delivers a digest.
func TestPostToUserUsesDirectChannel(t *testing.T) {
	var postedTo string
	var members []string
	srv := postServer(t, &postedTo, &members)
	d, _ := botDeps(t, srv.URL, "")

	if code := run(d, []string{"post", "@alice", "digest", "--context", "bot"}); code != 0 {
		t.Fatalf("post exit %d: %s", code, d.stderr)
	}
	if postedTo != "dm1" {
		t.Fatalf("posted to %q, want the direct channel", postedTo)
	}
	if strings.Join(members, ",") != "bot1,alice1" {
		t.Fatalf("direct channel members %v, want sender and recipient", members)
	}
}

// ~name is the Mattermost spelling of a channel and must resolve like a bare name.
func TestPostTildeChannel(t *testing.T) {
	var postedTo string
	var members []string
	srv := postServer(t, &postedTo, &members)
	d, _ := botDeps(t, srv.URL, "myteam")

	for _, target := range []string{"~ops", "ops"} {
		postedTo = ""
		if code := run(d, []string{"post", target, "hi", "--context", "bot"}); code != 0 {
			t.Fatalf("post %s exit %d: %s", target, code, d.stderr)
		}
		if postedTo != "ops1" {
			t.Fatalf("post %s landed in %q, want channel ops", target, postedTo)
		}
	}
}

// Adding a second context must not redirect commands run without --context;
// --use opts in explicitly.
func TestLoginKeepsCurrentContext(t *testing.T) {
	var logins atomic.Int32
	srv := fakeServer(t, &logins)
	store := secrets.NewMemory()
	d, _, errOut := newDeps(t, store, botToken+"\n")
	login := func(name string, extra ...string) {
		t.Helper()
		d.stdin = strings.NewReader(botToken + "\n")
		args := append([]string{"login", "--context", name, "--url", srv.URL, "--token-stdin"}, extra...)
		if code := run(d, args); code != 0 {
			t.Fatalf("login %s exit %d: %s", name, code, errOut)
		}
	}
	current := func() string {
		t.Helper()
		path, _ := config.DefaultPath()
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.CurrentName
	}

	login("work")
	if got := current(); got != "work" {
		t.Fatalf("first login must become current, got %q", got)
	}
	login("bot")
	if got := current(); got != "work" {
		t.Fatalf("second login switched current to %q", got)
	}
	login("bot", "--use")
	if got := current(); got != "bot" {
		t.Fatalf("--use must make the context current, got %q", got)
	}
}
