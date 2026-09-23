package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/esivres/mmcli/internal/config"
	"github.com/esivres/mmcli/internal/link"
	"github.com/esivres/mmcli/internal/mm"
	"github.com/esivres/mmcli/internal/output"
	"github.com/esivres/mmcli/internal/secrets"
)

// cmdLogin stores a context, persists the password (or access token) in the
// keyring, and logs in.
func cmdLogin(d deps, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	name := fs.String("context", "default", "context name")
	url := fs.String("url", "", "Mattermost base URL, e.g. https://mm.example.com")
	loginID := fs.String("login-id", "", "login id (email or username)")
	team := fs.String("team", "", "default team URL name")
	password := fs.String("password", "", "password (insecure; prefer --password-stdin)")
	passwordStdin := fs.Bool("password-stdin", false, "read password from the first line of stdin")
	tokenStdin := fs.Bool("token-stdin", false, "read a personal access token from the first line of stdin (bot accounts)")
	use := fs.Bool("use", false, "make this context current")
	pretty := fs.Bool("pretty", false, "indented JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tokenStdin {
		if *password != "" || *passwordStdin || *loginID != "" {
			return fmt.Errorf("--token-stdin cannot be combined with --login-id or password flags")
		}
		return loginWithToken(d, *name, *url, *team, *use, *pretty)
	}
	if *url == "" || *loginID == "" {
		return fmt.Errorf("--url and --login-id are required")
	}

	pw, err := readPassword(d, *password, *passwordStdin)
	if err != nil {
		return err
	}
	if pw == "" {
		return fmt.Errorf("empty password")
	}

	// Persist password before login so a re-login (on token expiry) works later.
	if err := d.store.Set(secrets.PasswordKey(*name), pw); err != nil {
		return fmt.Errorf("store password: %w", err)
	}

	client := mm.New(*url, "", *loginID, pw)
	client.OnToken = func(tok string) { _ = d.store.Set(secrets.TokenKey(*name), tok) }

	ctx, cancel := newCtx()
	defer cancel()
	if err := client.Login(ctx); err != nil {
		return err
	}

	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	cfg.Set(*name, config.Context{URL: strings.TrimRight(*url, "/"), LoginID: *loginID, DefaultTeam: *team})
	if *use {
		cfg.CurrentName = *name
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	return output.Emit(d.stdout, map[string]any{
		"status":  "ok",
		"context": *name,
		"url":     *url,
		"login":   *loginID,
		"current": cfg.CurrentName == *name,
	}, *pretty)
}

// loginWithToken validates a personal access token and stores a token context.
func loginWithToken(d deps, name, url, team string, use, pretty bool) error {
	if url == "" {
		return fmt.Errorf("--url is required")
	}
	sc := bufio.NewScanner(d.stdin)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return err
		}
		return fmt.Errorf("no token on stdin")
	}
	tok := strings.TrimSpace(sc.Text())
	if tok == "" {
		return fmt.Errorf("empty token")
	}

	ctx, cancel := newCtx()
	defer cancel()
	me, err := mm.New(url, tok, "", "").Me(ctx)
	if err != nil {
		return fmt.Errorf("validate token: %w", err)
	}

	if err := d.store.Set(secrets.TokenKey(name), tok); err != nil {
		return fmt.Errorf("store token: %w", err)
	}

	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	cfg.Set(name, config.Context{URL: strings.TrimRight(url, "/"), LoginID: me.Username, DefaultTeam: team, Auth: config.AuthToken})
	if use {
		cfg.CurrentName = name
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	// Only after Save: a password context must stay usable if Save fails.
	if err := d.store.Delete(secrets.PasswordKey(name)); err != nil {
		return err
	}

	return output.Emit(d.stdout, map[string]any{
		"status":  "ok",
		"context": name,
		"url":     url,
		"login":   me.Username,
		"auth":    config.AuthToken,
		"current": cfg.CurrentName == name,
	}, pretty)
}

// readPassword resolves the password from (in order): flag, stdin, env.
func readPassword(d deps, flagVal string, fromStdin bool) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if fromStdin {
		sc := bufio.NewScanner(d.stdin)
		if sc.Scan() {
			return strings.TrimRight(sc.Text(), "\r\n"), sc.Err()
		}
		return "", fmt.Errorf("no password on stdin")
	}
	if v := os.Getenv("MMCLI_PASSWORD"); v != "" {
		return v, nil
	}
	// Last resort: read a line from stdin (visible).
	sc := bufio.NewScanner(d.stdin)
	if sc.Scan() {
		return strings.TrimRight(sc.Text(), "\r\n"), sc.Err()
	}
	return "", fmt.Errorf("no password provided (use --password-stdin or $MMCLI_PASSWORD)")
}

