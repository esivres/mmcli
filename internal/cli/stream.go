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
)

// seenTTL keeps delivered keys long enough to drop late duplicates and
// replays after a resumed connection.
const seenTTL = 10 * time.Minute

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

// sighting is one context's copy of a post event.
type sighting struct {
	context   string
	key       string
	matched   bool
	mentioned bool
	event     streamEvent
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
	fs.Var(&channels, "channel", "only this channel name (repeatable)")
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

	var order []string
	sightings := make(chan sighting)
	status := make(chan streamEvent)
	for _, name := range contexts {
		client, ctxName, cc, err := d.buildClient(name)
		if err != nil {
			return err
		}
		if slices.Contains(order, ctxName) {
			return fmt.Errorf("context %q given twice", ctxName)
		}
		order = append(order, ctxName)
		src := &streamSource{name: ctxName, server: cc.URL, client: client, filter: filter,
			names: output.Names{Users: map[string]string{}, Channels: map[string]string{}}}
		go src.run(ctx, sightings, status)
	}
	return mergeStream(ctx, d, order, *window, sightings, status)
}

// mergeStream holds each post event for window, collecting the contexts that
// saw it, then emits one line. It is the only writer to stdout.
func mergeStream(ctx context.Context, d deps, order []string, window time.Duration, sightings <-chan sighting, status <-chan streamEvent) error {
	type pending struct {
		first  time.Time
		copies map[string]sighting
	}
	waiting := map[string]*pending{}
	seen := map[string]time.Time{}
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	emit := func(ev streamEvent) error { return output.Emit(d.stdout, ev, false) }

	flush := func(now time.Time, all bool) error {
		for key, p := range waiting {
			if !all && now.Sub(p.first) < window {
				continue
			}
			delete(waiting, key)
			seen[key] = now
			var out streamEvent
			matched := false
			for _, name := range order {
				s, ok := p.copies[name]
				if !ok {
					continue
				}
				if out.Event == "" {
					out = s.event // the label is as seen by the first context in order
				}
				out.Contexts = append(out.Contexts, name)
				if s.mentioned {
					out.Mentions = append(out.Mentions, name)
				}
				matched = matched || s.matched
			}
			if matched {
				if err := emit(out); err != nil {
					return err
				}
			}
		}
		for key, at := range seen {
			if now.Sub(at) > seenTTL {
				delete(seen, key)
			}
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return flush(time.Now(), true)
		case ev := <-status:
			if err := emit(ev); err != nil {
				return err
			}
		case s := <-sightings:
			if _, done := seen[s.key]; done {
				continue
			}
			p := waiting[s.key]
			if p == nil {
				p = &pending{first: time.Now(), copies: map[string]sighting{}}
				waiting[s.key] = p
			}
			p.copies[s.context] = s
		case now := <-tick.C:
			if err := flush(now, false); err != nil {
				return err
			}
		}
	}
}

// streamSource keeps one context's websocket alive and turns its events into
// sightings.
type streamSource struct {
	name   string
	server string
	client *mm.Client
	filter streamFilter
	meID   string
	names  output.Names // cache: channel labels and usernames seen so far
}

func (s *streamSource) run(ctx context.Context, sightings chan<- sighting, status chan<- streamEvent) {
	var connID string
	var nextSeq int64
	delay := reconnectMin
	send := func(event, detail string) {
		select {
		case status <- streamEvent{Event: event, Context: s.name, Detail: detail}:
		case <-ctx.Done():
		}
	}
	for ctx.Err() == nil {
		err := s.session(ctx, &connID, &nextSeq, sightings, send, func() { delay = reconnectMin })
		if ctx.Err() != nil {
			return
		}
		send("disconnected", err.Error())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		delay = min(delay*2, reconnectMax)
	}
}

// session runs one websocket connection until it fails.
func (s *streamSource) session(ctx context.Context, connID *string, nextSeq *int64, sightings chan<- sighting, send func(event, detail string), connected func()) error {
	// A REST call first lets password contexts re-login and refresh the token.
	me, err := s.client.Me(ctx)
	if err != nil {
		return err
	}
	s.meID = me.ID
	conn, err := s.client.DialWS(ctx, *connID, *nextSeq)
	if err != nil {
		return err
	}
	defer conn.Close()
	for {
		ev, err := conn.Next(ctx)
		if err != nil {
			return err
		}
		if ev.Event == "hello" {
			var id string
			_ = json.Unmarshal(ev.Data["connection_id"], &id)
			if *connID != "" && id != *connID {
				send("gap", "server could not resume the connection; events while disconnected are lost")
				*nextSeq = 0
			}
			*connID = id
			connected()
			send("connected", "")
		}
		if ev.Seq != *nextSeq {
			send("gap", fmt.Sprintf("missed events %d..%d", *nextSeq, ev.Seq-1))
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
	var channelType, channelName, mentionsRaw string
	_ = json.Unmarshal(ev.Data["channel_type"], &channelType)
	_ = json.Unmarshal(ev.Data["channel_name"], &channelName)
	_ = json.Unmarshal(ev.Data["mentions"], &mentionsRaw)
	var mentions []string
	_ = json.Unmarshal([]byte(mentionsRaw), &mentions)
	mentioned := slices.Contains(mentions, s.meID)

	if _, ok := s.names.Channels[p.ChannelID]; !ok || s.names.Users[p.UserID] == "" {
		n := namesFor(ctx, s.client, &p)
		for k, v := range n.Channels {
			s.names.Channels[k] = v
		}
		for k, v := range n.Users {
			s.names.Users[k] = v
		}
		// Cache misses too, so an unlabeled channel is not refetched per event.
		s.names.Channels[p.ChannelID] = n.Channels[p.ChannelID]
	}
	rendered := output.One(&p, s.names)
	return sighting{
		context:   s.name,
		key:       fmt.Sprintf("%s|%s|%s|%d", s.server, ev.Event, p.ID, p.UpdateAt),
		matched:   s.filter.matches(mentioned, channelType, channelName),
		mentioned: mentioned,
		event:     streamEvent{Event: ev.Event, RenderedPost: &rendered, ChannelType: channelType},
	}, true
}
