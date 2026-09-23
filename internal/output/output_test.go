package output

import (
	"testing"

	"github.com/esivres/mmcli/internal/mm"
)

// Posts must return chronological order (oldest first) even though Mattermost
// PostList.Order is newest-first — this is what makes a thread readable.
func TestPostsChronological(t *testing.T) {
	pl := &mm.PostList{
		Order: []string{"p2", "p1"}, // newest-first as the API returns
		Posts: map[string]*mm.Post{
			"p1": {ID: "p1", CreateAt: 1000, UserID: "u1", Message: "first"},
			"p2": {ID: "p2", CreateAt: 2000, UserID: "u2", Message: "second"},
		},
	}
	names := Names{Users: map[string]string{"u1": "alice", "u2": "bob"}}
	got := Posts(pl, names)
	if len(got) != 2 {
		t.Fatalf("expected 2 posts, got %d", len(got))
	}
	if got[0].ID != "p1" || got[1].ID != "p2" {
		t.Fatalf("expected chronological [p1 p2], got [%s %s]", got[0].ID, got[1].ID)
	}
	if got[0].User != "alice" {
		t.Fatalf("expected username resolution, got %q", got[0].User)
	}
}

// render must fall back to the raw user ID when no username is known.
func TestRenderUnknownUser(t *testing.T) {
	p := &mm.Post{ID: "x", UserID: "u9", Message: "hi"}
	r := One(p, Names{})
	if r.User != "u9" {
		t.Fatalf("expected fallback to raw id u9, got %q", r.User)
	}
}