// cmdLogout drops the cached token and stored password for a context.
func cmdLogout(d deps, args []string) error {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	name := fs.String("context", "", "context name (default: current)")
	pretty := fs.Bool("pretty", false, "indented JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctxName := *name
	if ctxName == "" {
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		ctxName, _, err = cfg.Current()
		if err != nil {
			return err
		}
	}
	if err := d.store.Delete(secrets.TokenKey(ctxName)); err != nil {
		return err
	}
	if err := d.store.Delete(secrets.PasswordKey(ctxName)); err != nil {
		return err
	}
	return output.Emit(d.stdout, map[string]any{"status": "ok", "context": ctxName, "cleared": true}, *pretty)
}

// cmdContext handles `context list|use NAME|current`.
func cmdContext(d deps, args []string) error {
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: mmcli context list|use NAME|current")
	}
	switch args[0] {
	case "list":
		type row struct {
			Name    string `json:"name"`
			URL     string `json:"url"`
			Login   string `json:"login"`
			Team    string `json:"team,omitempty"`
			Auth    string `json:"auth,omitempty"`
			Current bool   `json:"current"`
		}
		var rows []row
		for n, c := range cfg.Contexts {
			rows = append(rows, row{Name: n, URL: c.URL, Login: c.LoginID, Team: c.DefaultTeam, Auth: c.Auth, Current: n == cfg.CurrentName})
		}
		return output.Emit(d.stdout, rows, true)
	case "current":
		n, c, err := cfg.Current()
		if err != nil {
			return err
		}
		return output.Emit(d.stdout, map[string]any{"name": n, "url": c.URL, "login": c.LoginID, "team": c.DefaultTeam}, true)
	case "use":
		if len(args) < 2 {
			return fmt.Errorf("usage: mmcli context use NAME")
		}
		if err := cfg.Use(args[1]); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		return output.Emit(d.stdout, map[string]any{"status": "ok", "current": args[1]}, false)
	default:
		return fmt.Errorf("unknown context subcommand %q", args[0])
	}
}

// cmdGet fetches a single post or its whole thread.
func cmdGet(d deps, args []string, thread bool) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var c common
	addCommon(fs, &c)
	if !thread {
		fs.BoolVar(&thread, "thread", false, "include the whole thread")
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: mmcli get <link|post_id> [--thread]")
	}
	ref, err := link.Parse(fs.Arg(0))
	if err != nil {
		return err
	}
	if ref.PostID == "" {
		return fmt.Errorf("this command needs a post link or post id, got a channel reference")
	}

	client, _, _, err := d.buildClient(c.context)
	if err != nil {
		return err
	}
	ctx, cancel := newCtx()
	defer cancel()

	if thread {
		pl, err := client.GetThread(ctx, ref.PostID)
		if err != nil {
			return err
		}
		names := usernamesFor(ctx, client, pl)
		return output.Emit(d.stdout, output.Posts(pl, names), c.pretty)
	}
	p, err := client.GetPost(ctx, ref.PostID)
	if err != nil {
		return err
	}
	names := resolveUsernames(ctx, client, []string{p.UserID})
	return output.Emit(d.stdout, output.One(p, names), c.pretty)
}

// cmdSearch searches posts within a team, assembling Mattermost search
// modifiers from flags.
func cmdSearch(d deps, args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var c common
	addCommon(fs, &c)
	team := fs.String("team", "", "team URL name (default: context default team)")
	channel := fs.String("channel", "", "restrict to channel (in:)")
	from := fs.String("from", "", "restrict to author (from:)")
	after := fs.String("after", "", "only posts after date YYYY-MM-DD")
	before := fs.String("before", "", "only posts before date YYYY-MM-DD")
	limit := fs.Int("limit", 50, "max results (per_page)")
	orSearch := fs.Bool("or", false, "OR search instead of AND")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	terms := buildTerms(fs.Args(), *channel, *from, *after, *before)
	if strings.TrimSpace(terms) == "" {
		return fmt.Errorf("nothing to search: provide a query and/or filters")
	}

	client, _, cc, err := d.buildClient(c.context)
	if err != nil {
		return err
	}
	ctx, cancel := newCtx()
	defer cancel()

	teamID, err := resolveTeamID(ctx, client, cc, *team, "")
	if err != nil {
		return err
	}
	pl, err := client.Search(ctx, teamID, mm.SearchOpts{Terms: terms, IsOrSearch: *orSearch, PerPage: *limit})
	if err != nil {
		return err
	}
	names := usernamesFor(ctx, client, pl)
	return output.Emit(d.stdout, output.Posts(pl, names), c.pretty)
}

