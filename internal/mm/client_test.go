package mm

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Only Mattermost's own 401 is final; a proxy or SSO page may be transient
// and must not end a long-running stream.
func TestIsUnauthorized(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{401, `{"id":"api.context.session_expired.app_error","message":"Invalid or expired session","status_code":401}`, true},
		{401, `<html>sso login</html>`, false},
		{500, `{"id":"app.error","message":"db down","status_code":500}`, false},
	}
	for _, tc := range cases {
		err := decodeAPIError(&http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))})
		if got := IsUnauthorized(err); got != tc.want {
			t.Errorf("%d %s: IsUnauthorized = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}
