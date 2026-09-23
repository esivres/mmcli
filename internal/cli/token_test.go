package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/esivres/mmcli/internal/secrets"
)

const (
	botToken = "bot-pat-123"
	postID   = "abcdefghijklmnopqrstuvwxyz"
)

// fakeServer accepts only botToken and counts password-login attempts, which a
// bot account can never pass.
func fakeServer(t *testing.T, logins *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/users/login" {
			logins.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status_code":401,"message":"bots cannot log in"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+botToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status_code":401,"message":"Invalid or expired session"}`))
			return
		}
		switch r.URL.Path {
		case "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"u1","username":"digest-bot"}`))
		case "/api/v4/posts/" + postID:
			_, _ = w.Write([]byte(`{"id":"` + postID + `","user_id":"u1","channel_id":"c1","message":"hi"}`))
		case "/api/v4/users/ids":
			_, _ = w.Write([]byte(`[{"id":"u1","username":"digest-bot"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newDeps(t *testing.T, store secrets.Store, stdin string) (deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	return deps{store: store, stdin: strings.NewReader(stdin), stdout: &out, stderr: &errOut}, &out, &errOut
}

// A bot context must work on the token alone: no password is stored and the
// client never tries /users/login, which bot accounts cannot pass.
func TestTokenLoginThenRead(t *testing.T) {
	var logins atomic.Int32
	srv := fakeServer(t, &logins)
	store := secrets.NewMemory()
	_ = store.Set(secrets.PasswordKey("bot"), "stale")
	d, out, errOut := newDeps(t, store, botToken+"\n")

	if code := run(d, []string{"login", "--context", "bot", "--url", srv.URL, "--token-stdin"}); code != 0 {
		t.Fatalf("login exit %d: %s", code, errOut)
	}
	if !strings.Contains(out.String(), `"login":"digest-bot"`) {
		t.Fatalf("login should report the token owner, got %s", out)
	}
	if _, err := store.Get(secrets.PasswordKey("bot")); err == nil {
		t.Fatal("stale password must be dropped for a token context")
	}

	out.Reset()
	if code := run(d, []string{"get", postID, "--context", "bot"}); code != 0 {
		t.Fatalf("get exit %d: %s", code, errOut)
	}
	if !strings.Contains(out.String(), `"message":"hi"`) {
		t.Fatalf("unexpected get output %s", out)
	}
	if n := logins.Load(); n != 0 {
		t.Fatalf("token context attempted %d password logins", n)
	}
}

// A revoked token must fail loudly instead of attempting a password re-login.
func TestRevokedTokenFailsWithoutRelogin(t *testing.T) {
	var logins atomic.Int32
	srv := fakeServer(t, &logins)
	store := secrets.NewMemory()
	d, _, errOut := newDeps(t, store, botToken+"\n")
	if code := run(d, []string{"login", "--context", "bot", "--url", srv.URL, "--token-stdin"}); code != 0 {
		t.Fatalf("login exit %d: %s", code, errOut)
	}
	_ = store.Set(secrets.TokenKey("bot"), "revoked")

	if code := run(d, []string{"get", postID, "--context", "bot"}); code != 1 {
		t.Fatalf("want exit 1 for revoked token, got %d", code)
	}
	if !strings.Contains(errOut.String(), "access token rejected") {
		t.Fatalf("error should name the rejected token, got %s", errOut)
	}
	if n := logins.Load(); n != 0 {
		t.Fatalf("revoked token triggered %d password logins", n)
	}
}

// An invalid token must not leave a half-configured context behind.
func TestTokenLoginRejectedSavesNothing(t *testing.T) {
	var logins atomic.Int32
	srv := fakeServer(t, &logins)
	store := secrets.NewMemory()
	d, _, errOut := newDeps(t, store, "wrong\n")

	if code := run(d, []string{"login", "--context", "bot", "--url", srv.URL, "--token-stdin"}); code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if _, err := store.Get(secrets.TokenKey("bot")); err == nil {
		t.Fatal("rejected token must not be stored")
	}
	errOut.Reset()
	if code := run(d, []string{"get", postID, "--context", "bot"}); code != 1 || !strings.Contains(errOut.String(), "not found") {
		t.Fatalf("context must not exist after rejected login: exit %d, %s", code, errOut)
	}
}
