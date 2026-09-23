package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/esivres/mmcli/internal/extract"
	"github.com/esivres/mmcli/internal/mm"
	"github.com/esivres/mmcli/internal/output"
)

// fileHit is one file search result.
type fileHit struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Mime    string `json:"mime,omitempty"`
	Size    int64  `json:"size"`
	Time    string `json:"time"`
	User    string `json:"user"`
	Channel string `json:"channel,omitempty"`
	PostID  string `json:"post_id"`
}

// fileResult describes a downloaded file, with its text for `file text`.
type fileResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mime string `json:"mime,omitempty"`
	Size int64  `json:"size"`
	Path string `json:"path"`
	*extract.Result
}

// cmdFile handles `file search|get|text`.
func cmdFile(d deps, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mmcli file search|get|text ...")
	}
	switch args[0] {
	case "search":
		return cmdFileSearch(d, args[1:])
	case "get":
		return cmdFileGet(d, args[1:], false)
	case "text":
		return cmdFileGet(d, args[1:], true)
	default:
		return fmt.Errorf("unknown file subcommand %q", args[0])
	}
}

func cmdFileSearch(d deps, args []string) error {
	fs := flag.NewFlagSet("file search", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var c common
	addCommon(fs, &c)
	team := fs.String("team", "", "team URL name (default: context default team)")
	channel := fs.String("channel", "", "restrict to channel (in:)")
	from := fs.String("from", "", "restrict to uploader (from:)")
	after := fs.String("after", "", "only files after date YYYY-MM-DD")
	before := fs.String("before", "", "only files before date YYYY-MM-DD")
	ext := fs.String("ext", "", "only this extension (ext:)")
	limit := fs.Int("limit", 50, "max results")
	all := fs.Bool("all", false, "fetch every result, ignoring --limit")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	query := fs.Args()
	if *ext != "" {
		query = append([]string{"ext:" + strings.TrimPrefix(*ext, ".")}, query...)
	}
	if strings.TrimSpace(buildTerms(query, *channel, *from, *after, *before)) == "" {
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
	maxFiles := *limit
	if *all {
		maxFiles = 0
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

	files, warning, err := walkBack(*before, maxFiles, func(before string) ([]*mm.FileInfo, int, error) {
		fl, err := client.SearchFiles(ctx, teamID, mm.SearchOpts{Terms: buildTerms(query, *channel, *from, *after, before), PerPage: searchCap, TimeZoneOffset: 0})
		if err != nil {
			return nil, 0, err
		}
		var page []*mm.FileInfo
		for _, id := range fl.Order {
			if f := fl.FileInfos[id]; f != nil {
				page = append(page, f)
			}
		}
		return page, len(fl.Order), nil
	}, func(f *mm.FileInfo) (string, int64) { return f.ID, f.CreateAt })
	if err != nil {
		return err
	}
	if warning != "" {
		fmt.Fprintln(d.stderr, "warning:", warning)
	}

	// Files carry the same author and channel as their post.
	posts := make([]*mm.Post, 0, len(files))
	for _, f := range files {
		posts = append(posts, &mm.Post{UserID: f.UserID, ChannelID: f.ChannelID})
	}
	names := namesFor(ctx, client, posts...)
	sort.Slice(files, func(i, j int) bool { return files[i].CreateAt < files[j].CreateAt })
	hits := make([]fileHit, 0, len(files))
	for _, f := range files {
		user := names.Users[f.UserID]
		if user == "" {
			user = f.UserID
		}
		hits = append(hits, fileHit{ID: f.ID, Name: f.Name, Mime: f.MimeType, Size: f.Size,
			Time: time.UnixMilli(f.CreateAt).Format(time.RFC3339), User: user,
			Channel: names.Channels[f.ChannelID], PostID: f.PostID})
	}
	return output.Emit(d.stdout, hits, c.pretty)
}

// cmdFileGet downloads a file into a per-file cache directory (or --out) and,
// for `file text`, extracts its text.
func cmdFileGet(d deps, args []string, text bool) error {
	name := "file get"
	if text {
		name = "file text"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var c common
	addCommon(fs, &c)
	outDir := fs.String("out", "", "directory to save into; existing files are never replaced (default: user cache dir)")
	var maxBytes *int
	if text {
		maxBytes = fs.Int("max-bytes", extract.DefaultLimits.MaxText, "bytes of text kept; the middle of longer text is cut")
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: mmcli %s <file_id>", name)
	}
	if text && *maxBytes <= 0 {
		return fmt.Errorf("--max-bytes must be positive")
	}
	id := fs.Arg(0)

	client, _, _, err := d.buildClient(c.context)
	if err != nil {
		return err
	}
	// Attachments can be large; the usual per-command timeout is too short.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	info, err := client.GetFileInfo(ctx, id)
	if err != nil {
		return err
	}
	if text && info.Size > extract.DefaultLimits.MaxFileBytes {
		return fmt.Errorf("%s is %d bytes, over the %d-byte text extraction limit; use `mmcli file get`", info.Name, info.Size, extract.DefaultLimits.MaxFileBytes)
	}
	replace := *outDir == "" // the per-file cache dir holds only this file
	dir := *outDir
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(cache, "mmcli", "files", info.ID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dest := filepath.Join(dir, safeFileName(info.Name, info.ID))
	if err := download(ctx, client, info.ID, dest, replace); err != nil {
		return err
	}

	res := fileResult{ID: info.ID, Name: info.Name, Mime: info.MimeType, Size: info.Size, Path: dest}
	if text {
		lim := extract.DefaultLimits
		lim.MaxText = *maxBytes
		r, err := extract.File(ctx, dest, info.Name, info.MimeType, lim)
		if errors.Is(err, extract.ErrImage) {
			return fmt.Errorf("%w: saved to %s", err, dest)
		}
		if err != nil {
			return err
		}
		res.Result = &r
	}
	return output.Emit(d.stdout, res, c.pretty)
}

// download writes via a temp file so a failed transfer never leaves a
// truncated file that looks complete. Without replace an existing file is an
// error: the name comes from the uploader and may be ".bashrc".
func download(ctx context.Context, client *mm.Client, id, dest string, replace bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := client.DownloadFile(ctx, id, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if replace {
		return os.Rename(tmp.Name(), dest)
	}
	if err := os.Link(tmp.Name(), dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; not replacing it", dest)
		}
		return err
	}
	return nil
}

// safeFileName keeps only the base name so a crafted name cannot escape dir.
func safeFileName(name, fallback string) string {
	base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if base == "." || base == ".." || base == "/" || base == "" {
		return fallback
	}
	return base
}
