package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/esivres/mmcli/internal/mm"
	"github.com/esivres/mmcli/internal/output"
)

// Tunables overridden by tests.
var (
	reconnectMin = time.Second
	reconnectMax = 30 * time.Second
	flushEvery   = 100 * time.Millisecond
	// The server pings every 60s; our own ping detects a half-open connection
	// (sleep, NAT timeout) that would otherwise look alive and silent.
	pingEvery   = 30 * time.Second
	pingTimeout = 10 * time.Second
	restTimeout = 30 * time.Second
	// A failed channel/user lookup is retried after this, not per event.
	lookupRetry = 5 * time.Minute
)

const (
	// seenTTL keeps delivered keys long enough to drop late duplicates and
	// replays after a resumed connection.
	seenTTL = 10 * time.Minute
	// emittedTTL lets edits and deletes of an emitted post pass filters their
	// events carry no data for (channel, mentions).
	emittedTTL = 24 * time.Hour
)

// streamEvent is one JSONL line. Post events carry the rendered post; status
// events (connected, disconnected, gap) carry context and detail instead.
type streamEvent struct {
	Event    string   `json:"event"`
	Contexts []string `json:"contexts,omitempty"`
	Mentions []string `json:"mentions,omitempty"`
	*output.RenderedPost
	ChannelType string `json:"channel_type,omitempty"`
	Context     string `json:"context,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// sighting is one context's copy of a post event, or a status line when
// status is set.
type sighting struct {
	context   string
	key       string
	postKey   string // server|post id, shared by a post's posted/edited/deleted
	matched   bool
	mentioned bool
	event     streamEvent
	status    bool
	// fatal ends the source; neverConnected makes it end the whole stream.
	fatal          error
	neverConnected bool
}

type streamFilter struct {
	mention  bool
	dm       bool
	channels []string
}

// matches reports whether an event passes; filters are OR-ed, none means all.
func (f streamFilter) matches(mentioned bool, channelType, channelName string) bool {
	if !f.mention && !f.dm && len(f.channels) == 0 {
		return true
	}
	return (f.mention && mentioned) || (f.dm && channelType == "D") || slices.Contains(f.channels, channelName)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// cmdStream prints live post events from one or more contexts as JSONL,
// merging copies of the same event seen by several contexts.
func cmdStream(d deps, args []string) error {
	fs := flag.NewFlagSet("stream", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var contexts, channels multiFlag
	fs.Var(&contexts, "context", "context to stream from (repeatable; default: current)")
	fs.Var(&channels, "channel", "only this channel (URL name, repeatable)")
	mention := fs.Bool("mention", false, "only posts mentioning a streamed context's user")
	dm := fs.Bool("dm", false, "only direct messages")
	window := fs.Duration("merge-window", 1500*time.Millisecond, "how long to wait for copies of an event from other contexts")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if len(contexts) == 0 {
		contexts = multiFlag{""}
	}
	filter := streamFilter{mention: *mention, dm: *dm, channels: channels}

	ctx := d.ctx
	if ctx == nil {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var order []string
	var sources []*streamSource
	for _, name := range contexts {
		client, ctxName, cc, err := d.buildClient(name)
		if err != nil {
			return err
		}
		if slices.Contains(order, ctxName) {
			return fmt.Errorf("context %q given twice", ctxName)
		}
		order = append(order, ctxName)
		sources = append(sources, &streamSource{name: ctxName, server: cc.URL, client: client, filter: filter,
			channels: map[string]cachedChannel{}, users: map[string]cachedUser{}})
	}
	sightings := make(chan sighting)
	var wg sync.WaitGroup
	for _, src := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src.run(ctx, sightings)
		}()
	}
	err := mergeStream(ctx, d, order, *window, sightings, len(sources))
	cancel()
	wg.Wait()
	return err
}

// mergeStream holds every line for window, collecting the contexts that saw a
// post, then emits in arrival order. Status lines share the queue so they
// never overtake posts received before them. It is the only stdout writer.
func mergeStream(ctx context.Context, d deps, order []string, window time.Duration, sightings <-chan sighting, alive int) error {
	type pending struct {
		first  time.Time
		status *streamEvent
		copies map[string]sighting
	}
	var queue []*pending
	waiting := map[string]*pending{}
	seen := map[string]time.Time{}
	emitted := map[string]time.Time{}
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()

	emitPost := func(p *pending, now time.Time) error {
		var out streamEvent
		matched := false
		var postKey string
		for _, name := range order {
			s, ok := p.copies[name]
			if !ok {
				continue
			}
			if out.Event == "" {
				out = s.event // the label is as seen by the first context in order
				postKey = s.postKey
			}
			out.Contexts = append(out.Contexts, name)
			if s.mentioned {
				out.Mentions = append(out.Mentions, name)
			}
			matched = matched || s.matched
		}
		if _, ok := emitted[postKey]; ok && out.Event != "posted" {
			matched = true
		}
		if !matched {
			return nil
		}
		emitted[postKey] = now
		return output.Emit(d.stdout, out, false)
	}

	flush := func(now time.Time, all bool) error {
		for len(queue) > 0 && (all || now.Sub(queue[0].first) >= window) {
			p := queue[0]
			queue = queue[1:]
			if p.status != nil {
				if err := output.Emit(d.stdout, *p.status, false); err != nil {
					return err
				}
				continue
			}
			for key := range p.copies {
				delete(waiting, p.copies[key].key)
				seen[p.copies[key].key] = now
				break
			}
			if err := emitPost(p, now); err != nil {
				return err
			}
		}
		for key, at := range seen {
			if now.Sub(at) > seenTTL {
				delete(seen, key)
			}
		}
		for key, at := range emitted {
			if now.Sub(at) > emittedTTL {
				delete(emitted, key)
			}
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return flush(time.Now(), true)
		case s := <-sightings:
			now := time.Now()
			if s.fatal != nil {
				if s.neverConnected {
					_ = flush(now, true)
					return fmt.Errorf("context %q: %w", s.context, s.fatal)
				}
				queue = append(queue, &pending{first: now, status: &streamEvent{Event: "failed", Context: s.context, Detail: s.fatal.Error()}})
				if alive--; alive == 0 {
					if err := flush(now, true); err != nil {
						return err
					}
					return fmt.Errorf("all contexts failed; last: %q: %w", s.context, s.fatal)
				}
				continue
			}
			if s.status {
				ev := s.event
				queue = append(queue, &pending{first: now, status: &ev})
				continue
			}
			if _, done := seen[s.key]; done {
				continue
			}
			p := waiting[s.key]
			if p == nil {
				p = &pending{first: now, copies: map[string]sighting{}}
				waiting[s.key] = p
				queue = append(queue, p)
			}
			p.copies[s.context] = s
		case now := <-tick.C:
			if err := flush(now, false); err != nil {
				return err
			}
		}
	}
}

type cachedChannel struct {
	ch *mm.Channel // nil: lookup failed at
	at time.Time
}

type cachedUser struct {
	name string // "": lookup failed at
	at   time.Time
}

// streamSource keeps one context's websocket alive and turns its events into
// sightings. Its caches are touched only by its own goroutine.
type streamSource struct {
	name     string
	server   string
	client   *mm.Client
	filter   streamFilter
	meID     string
	channels map[string]cachedChannel
	users    map[string]cachedUser
}

func (s *streamSource) run(ctx context.Context, sightings chan<- sighting) {
	var connID string
	var nextSeq int64
	delay := reconnectMin
	everConnected := false
	for ctx.Err() == nil {
		err := s.session(ctx, &connID, &nextSeq, sightings, func() { delay = reconnectMin; everConnected = true })
		if ctx.Err() != nil {
			return
		}
		if mm.IsUnauthorized(err) {
			select {
			case sightings <- sighting{context: s.name, fatal: err, neverConnected: !everConnected}:
			case <-ctx.Done():
			}
			return
		}
		s.status(ctx, sightings, "disconnected", err.Error())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		delay = min(delay*2, reconnectMax)
	}
}

func (s *streamSource) status(ctx context.Context, sightings chan<- sighting, event, detail string) {
	select {
	case sightings <- sighting{status: true, event: streamEvent{Event: event, Context: s.name, Detail: detail}}:
	case <-ctx.Done():
	}
}

// session runs one websocket connection until it fails.
func (s *streamSource) session(ctx context.Context, connID *string, nextSeq *int64, sightings chan<- sighting, connected func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A REST call first lets password contexts re-login and refresh the token.
	rctx, rcancel := context.WithTimeout(ctx, restTimeout)
	me, err := s.client.Me(rctx)
	rcancel()
	if err != nil {
		return err
	}
	s.meID = me.ID
	resuming := *connID != ""
	conn, err := s.client.DialWS(ctx, *connID, *nextSeq)
	if err != nil {
		return err
	}
	defer conn.Close()

	pingErr := make(chan error, 1)
	pingDone := make(chan struct{})
	defer func() { cancel(); <-pingDone }()
	go func() {
		defer close(pingDone)
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, pingTimeout)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil && ctx.Err() == nil {
					pingErr <- fmt.Errorf("no pong from server: %w", err)
					cancel()
					return
				}
			}
		}
	}()

	first := true
	for {
		ev, err := conn.Next(ctx)
		if err != nil {
			select {
			case perr := <-pingErr:
				return perr
			default:
				return err
			}
		}
		if first {
			// A resumed connection gets no hello; replayed events follow directly.
			first = false
			if ev.Event == "hello" {
				var id string
				_ = json.Unmarshal(ev.Data["connection_id"], &id)
				if resuming && id != *connID {
					s.status(ctx, sightings, "gap", "server could not resume the connection; events while disconnected are lost")
					*nextSeq = 0
				}
				*connID = id
			}
			connected()
			s.status(ctx, sightings, "connected", "")
		}
		switch {
		case ev.Seq < *nextSeq:
			continue // replayed twice
		case ev.Seq > *nextSeq:
			s.status(ctx, sightings, "gap", fmt.Sprintf("missed events: %d", ev.Seq-*nextSeq))
		}
		*nextSeq = ev.Seq + 1

		if sg, ok := s.sighting(ctx, ev); ok {
			select {
			case sightings <- sg:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// sighting converts a post event; other events are ignored.
func (s *streamSource) sighting(ctx context.Context, ev *mm.WSEvent) (sighting, bool) {
	switch ev.Event {
	case "posted", "post_edited", "post_deleted":
	default:
		return sighting{}, false
	}
	var raw string
	var p mm.Post
	if json.Unmarshal(ev.Data["post"], &raw) != nil || json.Unmarshal([]byte(raw), &p) != nil {
		return sighting{}, false
	}
	// Only posted carries mentions; edits and deletes pass via emitted posts.
	var mentionsRaw string
	_ = json.Unmarshal(ev.Data["mentions"], &mentionsRaw)
	var mentions []string
	_ = json.Unmarshal([]byte(mentionsRaw), &mentions)
	mentioned := slices.Contains(mentions, s.meID)

	ch := s.channel(ctx, p.ChannelID)
	names := s.names(ctx, &p, ch)
	rendered := output.One(&p, names)
	var channelType, channelName string
	if ch != nil {
		channelType, channelName = ch.Type, ch.Name
	}
	return sighting{
		context:   s.name,
		key:       fmt.Sprintf("%s|%s|%s|%d", s.server, ev.Event, p.ID, p.UpdateAt),
		postKey:   s.server + "|" + p.ID,
		matched:   s.filter.matches(mentioned, channelType, channelName),
		mentioned: mentioned,
		event:     streamEvent{Event: ev.Event, RenderedPost: &rendered, ChannelType: channelType},
	}, true
}

// channel returns the cached channel, fetching it at most once per lookupRetry.
func (s *streamSource) channel(ctx context.Context, id string) *mm.Channel {
	if c, ok := s.channels[id]; ok && (c.ch != nil || time.Since(c.at) < lookupRetry) {
		return c.ch
	}
	rctx, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	ch, err := s.client.GetChannel(rctx, id)
	if err != nil {
		ch = nil
	}
	s.channels[id] = cachedChannel{ch: ch, at: time.Now()}
	return ch
}

// names resolves the author and the channel label of p from the caches.
func (s *streamSource) names(ctx context.Context, p *mm.Post, ch *mm.Channel) output.Names {
	ids := []string{p.UserID}
	if ch != nil && ch.Type == "D" {
		ids = append(ids, strings.Split(ch.Name, "__")...)
	}
	var missing []string
	for _, id := range ids {
		if u, ok := s.users[id]; !ok || (u.name == "" && time.Since(u.at) >= lookupRetry) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		rctx, cancel := context.WithTimeout(ctx, restTimeout)
		users, err := s.client.UsersByIDs(rctx, missing)
		cancel()
		now := time.Now()
		for _, id := range missing {
			s.users[id] = cachedUser{at: now}
		}
		if err == nil {
			for _, u := range users {
				s.users[u.ID] = cachedUser{name: u.Username, at: now}
			}
		}
	}

	names := output.Names{Users: map[string]string{}, Channels: map[string]string{}}
	for _, id := range ids {
		if n := s.users[id].name; n != "" {
			names.Users[id] = n
		}
	}
	if ch != nil {
		var label string
		switch ch.Type {
		case "D":
			label = directLabel(ch.Name, s.meID, names.Users)
		case "G":
			label = ch.DisplayName
		default:
			label = ch.Name
		}
		if label != "" {
			names.Channels[p.ChannelID] = label
		}
	}
	return names
}
