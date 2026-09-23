package cli

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/esivres/mmcli/internal/mm"
	"github.com/esivres/mmcli/internal/output"
)

// RenderedChannel is a channel with the caller's read state.
type RenderedChannel struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Type       string `json:"type"`
	LastPostAt string `json:"last_post_at,omitempty"`
	Unread     int64  `json:"unread"`
	Mentions   int64  `json:"mentions"`
}

// RenderedThread is a followed thread: its root post plus reply state.
type RenderedThread struct {
	output.RenderedPost
	ReplyCount     int64    `json:"reply_count"`
	LastReplyAt    string   `json:"last_reply_at,omitempty"`
	UnreadReplies  int64    `json:"unread_replies"`
	UnreadMentions int64    `json:"unread_mentions"`
	Participants   []string `json:"participants,omitempty"`
}

// parseSince accepts a UTC day (YYYY-MM-DD) or an RFC3339 time; "" means no bound.
func parseSince(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UnixMilli(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("--since: want YYYY-MM-DD or RFC3339, got %q", s)
	}
	return t.UnixMilli(), nil
}

func formatMillis(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).Format(time.RFC3339)
}

// listTeams is the --team team, or every team of the caller: direct messages
// belong to no team, so without --team none are missed.
func listTeams(ctx context.Context, client *mm.Client, team string) ([]string, error) {
	if team != "" {
		t, err := client.GetTeamByName(ctx, team)
		if err != nil {
			return nil, fmt.Errorf("resolve team %q: %w", team, err)
		}
		return []string{t.ID}, nil
	}
	teams, err := client.MyTeams(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(teams))
	for _, t := range teams {
		ids = append(ids, t.ID)
	}
	return ids, nil
}

func cmdChannels(d deps, args []string) error {
	fs := flag.NewFlagSet("channels", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var c common
	addCommon(fs, &c)
	team := fs.String("team", "", "team URL name (default: every team)")
	since := fs.String("since", "", "only channels with a post at or after YYYY-MM-DD (UTC) or RFC3339")
	unread := fs.Bool("unread", false, "only channels with unread posts or mentions")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	sinceMs, err := parseSince(*since)
	if err != nil {
		return err
	}
	client, _, _, err := d.buildClient(c.context)
	if err != nil {
		return err
	}
	ctx, cancel := newCtx()
	defer cancel()

	teams, err := listTeams(ctx, client, *team)
	if err != nil {
		return err
	}
	channels := map[string]mm.Channel{}
	members := map[string]mm.ChannelMember{}
	for _, tid := range teams {
		chs, err := client.MyChannels(ctx, tid)
		if err != nil {
			return err
		}
		ms, err := client.MyChannelMembers(ctx, tid)
		if err != nil {
			return err
		}
		// Direct and group messages repeat in every team.
		for _, ch := range chs {
			channels[ch.ID] = ch
		}
		for _, m := range ms {
			members[m.ChannelID] = m
		}
	}

	var out []RenderedChannel
	var direct []*mm.Channel
	for id, ch := range channels {
		m := members[id]
		r := RenderedChannel{ID: id, Type: ch.Type, LastPostAt: formatMillis(ch.LastPostAt),
			Unread: max(ch.TotalMsgCount-m.MsgCount, 0), Mentions: m.MentionCount}
		if ch.LastPostAt < sinceMs || (*unread && r.Unread == 0 && r.Mentions == 0) {
			continue
		}
		out = append(out, r)
		if ch.Type == "D" {
			ch := ch
			direct = append(direct, &ch)
		}
	}
	users, meID := directUsers(ctx, client, direct)
	for i := range out {
		ch := channels[out[i].ID]
		out[i].Name = channelLabel(&ch, meID, users)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := channels[out[i].ID].LastPostAt, channels[out[j].ID].LastPostAt
		if a != b {
			return a > b
		}
		return out[i].ID < out[j].ID
	})
	if out == nil {
		out = []RenderedChannel{}
	}
	return output.Emit(d.stdout, out, c.pretty)
}