// buildTerms assembles a Mattermost search string from a free query plus
// modifier flags.
func buildTerms(query []string, channel, from, after, before string) string {
	var b []string
	if channel != "" {
		b = append(b, "in:"+channel)
	}
	if from != "" {
		b = append(b, "from:"+from)
	}
	if after != "" {
		b = append(b, "after:"+after)
	}
	if before != "" {
		b = append(b, "before:"+before)
	}
	b = append(b, query...)
	return strings.TrimSpace(strings.Join(b, " "))
}

// cmdReply posts a threaded reply to the given post.
func cmdReply(d deps, args []string) error {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var c common
	addCommon(fs, &c)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: mmcli reply <link|post_id> <message...>")
	}
	ref, err := link.Parse(fs.Arg(0))
	if err != nil {
		return err
	}
	if ref.PostID == "" {
		return fmt.Errorf("reply needs a post link or post id")
	}
	message := joinMessage(fs.Args()[1:])
	if message == "" {
		return fmt.Errorf("empty message")
	}

	client, _, _, err := d.buildClient(c.context)
	if err != nil {
		return err
	}
	ctx, cancel := newCtx()
	defer cancel()

	target, err := client.GetPost(ctx, ref.PostID)
	if err != nil {
		return err
	}
	// Reply to the thread root: if the target is already a reply, reuse its root.
	root := target.RootID
	if root == "" {
		root = target.ID
	}
	created, err := client.CreatePost(ctx, target.ChannelID, message, root)
	if err != nil {
		return err
	}
	names := resolveUsernames(ctx, client, []string{created.UserID})
	return output.Emit(d.stdout, output.One(created, names), c.pretty)
}

// cmdDelete deletes a post by link or ID.
func cmdDelete(d deps, args []string) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var c common
	addCommon(fs, &c)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: mmcli delete <link|post_id>")
	}
	ref, err := link.Parse(fs.Arg(0))
	if err != nil {
		return err
	}
	if ref.PostID == "" {
		return fmt.Errorf("delete needs a post link or post id")
	}
	client, _, _, err := d.buildClient(c.context)
	if err != nil {
		return err
	}
	ctx, cancel := newCtx()
	defer cancel()
	if err := client.DeletePost(ctx, ref.PostID); err != nil {
		return err
	}
	return output.Emit(d.stdout, map[string]any{"status": "ok", "deleted": ref.PostID}, c.pretty)
}

// cmdPost posts a new message to a channel (~name, name or channel link) or,
// for @username, to the direct channel with that user.
func cmdPost(d deps, args []string) error {
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var c common
	addCommon(fs, &c)
	team := fs.String("team", "", "team URL name (default: context default team)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: mmcli post <~channel|@user|link> <message...>")
	}
	message := joinMessage(fs.Args()[1:])
	if message == "" {
		return fmt.Errorf("empty message")
	}

	client, _, cc, err := d.buildClient(c.context)
	if err != nil {
		return err
	}
	ctx, cancel := newCtx()
	defer cancel()

	var channelID string
	if username, ok := strings.CutPrefix(fs.Arg(0), "@"); ok {
		channelID, err = directChannelID(ctx, client, username)
	} else {
		channelID, err = teamChannelID(ctx, client, cc, *team, fs.Arg(0))
	}
	if err != nil {
		return err
	}
	created, err := client.CreatePost(ctx, channelID, message, "")
	if err != nil {
		return err
	}
	names := resolveUsernames(ctx, client, []string{created.UserID})
	return output.Emit(d.stdout, output.One(created, names), c.pretty)
}

// directChannelID returns the direct channel between the caller and username,
// creating it if needed; direct channels belong to no team.
func directChannelID(ctx context.Context, client *mm.Client, username string) (string, error) {
	me, err := client.Me(ctx)
	if err != nil {
		return "", err
	}
	u, err := client.GetUserByUsername(ctx, username)
	if err != nil {
		return "", fmt.Errorf("resolve user %q: %w", username, err)
	}
	ch, err := client.CreateDirectChannel(ctx, me.ID, u.ID)
	if err != nil {
		return "", fmt.Errorf("open direct channel with %q: %w", username, err)
	}
	return ch.ID, nil
}

// teamChannelID resolves a channel link, ~name or bare name within a team.
func teamChannelID(ctx context.Context, client *mm.Client, cc config.Context, team, target string) (string, error) {
	channelName := strings.TrimPrefix(target, "~")
	teamFromLink := ""
	if ref, err := link.Parse(target); err == nil && ref.ChannelName != "" {
		channelName = ref.ChannelName
		teamFromLink = ref.Team
	}
	teamID, err := resolveTeamID(ctx, client, cc, team, teamFromLink)
	if err != nil {
		return "", err
	}
	ch, err := client.GetChannelByName(ctx, teamID, channelName)
	if err != nil {
		return "", fmt.Errorf("resolve channel %q: %w", channelName, err)
	}
	return ch.ID, nil
}
