// Package telegram is a thin wrapper over github.com/go-telegram/bot v1.27.0.
// It owns the Bot API types (Update, Chat, User, ChatMember, Message...) and
// exposes a narrow Client interface so the bot decision layer never sees a
// library model. Library models are translated at this boundary; callers get
// plain structs and our own error type carrying Telegram's "description".
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// APIError is a non-2xx Bot API response (or a transport failure). Description
// carries Telegram's human-readable reason when available.
type APIError struct {
	Code        int
	Description string
	Method      string
	// RetryAfter is Telegram's 429 parameters.retry_after (seconds; 0 = unset).
	RetryAfter int
}

func (e *APIError) Error() string {
	if e.Method != "" {
		return fmt.Sprintf("telegram api %s: %s (code %d)", e.Method, e.Description, e.Code)
	}
	return fmt.Sprintf("telegram api: %s (code %d)", e.Description, e.Code)
}

// ---------------------------------------------------------------------------
// Domain types (translated from library models; no library types leak out).
// ---------------------------------------------------------------------------

// Update is the subset of a Telegram Update the bot cares about.
type Update struct {
	UpdateID      int64
	Message       *Message
	EditedMessage *Message
	ChatMember    *ChatMemberUpdate
	MyChatMember  *ChatMemberUpdate
}

// ChatMemberUpdate is the chat_member / my_chat_member update body.
type ChatMemberUpdate struct {
	Chat          Chat
	From          User
	Date          int
	NewChatMember ChatMember
	OldChatMember ChatMember
}

// ChatMember describes one membership state, flattened from the library's
// discriminated union so a caller only reads Status / User.
type ChatMember struct {
	Status string // member, left, kicked, administrator, creator, restricted
	User   User
}

// Message is the subset of a Telegram message used by the bot.
type Message struct {
	MessageID int64
	From      *User
	Chat      Chat
	Text      string
	// Caption is the text attached to media messages (photo/document…).
	Caption string
	// Photo carries the photo sizes when the message is a photo (largest
	// selection happens in the bot layer; here we keep all sizes).
	Photo []PhotoSize
	// Document is set for file documents (image documents are judged too).
	Document *Document
	// ReplyToMessage is set when this message replies to another.
	ReplyToMessage *Message
	// NewChatMembers lists users that just joined via this service message.
	NewChatMembers []User
	// LeftChatMember marks a leave service message.
	LeftChatMember *User
}

// PhotoSize is one photo rendition.
type PhotoSize struct {
	FileID       string
	FileUniqueID string
	Width        int
	Height       int
	FileSize     int64
}

// Document is a generic file attachment.
type Document struct {
	FileID   string
	FileName string
	MimeType string
	FileSize int64
}

// User is the subset of a Telegram user.
type User struct {
	ID        int64
	IsBot     bool
	FirstName string
	LastName  string
	Username  string
}

// Chat is the subset of a Telegram chat.
type Chat struct {
	ID    int64
	Type  string // private, group, supergroup, channel
	Title string
}

// ChatInfo is the getChat result (bio lives here, not on User).
type ChatInfo struct {
	ID        int64
	FirstName string
	LastName  string
	Username  string
	Bio       string
}

// ---------------------------------------------------------------------------
// Update conversion (library → domain).
// ---------------------------------------------------------------------------

func convUpdate(u *models.Update) Update {
	out := Update{UpdateID: u.ID}
	if u.Message != nil {
		out.Message = convMessagePtr(u.Message)
	}
	if u.EditedMessage != nil {
		out.EditedMessage = convMessagePtr(u.EditedMessage)
	}
	if u.ChatMember != nil {
		out.ChatMember = convChatMemberUpdate(u.ChatMember)
	}
	if u.MyChatMember != nil {
		out.MyChatMember = convChatMemberUpdate(u.MyChatMember)
	}
	return out
}

func convMessagePtr(m *models.Message) *Message {
	msg := convMessage(*m)
	return &msg
}

func convMessage(m models.Message) Message {
	out := Message{
		MessageID: int64(m.ID),
		Chat:      convChat(m.Chat),
		Text:      m.Text,
		Caption:   m.Caption,
	}
	if m.From != nil {
		u := convUser(*m.From)
		out.From = &u
	}
	for _, p := range m.Photo {
		out.Photo = append(out.Photo, PhotoSize{
			FileID:       p.FileID,
			FileUniqueID: p.FileUniqueID,
			Width:        p.Width,
			Height:       p.Height,
			FileSize:     int64(p.FileSize),
		})
	}
	if m.Document != nil {
		out.Document = &Document{
			FileID:   m.Document.FileID,
			FileName: m.Document.FileName,
			MimeType: m.Document.MimeType,
			FileSize: int64(m.Document.FileSize),
		}
	}
	if m.ReplyToMessage != nil {
		r := convMessage(*m.ReplyToMessage)
		out.ReplyToMessage = &r
	}
	for _, u := range m.NewChatMembers {
		out.NewChatMembers = append(out.NewChatMembers, convUser(u))
	}
	if m.LeftChatMember != nil {
		lcm := convUser(*m.LeftChatMember)
		out.LeftChatMember = &lcm
	}
	return out
}