// directUsers resolves the users of direct channels and the caller's ID, as
// directLabel needs; best-effort like namesFor.
func directUsers(ctx context.Context, client *mm.Client, direct []*mm.Channel) (map[string]string, string) {
	users := map[string]string{}
	if len(direct) == 0 {
		return users, ""
	}
	seen := map[string]bool{}
	var ids []string
	for _, ch := range direct {
		for _, id := range strings.Split(ch.Name, "__") {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if us, err := client.UsersByIDs(ctx, ids); err == nil {
		for _, u := range us {
			users[u.ID] = u.Username
		}
	}
	var meID string
	if me, err := client.Me(ctx); err == nil {
		meID = me.ID
	}
	return users, meID
}

func cmdThreads(d deps, args []string) error {
	fs := flag.NewFlagSet("threads", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var c common
	addCommon(fs, &c)
	team := fs.String("team", "", "team URL name (default: every team)")
	since := fs.String("since", "", "only threads with a reply at or after YYYY-MM-DD (UTC) or RFC3339")
	unread := fs.Bool("unread", false, "only threads with unread replies")
	limit := fs.Int("limit", 50, "max threads")
	all := fs.Bool("all", false, "fetch every thread, ignoring --limit")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *limit <= 0 && !*all {
		return fmt.Errorf("--limit must be positive; use --all for no limit")
	}
	sinceMs, err := parseSince(*since)
	if err != nil {
		return err
	}
	maxThreads := *limit
	if *all {
		maxThreads = 0
	}
	client, _, _, err := d.buildClient(c.context)
	if err != nil {
		return err
	}
	ctx, cancel := newCtx()
	defer cancel()

	teams, err := listTeams(ctx, client, *team)
	if err != nil {
		return err
	}
	byID := map[string]mm.Thread{}
	for _, tid := range teams {
		if err := collectThreads(ctx, client, tid, sinceMs, *unread, maxThreads, byID); err != nil {
			return err
		}
	}
	threads := make([]mm.Thread, 0, len(byID))
	for _, t := range byID {
		threads = append(threads, t)
	}
	sort.Slice(threads, func(i, j int) bool {
		if threads[i].LastReplyAt != threads[j].LastReplyAt {
			return threads[i].LastReplyAt > threads[j].LastReplyAt
		}
		return threads[i].ID < threads[j].ID
	})
	if maxThreads > 0 && len(threads) > maxThreads {
		threads = threads[:maxThreads]
		fmt.Fprintf(d.stderr, "warning: stopped at --limit %d, more threads exist (raise --limit or use --all)\n", maxThreads)
	}

	roots := make([]*mm.Post, 0, len(threads))
	for _, t := range threads {
		roots = append(roots, t.Post)
	}
	names := namesFor(ctx, client, roots...)
	out := make([]RenderedThread, 0, len(threads))
	for _, t := range threads {
		var parts []string
		for _, u := range t.Participants {
			parts = append(parts, u.Username)
		}
		out = append(out, RenderedThread{RenderedPost: output.One(t.Post, names), ReplyCount: t.ReplyCount,
			LastReplyAt: formatMillis(t.LastReplyAt), UnreadReplies: t.UnreadReplies,
			UnreadMentions: t.UnreadMentions, Participants: parts})
	}
	return output.Emit(d.stdout, out, c.pretty)
}

// collectThreads pages one team's followed threads into byID, newest reply
// first. It stops past since, or once one thread more than maxThreads is in
// hand: enough to tell the caller the list was cut off.
func collectThreads(ctx context.Context, client *mm.Client, teamID string, since int64, unread bool, maxThreads int, byID map[string]mm.Thread) error {
	before := ""
	taken := 0
	for {
		page, err := client.MyThreads(ctx, teamID, since, before, unread)
		if err != nil {
			return err
		}
		for _, t := range page.Threads {
			if t.LastReplyAt < since {
				return nil
			}
			if t.Post == nil {
				continue
			}
			byID[t.ID] = t
			taken++
			if maxThreads > 0 && taken > maxThreads {
				return nil
			}
		}
		if len(page.Threads) < mm.ThreadsPerPage {
			return nil
		}
		before = page.Threads[len(page.Threads)-1].ID
	}
}
