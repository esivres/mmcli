package mm

// Post is a single Mattermost post (subset of fields we use).
type Post struct {
	ID        string `json:"id"`
	CreateAt  int64  `json:"create_at"`
	UpdateAt  int64  `json:"update_at"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	RootID    string `json:"root_id"`
	Message   string `json:"message"`
}

// PostList is Mattermost's ordered post collection (threads, search results).
// Order lists post IDs newest-first; Posts maps ID -> Post.
type PostList struct {
	Order []string         `json:"order"`
	Posts map[string]*Post `json:"posts"`
}

// Team is a Mattermost team (subset).
type Team struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// Channel is a Mattermost channel (subset).
type Channel struct {
	ID          string `json:"id"`
	TeamID      string `json:"team_id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// User is a Mattermost user (subset).
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}
