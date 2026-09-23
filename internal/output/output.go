// Package output renders command results. Default is compact JSON (machine
// friendly, since the primary consumer is an automated agent); --pretty emits
// indented JSON.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/esivres/mmcli/internal/mm"
)

// RenderedPost is the flattened, enriched shape we emit for posts: human time
// and resolved username instead of raw epoch/IDs.
type RenderedPost struct {
	ID        string `json:"id"`
	Time      string `json:"time"`
	User      string `json:"user"`
	ChannelID string `json:"channel_id"`
	RootID    string `json:"root_id,omitempty"`
	Message   string `json:"message"`
}

// Posts flattens a PostList into chronological RenderedPosts (oldest first),
// resolving user IDs via the supplied username map. Mattermost's PostList.Order
// is not reliably time-sorted (notably the thread endpoint), so we sort by
// CreateAt explicitly.
func Posts(pl *mm.PostList, usernames map[string]string) []RenderedPost {
	if pl == nil {
		return nil
	}
	posts := make([]*mm.Post, 0, len(pl.Order))
	for _, id := range pl.Order {
		if p := pl.Posts[id]; p != nil {
			posts = append(posts, p)
		}
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].CreateAt < posts[j].CreateAt })
	out := make([]RenderedPost, 0, len(posts))
	for _, p := range posts {
		out = append(out, render(p, usernames))
	}
	return out
}

// One renders a single post.
func One(p *mm.Post, usernames map[string]string) RenderedPost {
	return render(p, usernames)
}

func render(p *mm.Post, usernames map[string]string) RenderedPost {
	user := usernames[p.UserID]
	if user == "" {
		user = p.UserID
	}
	return RenderedPost{
		ID:        p.ID,
		Time:      time.UnixMilli(p.CreateAt).Format(time.RFC3339),
		User:      user,
		ChannelID: p.ChannelID,
		RootID:    p.RootID,
		Message:   p.Message,
	}
}

// Emit writes v as JSON to w. When pretty is true the output is indented.
func Emit(w io.Writer, v any, pretty bool) error {
	enc := json.NewEncoder(w)
	if pretty {
		enc.SetIndent("", "  ")
	}
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}
