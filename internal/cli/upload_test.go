package cli

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type uploadServer struct {
	mu       sync.Mutex
	uploads  []string // channel|filename|content
	postBody map[string]any
}

func newUploadServer(t *testing.T) (*uploadServer, *httptest.Server) {
	us := &uploadServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"bot1","username":"digest-bot"}`))
		case "/api/v4/users/username/alice":
			_, _ = w.Write([]byte(`{"id":"alice1","username":"alice"}`))
		case "/api/v4/channels/direct":
			_, _ = w.Write([]byte(`{"id":"dm1","name":"alice1__bot1","type":"D"}`))
		case "/api/v4/files":
			// Read parts raw: the stdlib parser would hide a leaked local path.
			mr, err := r.MultipartReader()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var channel, name, content string
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				b, _ := io.ReadAll(part)
				_, params, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
				switch params["name"] {
				case "channel_id":
					channel = string(b)
				case "files":
					// Mattermost streams uploads and needs the channel first.
					if channel == "" {
						http.Error(w, `{"message":"Expected a channel_id to precede the files"}`, http.StatusBadRequest)
						return
					}
					name, content = params["filename"], string(b)
				}
			}
			if strings.HasPrefix(name, "fail") {
				http.Error(w, `{"id":"api.file.too_large","message":"File is too large","status_code":413}`, http.StatusRequestEntityTooLarge)
				return
			}
			us.mu.Lock()
			us.uploads = append(us.uploads, channel+"|"+name+"|"+content)
			id := "file" + string(rune('0'+len(us.uploads)))
			us.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"file_infos":[{"id":"` + id + `","name":"x"}]}`))
		case "/api/v4/posts/" + postID:
			_, _ = w.Write([]byte(`{"id":"` + postID + `","user_id":"alice1","channel_id":"ops1","root_id":""}`))
		case "/api/v4/posts":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			us.mu.Lock()
			us.postBody = body
			us.mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"` + postID + `","user_id":"bot1","channel_id":"dm1","message":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return us, srv
}

func tempFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A digest sent as a file must land in the recipient's channel, under its own
// name, and the post must reference exactly the uploaded files.
func TestPostWithFiles(t *testing.T) {
	us, srv := newUploadServer(t)
	d, _, errOut := loggedIn(t, srv.URL)
	a := tempFile(t, "digest.md", "# week")
	b := tempFile(t, "build.log", "FAIL")
	if code := run(d, []string{"post", "@alice", "--file", a, "--file", b}); code != 0 {
		t.Fatalf("post exit %d: %s", code, errOut)
	}
	if got := strings.Join(us.uploads, ";"); got != "dm1|digest.md|# week;dm1|build.log|FAIL" {
		t.Fatalf("uploads %q", got)
	}
	ids, _ := us.postBody["file_ids"].([]any)
	if us.postBody["channel_id"] != "dm1" || len(ids) != 2 || ids[0] != "file1" || ids[1] != "file2" {
		t.Fatalf("post body %v", us.postBody)
	}
}

// Bad input is refused before anything reaches the server, so a typo in the
// last path never leaves the first files uploaded and dangling.
func TestPostFilesCheckedFirst(t *testing.T) {
	us, srv := newUploadServer(t)
	d, _, _ := loggedIn(t, srv.URL)
	ok := tempFile(t, "a.txt", "a")
	var eleven []string
	for i := 0; i < 11; i++ {
		eleven = append(eleven, "--file", ok)
	}
	for _, args := range [][]string{
		{"post", "@alice", "hi", "--file", ok, "--file", "/nonexistent/x"},
		{"post", "@alice", "hi", "--file", t.TempDir()},
		append([]string{"post", "@alice", "hi"}, eleven...),
		{"post", "@alice"},
	} {
		if code := run(d, args); code != 1 {
			t.Fatalf("%v: want exit 1, got %d", args, code)
		}
	}
	if len(us.uploads) != 0 || us.postBody != nil {
		t.Fatalf("server touched: uploads %v, post %v", us.uploads, us.postBody)
	}
}

// A reply carries its files into the thread's channel.
func TestReplyWithFile(t *testing.T) {
	us, srv := newUploadServer(t)
	d, _, errOut := loggedIn(t, srv.URL)
	if code := run(d, []string{"reply", postID, "лог", "--file", tempFile(t, "build.log", "FAIL")}); code != 0 {
		t.Fatalf("reply exit %d: %s", code, errOut)
	}
	if got := strings.Join(us.uploads, ";"); got != "ops1|build.log|FAIL" {
		t.Fatalf("uploads %q", got)
	}
	if us.postBody["root_id"] != postID || us.postBody["message"] != "лог" {
		t.Fatalf("post body %v", us.postBody)
	}
}

// A failed upload midway must not produce a post missing some of its files.
func TestPostNotCreatedAfterFailedUpload(t *testing.T) {
	us, srv := newUploadServer(t)
	d, _, errOut := loggedIn(t, srv.URL)
	args := []string{"post", "@alice", "три файла", "--file", tempFile(t, "a.txt", "a"), "--file", tempFile(t, "fail.bin", "x"), "--file", tempFile(t, "c.txt", "c")}
	if code := run(d, args); code != 1 || !strings.Contains(errOut.String(), "too large") {
		t.Fatalf("want exit 1 with the server's reason, got %d: %s", code, errOut)
	}
	if us.postBody != nil {
		t.Fatalf("post created despite a failed upload: %v", us.postBody)
	}
}
