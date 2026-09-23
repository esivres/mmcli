package cli

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esivres/mmcli/internal/secrets"
)

func docxBytes(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	_, _ = w.Write([]byte(`<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fileServer serves attachments by id with the given names and content.
func fileServer(t *testing.T, files map[string][2]string, content map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+botToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p := r.URL.Path
		switch {
		case p == "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"bot1","username":"digest-bot"}`))
		case strings.HasPrefix(p, "/api/v4/files/") && strings.HasSuffix(p, "/info"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/api/v4/files/"), "/info")
			f, ok := files[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": f[0], "mime_type": f[1], "size": len(content[id])})
		case strings.HasPrefix(p, "/api/v4/files/"):
			_, _ = w.Write(content[strings.TrimPrefix(p, "/api/v4/files/")])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func loggedIn(t *testing.T, url string) (deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	d, out, errOut := newDeps(t, secrets.NewMemory(), botToken+"\n")
	if code := run(d, []string{"login", "--context", "bot", "--url", url, "--token-stdin"}); code != 0 {
		t.Fatalf("login exit %d: %s", code, errOut)
	}
	out.Reset()
	return d, out, errOut
}

// file text must hand the agent the document's words, and file get must never
// write outside the target dir whatever name the uploader chose.
func TestFileTextAndSafeNames(t *testing.T) {
	srv := fileServer(t,
		map[string][2]string{"f1": {"../../spec.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}, "f2": {"shot.png", "image/png"}},
		map[string][]byte{"f1": docxBytes(t, "Требования к релизу"), "f2": []byte("\x89PNG")})
	d, out, errOut := loggedIn(t, srv.URL)
	dir := t.TempDir()

	if code := run(d, []string{"file", "text", "f1", "--out", dir}); code != 0 {
		t.Fatalf("file text exit %d: %s", code, errOut)
	}
	var res struct{ Path, Text string }
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Text != "Требования к релизу\n" {
		t.Fatalf("text %q", res.Text)
	}
	if res.Path != filepath.Join(dir, "spec.docx") {
		t.Fatalf("saved outside the target dir: %s", res.Path)
	}

	errOut.Reset()
	if code := run(d, []string{"file", "text", "f2", "--out", dir}); code != 1 || !strings.Contains(errOut.String(), "no OCR") {
		t.Fatalf("image must be refused with a reason: exit %d, %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "shot.png")); err != nil {
		t.Fatalf("refused image must still be saved for viewing: %v", err)
	}
}

// A failed download must not leave a file that looks complete.
func TestFileGetFailureLeavesNothing(t *testing.T) {
	srv := fileServer(t, map[string][2]string{"f1": {"a.txt", "text/plain"}}, nil)
	d, _, _ := loggedIn(t, srv.URL)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info") {
			_, _ = w.Write([]byte(`{"id":"f1","name":"a.txt","mime_type":"text/plain","size":10}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"id":"api.file.denied","message":"denied","status_code":403}`))
	})
	dir := t.TempDir()
	if code := run(d, []string{"file", "get", "f1", "--out", dir}); code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left files behind: %v", entries)
	}
}

// Attachments must be visible on posts, otherwise nobody knows to fetch them.
func TestPostListsFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"bot1","username":"digest-bot"}`))
		case "/api/v4/posts/" + postID:
			_, _ = w.Write([]byte(`{"id":"` + postID + `","user_id":"bot1","channel_id":"c1","message":"see file",
				"metadata":{"files":[{"id":"f1","name":"guide.pdf","mime_type":"application/pdf","size":1234}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	d, out, errOut := loggedIn(t, srv.URL)
	if code := run(d, []string{"get", postID}); code != 0 {
		t.Fatalf("get exit %d: %s", code, errOut)
	}
	if !strings.Contains(out.String(), `"files":[{"id":"f1","name":"guide.pdf","mime":"application/pdf","size":1234}]`) {
		t.Fatalf("files missing: %s", out)
	}
}

// File search must keep modifiers before free text, walk past the server
// cap like post search, and say where each file came from.
func TestFileSearch(t *testing.T) {
	var terms []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"bot1","username":"digest-bot"}`))
		case "/api/v4/teams/name/myteam":
			_, _ = w.Write([]byte(`{"id":"team1","name":"myteam"}`))
		case "/api/v4/teams/team1/files/search":
			var body struct{ Terms string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			terms = append(terms, body.Terms)
			_, _ = w.Write([]byte(`{"order":["f2","f1"],"file_infos":{
				"f1":{"id":"f1","name":"old.pdf","mime_type":"application/pdf","size":1,"user_id":"alice1","channel_id":"ops1","post_id":"p1","create_at":1000},
				"f2":{"id":"f2","name":"new.pdf","mime_type":"application/pdf","size":2,"user_id":"alice1","channel_id":"ops1","post_id":"p2","create_at":2000}}}`))
		case "/api/v4/channels/ops1":
			_, _ = w.Write([]byte(`{"id":"ops1","name":"ops","type":"O"}`))
		case "/api/v4/users/ids":
			_, _ = w.Write([]byte(`[{"id":"alice1","username":"alice"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	d, out, errOut := loggedIn(t, srv.URL)
	if code := run(d, []string{"file", "search", "guide", "--ext", ".pdf", "--channel", "ops", "--team", "myteam"}); code != 0 {
		t.Fatalf("file search exit %d: %s", code, errOut)
	}
	if len(terms) != 1 || terms[0] != "in:ops ext:pdf guide" {
		t.Fatalf("terms %q", terms)
	}
	var hits []struct {
		ID, User, Channel string
		PostID            string `json:"post_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ID != "f1" || hits[0].User != "alice" || hits[0].Channel != "ops" || hits[0].PostID != "p1" {
		t.Fatalf("hits %+v", hits)
	}
}
