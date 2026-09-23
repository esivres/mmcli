// Package link parses Mattermost references into actionable parts: a post ID,
// a channel name, and (when present) a team name. It accepts permalinks, plain
// channel URLs, and bare 26-char Mattermost IDs.
package link

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// idRe matches a bare Mattermost ID (26 base-32-ish chars).
var idRe = regexp.MustCompile(`^[a-z0-9]{26}$`)

// Ref is the parsed result. Exactly one of PostID / ChannelName is meaningful
// depending on the input; Team is set when the URL carried it.
type Ref struct {
	Team        string // team URL name, may be empty
	PostID      string // set for /pl/<id> links or bare post IDs
	ChannelName string // set for /channels/<name> links
}

// IsID reports whether s looks like a bare Mattermost ID.
func IsID(s string) bool { return idRe.MatchString(s) }

// Parse interprets a permalink, channel URL, or bare ID.
//
// Supported forms:
//
//	https://host/<team>/pl/<post_id>        -> {Team, PostID}
//	https://host/<team>/channels/<name>     -> {Team, ChannelName}
//	<post_id>                                -> {PostID}
func Parse(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, fmt.Errorf("empty reference")
	}
	if IsID(s) {
		return Ref{PostID: s}, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return Ref{}, fmt.Errorf("not a Mattermost link or post ID: %q", s)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// Expect: <team>/<kind>/<value>
	if len(parts) >= 3 {
		team, kind, value := parts[0], parts[1], parts[2]
		switch kind {
		case "pl":
			if !IsID(value) {
				return Ref{}, fmt.Errorf("permalink post id looks invalid: %q", value)
			}
			return Ref{Team: team, PostID: value}, nil
		case "channels":
			return Ref{Team: team, ChannelName: value}, nil
		}
	}
	return Ref{}, fmt.Errorf("unrecognized Mattermost URL path: %q", u.Path)
}
