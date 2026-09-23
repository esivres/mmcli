// Package mm is a thin Mattermost REST client covering only the endpoints
// mmcli needs: login, fetch post/thread, search, create post, and resolve
// teams/channels/users. It is intentionally not the official SDK.
package mm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Client talks to one Mattermost server. It is stateless across processes:
// a session token is cached by the caller (in the keyring) and passed back in.
// On a 401 the client re-logs-in once using loginID/password, then retries,
// and reports the fresh token via OnToken. With an empty loginID (access token
// auth) there is nothing to re-log-in with, so a 401 is returned as is.
type Client struct {
	baseURL  string
	http     *http.Client
	token    string
	loginID  string
	password string

	// OnToken, if set, is called whenever a new session token is obtained.
	OnToken func(token string)
}

// New builds a client. token may be empty (a request will then trigger login).
func New(baseURL, token, loginID, password string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		// No fixed client timeout: the per-request context governs deadlines
		// (some searches on the server are slow). See newCtx in the cli package.
		http:     &http.Client{},
		token:    token,
		loginID:  loginID,
		password: password,
	}
}

// apiError carries the Mattermost error payload for clearer messages.
type apiError struct {
	StatusCode int    `json:"status_code"`
	ID         string `json:"id"`
	Message    string `json:"message"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("mattermost API %d: %s", e.StatusCode, e.Message)
}

// Login authenticates with login_id/password and stores the session token.
func (c *Client) Login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{
		"login_id": c.loginID,
		"password": c.password,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v4/users/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeAPIError(resp)
	}
	tok := resp.Header.Get("Token")
	if tok == "" {
		return fmt.Errorf("login succeeded but no session token returned")
	}
	c.token = tok
	if c.OnToken != nil {
		c.OnToken(tok)
	}
	return nil
}

// do performs an authenticated JSON request, decoding into out (may be nil).
// It transparently re-logs-in once on a 401 if credentials are available.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.doWithRetry(ctx, method, path, in, out, true)
}

func (c *Client) doWithRetry(ctx context.Context, method, path string, in, out any, retry bool) error {
	var bodyReader io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized && retry && c.loginID != "" {
		// Cached token likely expired; re-login and retry once.
		if err := c.Login(ctx); err != nil {
			return fmt.Errorf("re-login after 401: %w", err)
		}
		return c.doWithRetry(ctx, method, path, in, out, false)
	}
	if resp.StatusCode == http.StatusUnauthorized && c.loginID == "" {
		return fmt.Errorf("access token rejected (invalid, expired or revoked): %w", decodeAPIError(resp))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// IsUnauthorized reports a rejected token or failed login from Mattermost
// itself (its errors carry an id); a proxy's 401 page may be transient.
func IsUnauthorized(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.StatusCode == http.StatusUnauthorized && e.ID != ""
}

func decodeAPIError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e apiError
	if json.Unmarshal(data, &e) == nil && e.Message != "" {
		if e.StatusCode == 0 {
			e.StatusCode = resp.StatusCode
		}
		return &e
	}
	return &apiError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(data))}
}

// GetPost fetches a single post by ID.
func (c *Client) GetPost(ctx context.Context, id string) (*Post, error) {
	var p Post
	if err := c.do(ctx, http.MethodGet, "/api/v4/posts/"+id, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetThread fetches the full thread containing the given post.
func (c *Client) GetThread(ctx context.Context, id string) (*PostList, error) {
	var pl PostList
	if err := c.do(ctx, http.MethodGet, "/api/v4/posts/"+id+"/thread", nil, &pl); err != nil {
		return nil, err
	}
	return &pl, nil
}

// SearchOpts mirrors the Mattermost search request body fields we use.
type SearchOpts struct {
	Terms      string `json:"terms"`
	IsOrSearch bool   `json:"is_or_search"`
	Page       int    `json:"page"`
	PerPage    int    `json:"per_page"`
	// TimeZoneOffset (seconds) sets the day boundaries of after:/before:.
	TimeZoneOffset int `json:"time_zone_offset"`
}

// Search runs a posts search within a team. terms may contain Mattermost
// modifiers (in:, from:, before:, after:).
func (c *Client) Search(ctx context.Context, teamID string, opts SearchOpts) (*PostList, error) {
	var pl PostList
	path := "/api/v4/teams/" + teamID + "/posts/search"
	if err := c.do(ctx, http.MethodPost, path, opts, &pl); err != nil {
		return nil, err
	}
	return &pl, nil
}

// CreatePost posts a message to a channel. rootID, if non-empty, makes it a
// threaded reply; fileIDs attach previously uploaded files.
func (c *Client) CreatePost(ctx context.Context, channelID, message, rootID string, fileIDs []string) (*Post, error) {
	body := map[string]any{
		"channel_id": channelID,
		"message":    message,
	}
	if rootID != "" {
		body["root_id"] = rootID
	}
	if len(fileIDs) > 0 {
		body["file_ids"] = fileIDs
	}
	var p Post
	if err := c.do(ctx, http.MethodPost, "/api/v4/posts", body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// DeletePost deletes a post by ID.
func (c *Client) DeletePost(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v4/posts/"+id, nil, nil)
}

// SearchFiles runs a file search (names and extracted content) within a team.
func (c *Client) SearchFiles(ctx context.Context, teamID string, opts SearchOpts) (*FileInfoList, error) {
	var fl FileInfoList
	if err := c.do(ctx, http.MethodPost, "/api/v4/teams/"+teamID+"/files/search", opts, &fl); err != nil {
		return nil, err
	}
	return &fl, nil
}

// GetFileInfo fetches attachment metadata.
func (c *Client) GetFileInfo(ctx context.Context, id string) (*FileInfo, error) {
	var fi FileInfo
	if err := c.do(ctx, http.MethodGet, "/api/v4/files/"+id+"/info", nil, &fi); err != nil {
		return nil, err
	}
	return &fi, nil
}

// DownloadFile copies an attachment's content to w, re-logging-in once on a
// 401 like other requests.
func (c *Client) DownloadFile(ctx context.Context, id string, w io.Writer) error {
	for retry := true; ; retry = false {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v4/files/"+id, nil)
		if err != nil {
			return err
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("download file %s: %w", id, err)
		}
		if resp.StatusCode == http.StatusUnauthorized && retry && c.loginID != "" {
			resp.Body.Close()
			if err := c.Login(ctx); err != nil {
				return fmt.Errorf("re-login after 401: %w", err)
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && c.loginID == "" {
			return fmt.Errorf("access token rejected (invalid, expired or revoked): %w", decodeAPIError(resp))
		}
		if resp.StatusCode != http.StatusOK {
			return decodeAPIError(resp)
		}
		if _, err := io.Copy(w, resp.Body); err != nil {
			return fmt.Errorf("download file %s: %w", id, err)
		}
		return nil
	}
}

// UploadFile uploads a local file into a channel and returns its info; the
// file is attached once a post references its ID.
func (c *Client) UploadFile(ctx context.Context, channelID, path string) (*FileInfo, error) {
	for retry := true; ; retry = false {
		resp, err := c.upload(ctx, channelID, path)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && retry && c.loginID != "" {
			resp.Body.Close()
			if err := c.Login(ctx); err != nil {
				return nil, fmt.Errorf("re-login after 401: %w", err)
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && c.loginID == "" {
			return nil, fmt.Errorf("access token rejected (invalid, expired or revoked): %w", decodeAPIError(resp))
		}
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("upload %s: %w", filepath.Base(path), decodeAPIError(resp))
		}
		var out struct {
			FileInfos []FileInfo `json:"file_infos"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("decode upload response: %w", err)
		}
		if len(out.FileInfos) != 1 {
			return nil, fmt.Errorf("upload %s: server returned %d file infos", filepath.Base(path), len(out.FileInfos))
		}
		return &out.FileInfos[0], nil
	}
}

