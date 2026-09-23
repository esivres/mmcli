package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

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
		_ = cfg.Use(*name)
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
		_ = cfg.Use(name)
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
		return output.Emit(d.stdout, output.Posts(pl, namesFor(ctx, client, postsOf(pl)...)), c.pretty)
	}
	p, err := client.GetPost(ctx, ref.PostID)
	if err != nil {
		return err
	}
	return output.Emit(d.stdout, output.One(p, namesFor(ctx, client, p)), c.pretty)
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
	limit := fs.Int("limit", 50, "max results")
	all := fs.Bool("all", false, "fetch every result, ignoring --limit")
	orSearch := fs.Bool("or", false, "OR search instead of AND")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if strings.TrimSpace(buildTerms(fs.Args(), *channel, *from, *after, *before)) == "" {
		return fmt.Errorf("nothing to search: provide a query and/or filters")
	}
	for _, a := range fs.Args() {
		if strings.HasPrefix(a, "before:") {
			return fmt.Errorf("use --before instead of before: in the query")
		}
	}
	if *limit <= 0 && !*all {
		return fmt.Errorf("--limit must be positive; use --all for no limit")
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
	maxPosts := *limit
	if *all {
		maxPosts = 0
	}
	build := func(before string) string {
		return buildTerms(fs.Args(), *channel, *from, *after, before)
	}
	pl, warning, err := searchAll(ctx, client, teamID, build, *before, *orSearch, maxPosts)
	if err != nil {
		return err
	}
	if warning != "" {
		fmt.Fprintln(d.stderr, "warning:", warning)
	}
	return output.Emit(d.stdout, output.Posts(pl, namesFor(ctx, client, postsOf(pl)...)), c.pretty)
}

// searchCap is the server's hard limit of results per search query; it
// ignores per_page above it and returns nothing for further pages.
const searchCap = 100

// searchAll runs a post search past searchCap; see walkBack.
func searchAll(ctx context.Context, client *mm.Client, teamID string, terms func(before string) string, before string, orSearch bool, maxPosts int) (*mm.PostList, string, error) {
	posts, warning, err := walkBack(before, maxPosts, func(before string) ([]*mm.Post, int, error) {
		// Offset 0 pins day boundaries to UTC, matching walkBack's day arithmetic.
		pl, err := client.Search(ctx, teamID, mm.SearchOpts{Terms: terms(before), IsOrSearch: orSearch, PerPage: searchCap, TimeZoneOffset: 0})
		if err != nil {
			return nil, 0, err
		}
		var page []*mm.Post
		for _, id := range pl.Order {
			if p := pl.Posts[id]; p != nil {
				page = append(page, p)
			}
		}
		return page, len(pl.Order), nil
	}, func(p *mm.Post) (string, int64) { return p.ID, p.CreateAt })
	if err != nil {
		return nil, "", err
	}
	merged := &mm.PostList{Posts: map[string]*mm.Post{}}
	for _, p := range posts {
		merged.Order = append(merged.Order, p.ID)
		merged.Posts[p.ID] = p
	}
	return merged, warning, nil
}

// walkBack works around searchCap by re-querying with before: set just past
// the oldest result's UTC day, deduplicating the overlap. It stops at
// maxItems (0 = no cap) and returns a warning when results were cut off.
func walkBack[T any](before string, maxItems int, fetch func(before string) ([]T, int, error), key func(T) (string, int64)) ([]T, string, error) {
	var out []T
	seen := map[string]bool{}
	for {
		// raw is the server's result count; it decides whether the cap was hit.
		page, raw, err := fetch(before)
		if err != nil {
			return nil, "", err
		}
		var oldest int64
		for _, item := range page {
			id, at := key(item)
			if oldest == 0 || at < oldest {
				oldest = at
			}
			if seen[id] {
				continue
			}
			if maxItems > 0 && len(out) == maxItems {
				return out, fmt.Sprintf("stopped at --limit %d, more results exist (raise --limit or use --all)", maxItems), nil
			}
			seen[id] = true
			out = append(out, item)
		}
		if raw < searchCap {
			return out, "", nil
		}
		oldestDay := time.UnixMilli(oldest).UTC().Truncate(24 * time.Hour)
		next := oldestDay.Add(24 * time.Hour).Format("2006-01-02")
		// The bound must strictly move back in time, or the walk cannot finish.
		if before != "" && next >= before {
			return out, fmt.Sprintf("more than %d results on %s (UTC), results from that day and earlier skipped (narrow the query)", searchCap, oldestDay.Format("2006-01-02")), nil
		}
		before = next
	}
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
	var files multiFlag
	fs.Var(&files, "file", "attach a local file (repeatable, up to 10)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 || (fs.NArg() < 2 && len(files) == 0) {
		return fmt.Errorf("usage: mmcli reply <link|post_id> <message...> [--file PATH]...")
	}
	if err := checkAttachments(files); err != nil {
		return err
	}
	ref, err := link.Parse(fs.Arg(0))
	if err != nil {
		return err
	}
	if ref.PostID == "" {
		return fmt.Errorf("reply needs a post link or post id")
	}
	message := joinMessage(fs.Args()[1:])
	if message == "" && len(files) == 0 {
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
	fileIDs, err := uploadAll(ctx, client, target.ChannelID, files)
	if err != nil {
		return err
	}
	created, err := client.CreatePost(ctx, target.ChannelID, message, root, fileIDs)
	if err != nil {
		return err
	}
	return output.Emit(d.stdout, output.One(created, namesFor(ctx, client, created)), c.pretty)
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
	var files multiFlag
	fs.Var(&files, "file", "attach a local file (repeatable, up to 10)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 || (fs.NArg() < 2 && len(files) == 0) {
		return fmt.Errorf("usage: mmcli post <channel|~channel|@user|link> <message...> [--file PATH]...")
	}
	if err := checkAttachments(files); err != nil {
		return err
	}
	message := joinMessage(fs.Args()[1:])
	if message == "" && len(files) == 0 {
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
		if *team != "" {
			return fmt.Errorf("--team does not apply to direct messages")
		}
		channelID, err = directChannelID(ctx, client, username)
	} else {
		channelID, err = teamChannelID(ctx, client, cc, *team, fs.Arg(0))
	}
	if err != nil {
		return err
	}
	fileIDs, err := uploadAll(ctx, client, channelID, files)
	if err != nil {
		return err
	}
	created, err := client.CreatePost(ctx, channelID, message, "", fileIDs)
	if err != nil {
		return err
	}
	return output.Emit(d.stdout, output.One(created, namesFor(ctx, client, created)), c.pretty)
}

// directChannelID returns the direct channel between the caller and username,
// creating it if needed; direct channels belong to no team.
func directChannelID(ctx context.Context, client *mm.Client, username string) (string, error) {
	if username == "" {
		return "", fmt.Errorf("empty username after @")
	}
	// The server stores usernames lowercase and rejects other spellings.
	username = strings.ToLower(username)
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

// maxAttachments is the server's limit of files per post.
const maxAttachments = 10

// checkAttachments validates local files before anything is uploaded, so a
// typo never leaves a half-sent post.
func checkAttachments(paths []string) error {
	if len(paths) > maxAttachments {
		return fmt.Errorf("at most %d files per post, got %d", maxAttachments, len(paths))
	}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
	}
	return nil
}

func uploadAll(ctx context.Context, client *mm.Client, channelID string, paths []string) ([]string, error) {
	var ids []string
	for _, p := range paths {
		fi, err := client.UploadFile(ctx, channelID, p)
		if err != nil {
			return nil, err
		}
		ids = append(ids, fi.ID)
	}
	return ids, nil
}
