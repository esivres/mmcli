package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/esivres/mmcli/internal/secrets"
)

// Mentions are only actionable when the reader sees where they happened; a
// direct message must name the other person, not the caller.
func TestSearchLabelsChannels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+botToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"bot1","username":"digest-bot"}`))
		case "/api/v4/teams/name/myteam":
			_, _ = w.Write([]byte(`{"id":"team1","name":"myteam"}`))
		case "/api/v4/teams/team1/posts/search":
			_, _ = w.Write([]byte(`{"order":["a","b","c","e"],"posts":{
				"a":{"id":"a","create_at":1,"user_id":"alice1","channel_id":"ops1","message":"@usov ops"},
				"b":{"id":"b","create_at":2,"user_id":"alice1","channel_id":"dm1","message":"@usov dm"},
				"c":{"id":"c","create_at":3,"user_id":"alice1","channel_id":"gm1","message":"@usov gm"},
				"e":{"id":"e","create_at":4,"user_id":"alice1","channel_id":"dm2","message":"@usov dm"}}}`))
		case "/api/v4/channels/ops1":
			_, _ = w.Write([]byte(`{"id":"ops1","name":"ops","display_name":"Ops","type":"O"}`))
		case "/api/v4/channels/dm1":
			_, _ = w.Write([]byte(`{"id":"dm1","name":"alice1__bot1","type":"D"}`))
		case "/api/v4/channels/dm2": // the caller may come first in the name
			_, _ = w.Write([]byte(`{"id":"dm2","name":"bot1__alice1","type":"D"}`))
		case "/api/v4/channels/gm1":
			_, _ = w.Write([]byte(`{"id":"gm1","name":"f00d","display_name":"alice, bob, digest-bot","type":"G"}`))
		case "/api/v4/users/ids":
			_, _ = w.Write([]byte(`[{"id":"alice1","username":"alice"},{"id":"bot1","username":"digest-bot"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	d, out, errOut := newDeps(t, secrets.NewMemory(), botToken+"\n")
	if code := run(d, []string{"login", "--context", "bot", "--url", srv.URL, "--team", "myteam", "--token-stdin"}); code != 0 {
		t.Fatalf("login exit %d: %s", code, errOut)
	}
	out.Reset()

	if code := run(d, []string{"search", "@usov"}); code != 0 {
		t.Fatalf("search exit %d: %s", code, errOut)
	}
	for _, want := range []string{`"channel":"ops"`, `"channel":"alice, bob, digest-bot"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if n := strings.Count(out.String(), `"channel":"@alice"`); n != 2 {
		t.Errorf("both direct channels must be labeled @alice, got %d in %s", n, out)
	}
}