func convUser(u models.User) User {
	return User{ID: u.ID, IsBot: u.IsBot, FirstName: u.FirstName, LastName: u.LastName, Username: u.Username}
}

func convChat(c models.Chat) Chat {
	return Chat{ID: c.ID, Type: string(c.Type), Title: c.Title}
}

func convChatMemberUpdate(u *models.ChatMemberUpdated) *ChatMemberUpdate {
	out := &ChatMemberUpdate{
		Chat:          convChat(u.Chat),
		From:          convUser(u.From),
		Date:          u.Date,
		NewChatMember: convChatMember(u.NewChatMember),
		OldChatMember: convChatMember(u.OldChatMember),
	}
	return out
}

func convChatMember(m models.ChatMember) ChatMember {
	out := ChatMember{Status: string(m.Type)}
	// Pull the user from whichever variant populated the union.
	users := []*models.User{}
	switch m.Type {
	case models.ChatMemberTypeOwner:
		if m.Owner != nil {
			users = append(users, m.Owner.User)
		}
	case models.ChatMemberTypeAdministrator:
		if m.Administrator != nil {
			u := m.Administrator.User
			users = append(users, &u)
		}
	case models.ChatMemberTypeMember:
		if m.Member != nil {
			users = append(users, m.Member.User)
		}
	case models.ChatMemberTypeRestricted:
		if m.Restricted != nil {
			users = append(users, m.Restricted.User)
		}
	case models.ChatMemberTypeLeft:
		if m.Left != nil {
			users = append(users, m.Left.User)
		}
	case models.ChatMemberTypeBanned:
		if m.Banned != nil {
			users = append(users, m.Banned.User)
		}
	}
	for _, u := range users {
		if u != nil {
			out.User = convUser(*u)
			break
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Client (library-backed).
// ---------------------------------------------------------------------------

// Client talks to the Bot API through github.com/go-telegram/bot.
type Client struct {
	b *tgbot.Bot
	// updateWorkers records the worker count handed to the library (0 = the
	// library default of 1). The library keeps the live field private, so the
	// configured value is kept here for tests and diagnostics.
	updateWorkers int
}

// UpdateWorkers reports the configured update-dispatch worker count
// (0 = the library default of 1).
func (c *Client) UpdateWorkers() int { return c.updateWorkers }

// Options configure the underlying library client. ServerURL is used by tests
// to point at an httptest server (fully offline); SkipGetMe avoids the
// library's init-time getMe on an empty/fake server.
type Options struct {
	ServerURL string
	SkipGetMe bool
	// PollTimeout is the long-poll seconds; 0 = library default (1m).
	PollTimeout int
	// UpdateWorkers is the number of library update-dispatch goroutines
	// (amend6 A.1: go-telegram/bot defaults to 1 — one slow handler blocks
	// every other update). 0 = library default (1). Range-clamped by the
	// config layer (1..128).
	UpdateWorkers  int
	AllowedUpdates []string
	ErrorsHandler  func(error)
}

// NewClient builds a client for the given token using default opts (real
// api.telegram.org endpoint, getMe checked at init). The library default of a
// single update worker stays in effect here; main passes BOT_UPDATE_WORKERS
// through NewClientWithOptions so a slow handler cannot serialize the whole
// update stream (amend6 A.1).
func NewClient(token string) (*Client, error) {
	return NewClientWithOptions(token, Options{})
}

// NewClientWithOptions builds a client honoring the given options.
func NewClientWithOptions(token string, opts Options) (*Client, error) {
	optsT := []tgbot.Option{}
	if opts.ServerURL != "" {
		optsT = append(optsT, tgbot.WithServerURL(opts.ServerURL))
	}
	if opts.SkipGetMe {
		optsT = append(optsT, tgbot.WithSkipGetMe())
	}
	if opts.ErrorsHandler != nil {
		optsT = append(optsT, tgbot.WithErrorsHandler(opts.ErrorsHandler))
	}
	if len(opts.AllowedUpdates) > 0 {
		optsT = append(optsT, tgbot.WithAllowedUpdates(opts.AllowedUpdates))
	}
	if opts.UpdateWorkers > 0 {
		optsT = append(optsT, tgbot.WithWorkers(opts.UpdateWorkers))
	}
	b, err := tgbot.New(token, optsT...)
	if err != nil {
		return nil, err
	}
	return &Client{b: b, updateWorkers: opts.UpdateWorkers}, nil
}

func (c *Client) toAPIError(method string, err error) error {
	if err == nil {
		return nil
	}
	// Map library sentinel/text-bearing errors into our APIError carrying the
	// Telegram description (and 429 retry_after when present).
	desc := err.Error()
	code := 0
	retryAfter := 0
	var tooMany *tgbot.TooManyRequestsError
	if errors.As(err, &tooMany) {
		code = 429
		retryAfter = tooMany.RetryAfter
	}
	switch {
	case errors.Is(err, tgbot.ErrorBadRequest):
		code = 400
	case errors.Is(err, tgbot.ErrorUnauthorized):
		code = 401
	case errors.Is(err, tgbot.ErrorForbidden):
		code = 403
	case errors.Is(err, tgbot.ErrorNotFound):
		code = 404
	case errors.Is(err, tgbot.ErrorConflict):
		code = 409
	case errors.Is(err, tgbot.ErrorTooManyRequests):
		if code == 0 {
			code = 429
		}
	case tgbot.IsMigrateError(err):
		code = 400
	}
	// The library formats "error response from telegram for method X, <code> <desc>"
	// for non-2xx; extract a stable description and keep the code.
	if i := strings.LastIndex(desc, ": "); i >= 0 && code == 0 {
		desc = strings.TrimSpace(desc[i+2:])
	}
	return &APIError{Code: code, Description: desc, Method: method, RetryAfter: retryAfter}
}

// GetMe returns the bot's own User record.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	u, err := c.b.GetMe(ctx)
	if err != nil {
		return User{}, c.toAPIError("getMe", err)
	}
	if u == nil {
		return User{}, nil
	}
	return convUser(*u), nil
}

// Start runs the library's long-polling loop (getUpdates + worker dispatch)
// until ctx is canceled. Install a handler with RegisterDefaultHandler first.
func (c *Client) Start(ctx context.Context) {
	c.b.Start(ctx)
}

// Close stops the polling loop and shutdowns the bot client (best-effort).
func (c *Client) Close(ctx context.Context) {
	_, _ = c.b.Close(ctx)
}

// RegisterDefaultHandler installs the handler the library invokes for updates
// that match no other handler (we use it as our single dispatch point).
func (c *Client) RegisterDefaultHandler(fn func(ctx context.Context, u Update)) {
	c.b.RegisterHandlerMatchFunc(func(*models.Update) bool { return true }, func(ctx context.Context, _ *tgbot.Bot, u *models.Update) {
		fn(ctx, convUpdate(u))
	})
}

// GetChat returns chat info (including bio when the chat is private/user).
func (c *Client) GetChat(ctx context.Context, chatID int64) (ChatInfo, error) {
	info, err := c.b.GetChat(ctx, &tgbot.GetChatParams{ChatID: chatID})
	if err != nil {
		return ChatInfo{}, c.toAPIError("getChat", err)
	}
	if info == nil {
		return ChatInfo{}, nil
	}
	return ChatInfo{ID: info.ID, FirstName: info.FirstName, LastName: info.LastName, Username: info.Username, Bio: info.Bio}, nil
}

// GetChatMember returns the member state of userID in chatID.
func (c *Client) GetChatMember(ctx context.Context, chatID, userID int64) (ChatMember, error) {
	m, err := c.b.GetChatMember(ctx, &tgbot.GetChatMemberParams{ChatID: chatID, UserID: userID})
	if err != nil {
		return ChatMember{}, c.toAPIError("getChatMember", err)
	}
	if m == nil {
		return ChatMember{}, nil
	}
	return convChatMember(*m), nil
}

// BanChatMember bans a user from a group.
func (c *Client) BanChatMember(ctx context.Context, chatID, userID int64) error {
	_, err := c.b.BanChatMember(ctx, &tgbot.BanChatMemberParams{ChatID: chatID, UserID: userID})
	return c.toAPIError("banChatMember", err)
}

// UnbanChatMember unbans (and, after a kick, allows rejoin) a user.
func (c *Client) UnbanChatMember(ctx context.Context, chatID, userID int64) error {
	_, err := c.b.UnbanChatMember(ctx, &tgbot.UnbanChatMemberParams{ChatID: chatID, UserID: userID})
	return c.toAPIError("unbanChatMember", err)
}

// DeleteMessage deletes one message; >48h old messages or non-admin bots fail
// (returned as an error the caller logs and ignores).
func (c *Client) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	_, err := c.b.DeleteMessage(ctx, &tgbot.DeleteMessageParams{ChatID: chatID, MessageID: int(messageID)})
	return c.toAPIError("deleteMessage", err)
}

// SendMessage sends a text message (e.g. a notification to BOT_LOG_CHAT).
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	m, err := c.b.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text})
	if err != nil {
		return 0, c.toAPIError("sendMessage", err)
	}
	if m == nil {
		return 0, nil
	}
	return int64(m.ID), nil
}

