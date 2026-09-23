// Package cli wires command-line subcommands to the Mattermost client.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/esivres/mmcli/internal/config"
	"github.com/esivres/mmcli/internal/mm"
	"github.com/esivres/mmcli/internal/output"
	"github.com/esivres/mmcli/internal/secrets"
)

const usage = `mmcli — console Mattermost client

Usage:
  mmcli login   --url URL --login-id ID [--context NAME] [--team TEAM] [--password-stdin] [--use]
  mmcli login   --url URL --token-stdin [--context NAME] [--team TEAM] [--use]
  mmcli logout  [--context NAME]
  mmcli context list | use NAME | current
  mmcli get     <link|post_id> [--thread] [common]
  mmcli thread  <link|post_id> [common]
  mmcli search  <query...> [--team T] [--channel C] [--from USER] [--after YYYY-MM-DD] [--before YYYY-MM-DD] [--limit N | --all] [--or] [common]
  mmcli reply   <link|post_id> [message...] [--file PATH]... [common]
  mmcli post    <channel|~channel|@user|link> [message...] [--file PATH]... [--team T] [common]
  mmcli delete  <link|post_id> [common]
  mmcli channels [--team T] [--since TIME] [--unread] [common]
  mmcli threads [--team T] [--since TIME] [--unread] [--limit N | --all] [common]
  mmcli stream  [--context NAME]... [--mention] [--dm] [--channel NAME]... [--merge-window 1.5s]
  mmcli file search <query...> [--ext EXT] [--channel C] [--from USER] [--after D] [--before D] [--limit N | --all] [common]
  mmcli file get  <file_id> [--out DIR] [common]
  mmcli file text <file_id> [--max-bytes N] [--out DIR] [common]
  mmcli version

Common flags (get/thread/search/reply/post/file/channels/threads):
  --context NAME   use a specific stored context (default: current)
  --pretty         indented JSON output (default: compact JSON)

Notes:
  - References accepted everywhere: https://host/<team>/pl/<id>, a channel URL,
    or a bare 26-char post/ID.
  - Flags may appear before or after positional args. If a message word starts
    with '-' or matches a flag name, put '--' before the message, e.g.
    mmcli reply <id> -- "-- looks off to me".
  - search assembles Mattermost modifiers from flags: --channel→in:, --from→from:,
    --after→after:, --before→before:; mention search is just a "@username" query.
  - The server returns at most 100 results per search; search re-queries
    further back in time until done. A cut-off (--limit, default 50, or over
    100 results on a single UTC day) is reported as a warning on stderr.
    Dates in --after/--before are UTC days.

login password sources (first non-empty wins):
  --password VALUE (insecure; visible in ps), --password-stdin (first stdin line),
  $MMCLI_PASSWORD, then a line read from stdin.
  post @user sends a direct message; --team does not apply there. --file
  attaches up to 10 local files (checked before any upload); the message may
  then be empty. Flags go before "--". If an upload fails midway, files
  already uploaded stay unattached on the server and no post is created.
  stream prints one JSON line per live event until interrupted. A post seen
  by several contexts of the same server is merged into one line after
  --merge-window: "contexts" lists who can see (and reply to) it, "mentions"
  whose user is mentioned. Filters are OR-ed (--channel takes the channel URL
  name); none means every post; edits and deletes follow a post this run
  emitted within 24h. Lines
  keep arrival order. Status lines:
  {"event":"connected"|"disconnected"|"gap","context","detail"}; a gap means
  events were lost (no resume, or skipped sequence numbers); on a slow
  network a gap may follow connected. A token or login rejected by
  Mattermost is final: on the first attempt stream exits with an error;
  later it prints {"event":"failed",...} and exits non-zero once no context
  is left.
  Posts list attachments in "files" ({"id","name","mime","size"}). file search
  matches names, and contents where the server extracts them. file get saves
  to ~/.cache/mmcli/files/<id>/ (or --out, never replacing a file) and prints
  the path. file text also extracts text: plain text/logs, docx, xlsx, pdf
  (needs pdftotext), zip (size/count/depth limits; skipped entries are
  listed). Long text keeps its start and end (--max-bytes). Images are
  refused: no OCR, view the file.
  channels lists the caller's channels, direct and group messages included,
  most recent post first: {"id","name","type","last_post_at","unread",
  "mentions"}; unread counts thread replies too. threads lists the threads
  the caller follows (collapsed reply threads), most recent reply first: the
  root post plus "reply_count","last_reply_at","unread_replies",
  "unread_mentions","participants". Both cover every team unless --team is
  given; TIME is YYYY-MM-DD (UTC) or RFC3339.
  login makes the context current only if none is current yet, or with --use.
  Bot accounts cannot log in with a password: use --token-stdin with a personal
  access token. A token context never re-logs-in; a rejected token is an error.

Output (for an automated consumer):
  Default is compact JSON to stdout; errors go to stderr with a non-zero exit.
  get returns one object; thread/search return a JSON array sorted oldest-first.
  Each post object: {"id","time" (RFC3339),"user" (username),"channel_id",
  "channel" (name, "@user" for a direct message, title for a group message;
  omitted if unresolved),"root_id" (omitted if none),"message"}.

Examples:
  echo "$PW" | mmcli login --context work --url https://mm.example.com \
      --login-id me@example.com --team myteam --password-stdin
  echo "$BOT_TOKEN" | mmcli login --context bot --url https://mm.example.com \
      --team myteam --token-stdin
  mmcli get https://mm.example.com/myteam/pl/<id> --thread --pretty
  mmcli search "@me" --after 2026-06-01 --limit 20      # posts mentioning me
  mmcli search deploy --channel ops --from alice --before 2026-06-15
  mmcli reply <link|id> "looking into it"
  mmcli post ~ops "deploy finished"
  mmcli post @alice "digest is ready" --context bot   # direct message`