// upload streams path as multipart form data, so large files are not held
// in memory.
func (c *Client) upload(ctx context.Context, channelID, path string) (*http.Response, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer f.Close()
		err := mw.WriteField("channel_id", channelID)
		if err == nil {
			var part io.Writer
			part, err = mw.CreateFormFile("files", filepath.Base(path))
			if err == nil {
				_, err = io.Copy(part, f)
			}
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v4/files", pr)
	if err != nil {
		pr.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload %s: %w", filepath.Base(path), err)
	}
	return resp, nil
}

// GetTeamByName resolves a team by its URL name.
func (c *Client) GetTeamByName(ctx context.Context, name string) (*Team, error) {
	var t Team
	if err := c.do(ctx, http.MethodGet, "/api/v4/teams/name/"+name, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetChannel fetches a channel by ID.
func (c *Client) GetChannel(ctx context.Context, id string) (*Channel, error) {
	var ch Channel
	if err := c.do(ctx, http.MethodGet, "/api/v4/channels/"+id, nil, &ch); err != nil {
		return nil, err
	}
	return &ch, nil
}

// GetChannelByName resolves a channel by name within a team.
func (c *Client) GetChannelByName(ctx context.Context, teamID, name string) (*Channel, error) {
	var ch Channel
	path := "/api/v4/teams/" + teamID + "/channels/name/" + name
	if err := c.do(ctx, http.MethodGet, path, nil, &ch); err != nil {
		return nil, err
	}
	return &ch, nil
}

// Me returns the user the current token belongs to.
func (c *Client) Me(ctx context.Context) (*User, error) {
	var u User
	if err := c.do(ctx, http.MethodGet, "/api/v4/users/me", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByUsername resolves a user by username.
func (c *Client) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	var u User
	if err := c.do(ctx, http.MethodGet, "/api/v4/users/username/"+username, nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateDirectChannel returns the direct channel between two users; the server
// returns the existing one if it was created before.
func (c *Client) CreateDirectChannel(ctx context.Context, userID, otherID string) (*Channel, error) {
	var ch Channel
	if err := c.do(ctx, http.MethodPost, "/api/v4/channels/direct", []string{userID, otherID}, &ch); err != nil {
		return nil, err
	}
	return &ch, nil
}

// UsersByIDs resolves usernames for a set of user IDs.
func (c *Client) UsersByIDs(ctx context.Context, ids []string) ([]User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var users []User
	if err := c.do(ctx, http.MethodPost, "/api/v4/users/ids", ids, &users); err != nil {
		return nil, err
	}
	return users, nil
}

// MyTeams lists the teams the caller belongs to.
func (c *Client) MyTeams(ctx context.Context) ([]Team, error) {
	var teams []Team
	if err := c.do(ctx, http.MethodGet, "/api/v4/users/me/teams", nil, &teams); err != nil {
		return nil, err
	}
	return teams, nil
}

// MyChannels lists the caller's channels in a team, direct and group
// messages included.
func (c *Client) MyChannels(ctx context.Context, teamID string) ([]Channel, error) {
	var chs []Channel
	if err := c.do(ctx, http.MethodGet, "/api/v4/users/me/teams/"+teamID+"/channels", nil, &chs); err != nil {
		return nil, err
	}
	return chs, nil
}

// MyChannelMembers returns the caller's read state for every channel of MyChannels.
func (c *Client) MyChannelMembers(ctx context.Context, teamID string) ([]ChannelMember, error) {
	var ms []ChannelMember
	if err := c.do(ctx, http.MethodGet, "/api/v4/users/me/teams/"+teamID+"/channels/members", nil, &ms); err != nil {
		return nil, err
	}
	return ms, nil
}

// ThreadsPerPage is the server's maximum page size for followed threads.
const ThreadsPerPage = 200

// MyThreads returns one page of threads the caller follows, most recent reply
// first. since (ms, 0 = any) filters on the thread's last update, a superset
// of its last reply; before is the last thread ID of the previous page.
func (c *Client) MyThreads(ctx context.Context, teamID string, since int64, before string, unread bool) (*ThreadList, error) {
	// Without extended the participants come back with empty usernames.
	q := url.Values{"per_page": {strconv.Itoa(ThreadsPerPage)}, "extended": {"true"}}
	if since > 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	if before != "" {
		q.Set("before", before)
	}
	if unread {
		q.Set("unread", "true")
	}
	var tl ThreadList
	if err := c.do(ctx, http.MethodGet, "/api/v4/users/me/teams/"+teamID+"/threads?"+q.Encode(), nil, &tl); err != nil {
		return nil, err
	}
	return &tl, nil
}
