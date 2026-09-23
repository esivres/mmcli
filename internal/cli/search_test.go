package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/esivres/mmcli/internal/mm"
	"github.com/esivres/mmcli/internal/secrets"
)

var beforeRe = regexp.MustCompile(`before:(\d{4}-\d{2}-\d{2})`)

var modifiersFirstRe = regexp.MustCompile(`^(\S+:\S+ )*x$`)

// cappedServer mimics the live server: at most searchCap results per query,
// newest first, per_page ignored, before: filtering by UTC day. It fails the
// test on a query with modifiers after the free text or without an explicit
// UTC offset, and counts search requests.
func cappedServer(t *testing.T, posts []*mm.Post) (*httptest.Server, *int) {
	t.Helper()
	var searches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"bot1","username":"digest-bot"}`))
		case "/api/v4/teams/name/myteam":
			_, _ = w.Write([]byte(`{"id":"team1","name":"myteam"}`))
		case "/api/v4/teams/team1/posts/search":
			searches++
			var raw map[string]any
			_ = json.NewDecoder(r.Body).Decode(&raw)
			if off, ok := raw["time_zone_offset"]; !ok || off != float64(0) {
				t.Errorf("search must pin time_zone_offset to 0, got %v", raw["time_zone_offset"])
			}
			var opts mm.SearchOpts
			b, _ := json.Marshal(raw)
			_ = json.Unmarshal(b, &opts)
			if !modifiersFirstRe.MatchString(opts.Terms) {
				t.Errorf("modifiers must precede the free text, got %q", opts.Terms)
			}
			var bound int64 = 1 << 62
			if m := beforeRe.FindStringSubmatch(opts.Terms); m != nil {
				day, _ := time.Parse("2006-01-02", m[1])
				bound = day.UnixMilli()
			}
			pl := mm.PostList{Posts: map[string]*mm.Post{}}
			for _, p := range posts {
				if opts.Page == 0 && p.CreateAt < bound && len(pl.Order) < 100 {
					pl.Order = append(pl.Order, p.ID)
					pl.Posts[p.ID] = p
				}
			}
			_ = json.NewEncoder(w).Encode(pl)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &searches
}

// makePosts returns n posts newest first, perDay of them per UTC day.
func makePosts(n, perDay int) []*mm.Post {
	start := time.Date(2026, 9, 20, 23, 0, 0, 0, time.UTC)
	var out []*mm.Post
	for i := 0; i < n; i++ {
		at := start.AddDate(0, 0, -i/perDay).Add(-time.Duration(i%perDay) * time.Minute)
		out = append(out, &mm.Post{ID: fmt.Sprintf("p%03d", i), CreateAt: at.UnixMilli(), UserID: "u1", ChannelID: "c1"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreateAt > out[j].CreateAt })
	return out
}

// A digest built on search must never lose results silently: either every
// matching post comes back, or the cut-off is reported.
func TestSearchPastServerCap(t *testing.T) {
	sameTime := makePosts(150, 150)
	for _, p := range sameTime {
		p.CreateAt = sameTime[0].CreateAt
	}
	cases := []struct {
		name     string
		posts    []*mm.Post
		args     []string
		want     int
		warn     string
		searches int // 0 = not checked
	}{
		{"all across days", makePosts(250, 7), []string{"--all"}, 250, "", 0},
		{"limit cuts off", makePosts(250, 7), []string{"--limit", "120"}, 120, "--limit 120", 0},
		{"exact limit", makePosts(250, 7), []string{"--limit", "250"}, 250, "", 0},
		{"default limit", makePosts(250, 7), nil, 50, "--limit 50", 0},
		{"one day over cap", makePosts(150, 150), []string{"--all"}, 100, "more than 100 results on 2026-09-20", 0},
		// Each stop condition must end the walk on its own.
		{"full day right before --before", makePosts(150, 150), []string{"--all", "--before", "2026-09-21"}, 100, "more than 100 results on 2026-09-20", 1},
		{"no new posts in a round", sameTime, []string{"--all"}, 100, "more than 100 results on 2026-09-20", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, searches := cappedServer(t, tc.posts)
			d, out, errOut := newDeps(t, secrets.NewMemory(), botToken+"\n")
			if code := run(d, []string{"login", "--context", "bot", "--url", srv.URL, "--team", "myteam", "--token-stdin"}); code != 0 {
				t.Fatalf("login exit %d: %s", code, errOut)
			}
			out.Reset()
			errOut.Reset()
			if code := run(d, append([]string{"search", "x"}, tc.args...)); code != 0 {
				t.Fatalf("search exit %d: %s", code, errOut)
			}
			var got []struct{ ID string }
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, p := range got {
				if seen[p.ID] {
					t.Fatalf("duplicate post %s", p.ID)
				}
				seen[p.ID] = true
			}
			if len(got) != tc.want {
				t.Fatalf("got %d posts, want %d", len(got), tc.want)
			}
			if tc.warn == "" && errOut.Len() != 0 {
				t.Fatalf("unexpected warning %q", errOut)
			}
			if tc.warn != "" && !strings.Contains(errOut.String(), tc.warn) {
				t.Fatalf("warning %q does not mention %q", errOut, tc.warn)
			}
			if tc.searches != 0 && *searches != tc.searches {
				t.Fatalf("%d search requests, want %d", *searches, tc.searches)
			}
		})
	}
}

// Inputs that would silently change the result set are refused up front.
func TestSearchRejectsAmbiguousInput(t *testing.T) {
	srv, searches := cappedServer(t, makePosts(10, 7))
	d, _, errOut := newDeps(t, secrets.NewMemory(), botToken+"\n")
	if code := run(d, []string{"login", "--context", "bot", "--url", srv.URL, "--team", "myteam", "--token-stdin"}); code != 0 {
		t.Fatalf("login exit %d: %s", code, errOut)
	}
	for _, args := range [][]string{
		{"search", "x", "--limit", "0"},
		{"search", "x", "before:2026-09-01"},
	} {
		if code := run(d, args); code != 1 {
			t.Fatalf("%v: want exit 1, got %d", args, code)
		}
	}
	if *searches != 0 {
		t.Fatalf("refused input still sent %d searches", *searches)
	}
}