// Version is set at release time via -ldflags.
var Version = "dev"

// deps bundles injectable dependencies (real or test fakes).
type deps struct {
	store  secrets.Store
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	ctx    context.Context // stream lifetime; nil means until SIGINT/SIGTERM
}

// Run dispatches a command. Returns a process exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	d := deps{store: secrets.SystemStore{}, stdin: stdin, stdout: stdout, stderr: stderr}
	return run(d, args)
}

func run(d deps, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(d.stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "login":
		err = cmdLogin(d, args[1:])
	case "logout":
		err = cmdLogout(d, args[1:])
	case "context":
		err = cmdContext(d, args[1:])
	case "get":
		err = cmdGet(d, args[1:], false)
	case "thread":
		err = cmdGet(d, args[1:], true)
	case "search":
		err = cmdSearch(d, args[1:])
	case "reply":
		err = cmdReply(d, args[1:])
	case "post":
		err = cmdPost(d, args[1:])
	case "delete":
		err = cmdDelete(d, args[1:])
	case "stream":
		err = cmdStream(d, args[1:])
	case "channels":
		err = cmdChannels(d, args[1:])
	case "threads":
		err = cmdThreads(d, args[1:])
	case "file":
		err = cmdFile(d, args[1:])
	case "version", "--version":
		fmt.Fprintln(d.stdout, Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprintln(d.stdout, usage)
		return 0
	default:
		fmt.Fprintf(d.stderr, "unknown command %q\n\n%s\n", args[0], usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(d.stderr, "error:", err)
		return 1
	}
	return 0
}

// common flags shared by read/write commands.
type common struct {
	context string
	pretty  bool
}

func addCommon(fs *flag.FlagSet, c *common) {
	fs.StringVar(&c.context, "context", "", "stored context name (default: current)")
	fs.BoolVar(&c.pretty, "pretty", false, "indented JSON output")
}

// newCtx returns a request context with a sane timeout.
func newCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

// buildClient loads config + secrets and returns an authenticated client for
// the chosen context (explicit name, or the current one).
func (d deps) buildClient(name string) (*mm.Client, string, config.Context, error) {
	path, err := config.DefaultPath()
	if err != nil {
		return nil, "", config.Context{}, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", config.Context{}, err
	}
	var ctxName string
	var cc config.Context
	if name != "" {
		var ok bool
		cc, ok = cfg.Get(name)
		if !ok {
			return nil, "", config.Context{}, fmt.Errorf("context %q not found", name)
		}
		ctxName = name
	} else {
		ctxName, cc, err = cfg.Current()
		if err != nil {
			return nil, "", config.Context{}, err
		}
	}

	if cc.Auth == config.AuthToken {
		token, err := d.store.Get(secrets.TokenKey(ctxName))
		if err != nil {
			return nil, "", config.Context{}, fmt.Errorf("read access token for context %q: %w", ctxName, err)
		}
		return mm.New(cc.URL, token, "", ""), ctxName, cc, nil
	}

	password, err := d.store.Get(secrets.PasswordKey(ctxName))
	if err != nil {
		return nil, "", config.Context{}, fmt.Errorf("read password for context %q: %w", ctxName, err)
	}
	token, _ := d.store.Get(secrets.TokenKey(ctxName)) // may be absent; login will populate

	client := mm.New(cc.URL, token, cc.LoginID, password)
	client.OnToken = func(tok string) {
		_ = d.store.Set(secrets.TokenKey(ctxName), tok)
	}
	return client, ctxName, cc, nil
}

// resolveTeamID picks a team name (explicit flag > from-link > context default)
// and resolves it to an ID.
func resolveTeamID(ctx context.Context, client *mm.Client, cc config.Context, explicit, fromLink string) (string, error) {
	name := explicit
	if name == "" {
		name = fromLink
	}
	if name == "" {
		name = cc.DefaultTeam
	}
	if name == "" {
		return "", fmt.Errorf("no team specified: pass --team, use a link with a team, or set a default team at login")
	}
	t, err := client.GetTeamByName(ctx, name)
	if err != nil {
		return "", fmt.Errorf("resolve team %q: %w", name, err)
	}
	return t.ID, nil
}

// namesFor resolves authors and channels of posts. It is best-effort: on
// errors the renderer falls back to raw IDs and omits the channel label.
func namesFor(ctx context.Context, client *mm.Client, posts ...*mm.Post) output.Names {
	names := output.Names{Users: map[string]string{}, Channels: map[string]string{}}
	userIDs := map[string]struct{}{}
	channels := map[string]*mm.Channel{} // nil marks an inaccessible channel
	hasDirect := false
	for _, p := range posts {
		userIDs[p.UserID] = struct{}{}
		if _, seen := channels[p.ChannelID]; seen {
			continue
		}
		ch, err := client.GetChannel(ctx, p.ChannelID)
		if err != nil {
			channels[p.ChannelID] = nil
			continue
		}
		channels[p.ChannelID] = ch
		if ch.Type == "D" {
			hasDirect = true
			// Direct channel names are "<userID>__<userID>".
			for _, id := range strings.Split(ch.Name, "__") {
				userIDs[id] = struct{}{}
			}
		}
	}

	ids := make([]string, 0, len(userIDs))
	for id := range userIDs {
		ids = append(ids, id)
	}
	if users, err := client.UsersByIDs(ctx, ids); err == nil {
		for _, u := range users {
			names.Users[u.ID] = u.Username
		}
	}

	var meID string
	if hasDirect {
		if me, err := client.Me(ctx); err == nil {
			meID = me.ID
		}
	}
	for id, ch := range channels {
		if ch == nil {
			continue
		}
		if label := channelLabel(ch, meID, names.Users); label != "" {
			names.Channels[id] = label
		}
	}
	return names
}

// channelLabel is the readable name of a channel: its URL name, "@<the other
// user>" for a direct message, the title for a group message.
func channelLabel(ch *mm.Channel, meID string, users map[string]string) string {
	switch ch.Type {
	case "D":
		return directLabel(ch.Name, meID, users)
	case "G":
		return ch.DisplayName
	default:
		return ch.Name
	}
}

// directLabel renders a direct channel as "@<the other user>", or "" when the
// other side cannot be told apart from the caller.
func directLabel(channelName, meID string, users map[string]string) string {
	a, b, ok := strings.Cut(channelName, "__")
	if !ok {
		return ""
	}
	var other string
	switch {
	case a == b:
		other = a
	case meID == a:
		other = b
	case meID == b:
		other = a
	default:
		return ""
	}
	if u := users[other]; u != "" {
		return "@" + u
	}
	return ""
}

// postsOf returns the posts of a PostList in no particular order.
func postsOf(pl *mm.PostList) []*mm.Post {
	if pl == nil {
		return nil
	}
	out := make([]*mm.Post, 0, len(pl.Posts))
	for _, p := range pl.Posts {
		out = append(out, p)
	}
	return out
}

func joinMessage(args []string) string {
	return strings.TrimSpace(strings.Join(args, " "))
}

// parseFlags parses args while allowing flags to appear after positional
// arguments. The stdlib flag package stops at the first positional, which is a
// footgun for commands like `get <link> --thread`. We permute flags ahead of
// positionals; `--` ends flag scanning, and known value-taking flags consume
// the following token. For reply/post, whose free-text message may start with
// '-' or contain a token matching a known flag, pass `--` before the message.
func parseFlags(fs *flag.FlagSet, args []string) error {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			if !strings.Contains(a, "=") {
				name := strings.TrimLeft(a, "-")
				if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
					i++
					flags = append(flags, args[i])
				}
			}
			continue
		}
		positional = append(positional, a)
	}
	return fs.Parse(append(flags, positional...))
}

func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}
