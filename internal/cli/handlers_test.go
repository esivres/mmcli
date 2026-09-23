package cli

import "testing"

// buildTerms must place modifiers before the free-text query so Mattermost
// scopes the search; order and spacing matter for the server-side parser.
func TestBuildTerms(t *testing.T) {
	cases := []struct {
		name    string
		query   []string
		channel string
		from    string
		after   string
		before  string
		want    string
	}{
		{"query only", []string{"hello", "world"}, "", "", "", "", "hello world"},
		{"all modifiers", []string{"deploy"}, "ops", "alice", "2026-01-01", "2026-02-01",
			"in:ops from:alice after:2026-01-01 before:2026-02-01 deploy"},
		{"modifiers only", nil, "ops", "", "2026-01-01", "", "in:ops after:2026-01-01"},
		{"empty", nil, "", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildTerms(tc.query, tc.channel, tc.from, tc.after, tc.before)
			if got != tc.want {
				t.Fatalf("buildTerms = %q, want %q", got, tc.want)
			}
		})
	}
}
