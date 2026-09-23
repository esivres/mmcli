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
	Metadata  struct {
		Files []FileInfo `json:"files"`
	} `json:"metadata"`
}

// FileInfo describes an attachment (subset).
type FileInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Extension string `json:"extension"`
	Size      int64  `json:"size"`
	MimeType  string `json:"mime_type"`
	PostID    string `json:"post_id"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	CreateAt  int64  `json:"create_at"`
}

// FileInfoList is a file search result. Order is newest-first.
type FileInfoList struct {
	Order     []string             `json:"order"`
	FileInfos map[string]*FileInfo `json:"file_infos"`
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
	// Type is O (public), P (private), D (direct) or G (group message).
	Type          string `json:"type"`
	LastPostAt    int64  `json:"last_post_at"`
	TotalMsgCount int64  `json:"total_msg_count"`
}

// ChannelMember is the caller's read state in a channel (subset).
type ChannelMember struct {
	ChannelID    string `json:"channel_id"`
	MsgCount     int64  `json:"msg_count"`
	MentionCount int64  `json:"mention_count"`
}

// Thread is a followed thread with the caller's read state (subset).
type Thread struct {
	ID             string `json:"id"`
	ReplyCount     int64  `json:"reply_count"`
	LastReplyAt    int64  `json:"last_reply_at"`
	UnreadReplies  int64  `json:"unread_replies"`
	UnreadMentions int64  `json:"unread_mentions"`
	Participants   []User `json:"participants"`
	Post           *Post  `json:"post"`
}

// ThreadList is a page of followed threads, most recent reply first.
type ThreadList struct {
	Threads []Thread `json:"threads"`
}

// User is a Mattermost user (subset).
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}