// NotifySender sends one notification message as PLAIN TEXT (amend4 §2.4):
// no parse_mode is ever set (ad text is full of Markdown/HTML specials), the
// link preview is explicitly disabled, and the caller is expected to have
// clipped to Telegram's 4096 limit. disable_notification stays false (the
// channel push should have a notification sound).
func (c *Client) NotifySender(ctx context.Context, chatID int64, text string) (int64, error) {
	previewDisabled := true
	m, err := c.b.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:             chatID,
		Text:               text,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: &previewDisabled},
	})
	if err != nil {
		return 0, c.toAPIError("sendMessage", err)
	}
	if m == nil {
		return 0, nil
	}
	return int64(m.ID), nil
}

// LeaveChat makes the bot leave a chat.
func (c *Client) LeaveChat(ctx context.Context, chatID int64) error {
	_, err := c.b.LeaveChat(ctx, &tgbot.LeaveChatParams{ChatID: chatID})
	return c.toAPIError("leaveChat", err)
}

// GetFileMetadata returns file metadata (file_id → file_path) via getFile.
func (c *Client) GetFileMetadata(ctx context.Context, fileID string) (FileMeta, error) {
	f, err := c.b.GetFile(ctx, &tgbot.GetFileParams{FileID: fileID})
	if err != nil {
		return FileMeta{}, c.toAPIError("getFile", err)
	}
	if f == nil {
		return FileMeta{}, nil
	}
	return FileMeta{FileID: f.FileID, FilePath: f.FilePath, FileSize: int64(f.FileSize)}, nil
}

