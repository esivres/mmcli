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
  mmcli search  <query...> [--team T] [--channel C] [--from USER] [--after YYYY-MM-DD] [--before YYYY-MM-DD] [--limit N] [--or] [common]
  mmcli reply   <link|post_id> <message...> [common]
  mmcli post    <~channel|@user|link> <message...> [--team T] [common]
  mmcli delete  <link|post_id> [common]
  mmcli version

Common flags (get/thread/search/reply/post):
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

login password sources (first non-empty wins):
  --password VALUE (insecure; visible in ps), --password-stdin (first stdin line),
  $MMCLI_PASSWORD, then a line read from stdin.
  login makes the context current only if none is current yet, or with --use.
  Bot accounts cannot log in with a password: use --token-stdin with a personal
  access token. A token context never re-logs-in; a rejected token is an error.

Output (for an automated consumer):
  Default is compact JSON to stdout; errors go to stderr with a non-zero exit.
  get returns one object; thread/search return a JSON array sorted oldest-first.
  Each post object: {"id","time" (RFC3339),"user" (username),"channel_id",
  "root_id" (omitted if none),"message"}.

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

// usernamesFor resolves usernames for all authors in a PostList.
func usernamesFor(ctx context.Context, client *mm.Client, pl *mm.PostList) map[string]string {
	if pl == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var ids []string
	for _, p := range pl.Posts {
		if _, ok := seen[p.UserID]; !ok {
			seen[p.UserID] = struct{}{}
			ids = append(ids, p.UserID)
		}
	}
	return resolveUsernames(ctx, client, ids)
}

func resolveUsernames(ctx context.Context, client *mm.Client, ids []string) map[string]string {
	m := map[string]string{}
	users, err := client.UsersByIDs(ctx, ids)
	if err != nil {
		return m // best-effort; renderer falls back to raw IDs
	}
	for _, u := range users {
		m[u.ID] = u.Username
	}
	return m
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
