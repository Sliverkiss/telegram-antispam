// Package telegram is a stdlib-only Bot API client covering the narrow
// surface the antispam bot needs. There is no getMe-style polling loop here;
// the bot package drives polling. Errors carry the API's "description".
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIError is a non-2xx Bot API response (or a malformed 200 envelope).
type APIError struct {
	Code        int
	Description string
	Method      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram api %s: %s (code %d)", e.Method, e.Description, e.Code)
}

// Update is the subset of a Telegram Update the bot cares about.
type Update struct {
	UpdateID     int64             `json:"update_id"`
	Message      *Message          `json:"message"`
	ChatMember   *ChatMemberUpdate `json:"chat_member"`
	MyChatMember *ChatMemberUpdate `json:"my_chat_member"`
}

// ChatMemberUpdate is the chat_member / my_chat_member update body.
type ChatMemberUpdate struct {
	Chat          Chat       `json:"chat"`
	From          User       `json:"from"`
	NewChatMember ChatMember `json:"new_chat_member"`
	OldChatMember ChatMember `json:"old_chat_member"`
}

// ChatMember describes a (new or old) membership state.
type ChatMember struct {
	Status string `json:"status"` // member, left, kicked, administrator, creator, restricted
	User   User   `json:"user"`
}

// Message is the subset of a Telegram message used by the bot.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
	// NewChatMembers lists users that just joined via this service message.
	NewChatMembers []User `json:"new_chat_members"`
	// LeftChatMember marks a leave service message.
	LeftChatMember *User `json:"left_chat_member"`
}

// User is the subset of a Telegram user.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// Chat is the subset of a Telegram chat.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// ChatInfo is the getChat result (bio lives here, not on User).
type ChatInfo struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
	Bio       string `json:"bio"`
}

// Client talks to the Bot API. Config is the only knob; the HTTP client is
// injectable for tests (and to tune timeouts in production).
type Client struct {
	Token string
	Base  string
	HTTP  *http.Client
}

// NewClient builds a client for the given token. base defaults to
// https://api.telegram.org.
func NewClient(token string) *Client {
	return &Client{Token: token, Base: "https://api.telegram.org", HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// call performs one method call, decoding ok/result or returning an APIError
// carrying the Bot API description.
func (c *Client) call(ctx context.Context, method string, params url.Values, out any) error {
	endpoint := fmt.Sprintf("%s/bot%s/%s", strings.TrimRight(c.Base, "/"), c.Token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &APIError{Method: method, Description: err.Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return &APIError{Method: method, Description: "read body: " + err.Error()}
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return &APIError{Method: method, Description: "malformed envelope: " + err.Error(), Code: resp.StatusCode}
	}
	if !env.OK {
		return &APIError{Method: method, Description: env.Description, Code: env.ErrorCode}
	}
	if out != nil && len(env.Result) > 0 && string(env.Result) != "null" {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return &APIError{Method: method, Description: "decode result: " + err.Error()}
		}
	}
	return nil
}

// GetMe returns the bot's own User record.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", nil, &u)
	return u, err
}

// GetUpdates long-polls for updates. timeout is the long-poll seconds.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int, allowed []string) ([]Update, error) {
	params := url.Values{}
	if offset > 0 {
		params.Set("offset", strconv.FormatInt(offset, 10))
	}
	if timeout > 0 {
		params.Set("timeout", strconv.Itoa(timeout))
	}
	if len(allowed) > 0 {
		params.Set("allowed_updates", strings.Join(allowed, ","))
	}
	var updates []Update
	if err := c.call(ctx, "getUpdates", params, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// GetChat returns chat info (including bio when the chat is private/user).
func (c *Client) GetChat(ctx context.Context, chatID int64) (ChatInfo, error) {
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(chatID, 10))
	var info ChatInfo
	err := c.call(ctx, "getChat", params, &info)
	return info, err
}

// GetChatMember returns the member state of userID in chatID.
func (c *Client) GetChatMember(ctx context.Context, chatID, userID int64) (ChatMember, error) {
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(chatID, 10))
	params.Set("user_id", strconv.FormatInt(userID, 10))
	var m ChatMember
	err := c.call(ctx, "getChatMember", params, &m)
	return m, err
}

// BanChatMember bans a user from a group.
func (c *Client) BanChatMember(ctx context.Context, chatID, userID int64) error {
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(chatID, 10))
	params.Set("user_id", strconv.FormatInt(userID, 10))
	return c.call(ctx, "banChatMember", params, nil)
}

// UnbanChatMember unbans (and, after a kick, allows rejoin) a user.
func (c *Client) UnbanChatMember(ctx context.Context, chatID, userID int64) error {
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(chatID, 10))
	params.Set("user_id", strconv.FormatInt(userID, 10))
	return c.call(ctx, "unbanChatMember", params, nil)
}

// DeleteMessage deletes one message; >48h old messages or non-admin bots
// fail (returned as an error the caller logs and ignores).
func (c *Client) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(chatID, 10))
	params.Set("message_id", strconv.FormatInt(messageID, 10))
	return c.call(ctx, "deleteMessage", params, nil)
}

// SendMessage sends a text message (e.g. a notification to BOT_LOG_CHAT).
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(chatID, 10))
	params.Set("text", text)
	var msg Message
	if err := c.call(ctx, "sendMessage", params, &msg); err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

// IsRetriable reports whether a getUpdates error deserves a backoff+retry
// (network errors and 409 conflict lookalikes / 5xx); user-action errors
// (400 etc.) do not.
func IsRetriable(err error) bool {
	var api *APIError
	if errors.As(err, &api) {
		return api.Code == 409 || api.Code == 429 || api.Code >= 500
	}
	return true // transport/timeout errors are retriable
}
