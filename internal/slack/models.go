package slack

import "strings"

// User is a Slack workspace member as returned by users.list. Only the fields
// the matcher and the digest need are modelled.
type User struct {
	ID        string  `json:"id"`
	TeamID    string  `json:"team_id"`
	Name      string  `json:"name"` // the @handle
	RealName  string  `json:"real_name"`
	Deleted   bool    `json:"deleted"`
	IsBot     bool    `json:"is_bot"`
	IsAppUser bool    `json:"is_app_user"`
	Profile   Profile `json:"profile"`
}

// Profile is the profile sub-object of a Slack user. Email is only populated
// when the token carries the users:read.email scope.
type Profile struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	RealName    string `json:"real_name"`
}

// DisplayName is the best human label for u: the display name people chose,
// falling back to their real name and finally to the handle.
func (u User) DisplayName() string {
	if s := strings.TrimSpace(u.Profile.DisplayName); s != "" {
		return s
	}
	if s := strings.TrimSpace(u.Profile.RealName); s != "" {
		return s
	}
	if s := strings.TrimSpace(u.RealName); s != "" {
		return s
	}
	return strings.TrimSpace(u.Name)
}

// realName returns the user's real name from either place Slack puts it.
func (u User) realName() string {
	if s := strings.TrimSpace(u.Profile.RealName); s != "" {
		return s
	}
	return strings.TrimSpace(u.RealName)
}

// AuthInfo is the auth.test response: who the token belongs to.
type AuthInfo struct {
	URL    string `json:"url"`
	Team   string `json:"team"`
	User   string `json:"user"`
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
}

// Conversation is the conversations.info channel object.
type Conversation struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsChannel  bool   `json:"is_channel"`
	IsGroup    bool   `json:"is_group"`
	IsPrivate  bool   `json:"is_private"`
	IsArchived bool   `json:"is_archived"`
	IsMember   bool   `json:"is_member"`
}

// PostMessageRequest is the chat.postMessage payload. Text is the notification
// fallback and is required whenever Blocks are used.
type PostMessageRequest struct {
	Channel     string  `json:"channel"`
	Text        string  `json:"text"`
	Blocks      []Block `json:"blocks,omitempty"`
	UnfurlLinks bool    `json:"unfurl_links"`
	UnfurlMedia bool    `json:"unfurl_media"`
}

// PostMessageResult is the chat.postMessage response. TS identifies the posted
// message and is persisted so a re-run can tell a delivery apart from a resend.
type PostMessageResult struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}