// FileMeta mirrors models.File for the download path.
type FileMeta struct {
	FileID   string
	FilePath string
	FileSize int64
}

// DownloadFile downloads a file (photo/image document, ≤ maxBytes) over the
// library's file endpoint. Larger files return an error without allocating.
func (c *Client) DownloadFile(ctx context.Context, meta FileMeta, maxBytes int64) ([]byte, error) {
	if meta.FilePath == "" {
		return nil, &APIError{Code: 400, Description: "file path unavailable", Method: "downloadFile"}
	}
	if maxBytes > 0 && meta.FileSize > maxBytes {
		return nil, &APIError{Code: 413, Description: "file too large: " + strconv.FormatInt(meta.FileSize, 10), Method: "downloadFile"}
	}
	link := c.b.FileDownloadLink(&models.File{FileID: meta.FileID, FilePath: meta.FilePath})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Code: resp.StatusCode, Description: "download http " + strconv.Itoa(resp.StatusCode), Method: "downloadFile"}
	}
	// Read at most maxBytes+1: one byte over the cap proves oversize.
	limit := maxBytes
	if limit <= 0 {
		limit = 20 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, &APIError{Code: 413, Description: "file too large (streamed)", Method: "downloadFile"}
	}
	return data, nil
}

// SetMyCommands registers the bot command list.
func (c *Client) SetMyCommands(ctx context.Context, cmds []BotCommand) error {
	params := &tgbot.SetMyCommandsParams{Commands: make([]models.BotCommand, len(cmds))}
	for i, cmd := range cmds {
		params.Commands[i] = models.BotCommand{Command: cmd.Command, Description: cmd.Description}
	}
	_, err := c.b.SetMyCommands(ctx, params)
	return c.toAPIError("setMyCommands", err)
}

// BotCommand is a command + description pair for SetMyCommands.
type BotCommand struct {
	Command     string
	Description string
}

// IsRetriable reports whether an error deserves a backoff+retry (network error,
// 409 conflict, 429 rate limit, or 5xx). User-action errors (400, 403) do not.
func IsRetriable(err error) bool {
	var api *APIError
	if errors.As(err, &api) {
		return api.Code == 409 || api.Code == 429 || api.Code >= 500 || api.Code == 0
	}
	return true // transport/timeout errors are retriable
}

// convertUpdateJSON decodes a raw update (library model) into our domain type.
// It exists for the conversion tests and keeps the models import local.
func convertUpdateJSON(raw string) (Update, error) {
	var u models.Update
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		return Update{}, err
	}
	return convUpdate(&u), nil
}
