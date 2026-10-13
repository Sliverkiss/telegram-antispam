// Package bot holds the antispam decision logic: member-join bio screening,
// in-group message screening, signature-library direct hits, AI fallback
// classification, and the shared punishment flow (delete known messages →
// ban/kick → record → notify). It depends only on interfaces, so the tests
// run against fakes with no network.
package bot

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"adbot/internal/ai"
	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/normalize"
	"adbot/internal/store"
	"adbot/internal/telegram"
)

// TelegramClient is the subset of telegram.Client the bot drives.
type TelegramClient interface {
	GetChat(ctx context.Context, chatID int64) (telegram.ChatInfo, error)
	GetChatMember(ctx context.Context, chatID, userID int64) (telegram.ChatMember, error)
	BanChatMember(ctx context.Context, chatID, userID int64) error
	UnbanChatMember(ctx context.Context, chatID, userID int64) error
	DeleteMessage(ctx context.Context, chatID, messageID int64) error
	SendMessage(ctx context.Context, chatID int64, text string) (int64, error)
}

// Classifier returns an ad verdict for one piece of text; ok=false means
// "undecided" (never act).
type Classifier func(ctx context.Context, content string) (ai.Verdict, bool)

// Bot binds the store, telegram, AI, and config into the decision flow.
type Bot struct {
	Store store.Store
	TG    TelegramClient
	AI    Classifier
	Cfg   config.Config
	Log   *logx.Logger

	// Stats (atomic for /statusz while the poll loop is running).
	pollCount   atomic.Int64
	punishCount atomic.Int64
	aiCalls     atomic.Int64
	aiFails     atomic.Int64
}

// New builds a Bot with defaults filled in.
func New(st store.Store, tg TelegramClient, classify Classifier, cfg config.Config, log *logx.Logger) *Bot {
	if log == nil {
		log = logx.New(nil, logx.Info, "text", "bot")
	}
	return &Bot{Store: st, TG: tg, AI: classify, Cfg: cfg, Log: log}
}

// Stats returns a snapshot for /statusz.
func (b *Bot) Stats() (poll, punished, aiCalls, aiFails int64) {
	return b.pollCount.Load(), b.punishCount.Load(), b.aiCalls.Load(), b.aiFails.Load()
}

// NotePoll bumps the poll counter (called once per getUpdates round).
func (b *Bot) NotePoll() { b.pollCount.Add(1) }

// HandleUpdate dispatches one Telegram update to the right handler.
func (b *Bot) HandleUpdate(ctx context.Context, u telegram.Update) {
	if u.Message != nil && len(u.Message.NewChatMembers) > 0 {
		b.HandleNewMembers(ctx, u.Message.Chat.ID, u.Message.NewChatMembers, u.Message.MessageID)
		return
	}
	if u.ChatMember != nil {
		b.handleChatMember(ctx, u.ChatMember)
		return
	}
	if u.MyChatMember != nil {
		return // the bot's own membership changed — nothing to screen
	}
	if u.Message != nil {
		b.HandleMessage(ctx, *u.Message)
	}
}

func (b *Bot) handleChatMember(ctx context.Context, cm *telegram.ChatMemberUpdate) {
	if cm.Chat.Type == "private" || cm.Chat.Type == "channel" {
		return
	}
	oldStatus := cm.OldChatMember.Status
	newStatus := cm.NewChatMember.Status
	if (oldStatus == "left" || oldStatus == "") && newStatus == "member" {
		b.HandleNewMembers(ctx, cm.Chat.ID, []telegram.User{cm.NewChatMember.User}, 0)
	}
}

// HandleNewMembers screens each freshly joined user: watchlist + bio check.
func (b *Bot) HandleNewMembers(ctx context.Context, chatID int64, members []telegram.User, joinMsgID int64) {
	for _, u := range members {
		if u.IsBot {
			b.Log.Debug("join_skip_bot", "uid", u.ID, "update_id", 0)
			continue
		}
		b.screenNewMember(ctx, chatID, u, joinMsgID)
	}
}

func (b *Bot) screenNewMember(ctx context.Context, chatID int64, u telegram.User, joinMsgID int64) {
	// Bio comes from getChat (Bot API only exposes bio there). Empty/err is
	// non-fatal — we keep the watchlist record and move on.
	bio := ""
	info, err := b.TG.GetChat(ctx, u.ID)
	if err != nil {
		b.Log.Warn("getchat_error", "uid", u.ID, "err", err.Error())
	} else {
		bio = info.Bio
	}
	sp := store.Suspect{
		UID:       u.ID,
		Username:  u.Username,
		FirstName: u.FirstName,
		JoinedAt:  time.Now(),
		ChatID:    chatID,
		Bio:       bio,
	}
	if err := b.Store.AddSuspect(ctx, sp); err != nil {
		b.Log.Error("add_suspect_error", "uid", u.ID, "err", err.Error())
		return
	}
	b.Log.Info("join_recorded", "chat", chatID, "uid", u.ID, "username", u.Username, "has_bio", bio != "")

	// C: signature library check over bio + username + first_name (normalized).
	sigs := []string{}
	for _, s := range []string{bio, u.Username, u.FirstName} {
		if sig := normalize.Signature(s); sig != "" {
			sigs = append(sigs, sig)
		}
	}
	for _, sig := range sigs {
		hit, err := b.Store.HasBioSignature(ctx, sig)
		if err != nil {
			b.Log.Error("sig_lookup_error", "uid", u.ID, "err", err.Error())
			continue
		}
		if hit {
			b.Log.Info("signature_hit", "chat", chatID, "uid", u.ID, "kind", "bio")
			b.punish(ctx, chatID, u.ID, b.Cfg.Action, "signature", "bio", nil)
			if b.Cfg.DeleteJoinMsg && joinMsgID > 0 {
				if err := b.TG.DeleteMessage(ctx, chatID, joinMsgID); err != nil {
					b.Log.Warn("delete_join_msg_error", "err", err.Error())
				}
			}
			return
		}
	}

	// Not hit + non-empty bio → AI.
	if strings.TrimSpace(bio) == "" {
		return
	}
	b.aiCalls.Add(1)
	v, ok := b.AI(ctx, bio)
	if !ok {
		b.aiFails.Add(1)
		b.Log.Warn("ai_undecided_join", "uid", u.ID)
		return
	}
	b.Log.Info("ai_join_verdict", "uid", u.ID, "is_ad", v.IsAd, "category", v.Category, "confidence", v.Confidence)
	if v.IsAd {
		b.punish(ctx, chatID, u.ID, b.Cfg.Action, v.Category, v.Reason, nil)
		b.Store.AddBioSignature(ctx, normalize.Signature(bio), normalize.Normalize(bio))
	}
}

// HandleMessage screens one group message.
func (b *Bot) HandleMessage(ctx context.Context, m telegram.Message) {
	if m.From == nil || m.From.IsBot {
		return
	}
	if strings.TrimSpace(m.Text) == "" {
		return
	}
	chatID := m.Chat.ID
	uid := m.From.ID

	if b.Cfg.IgnoreAdmins && b.isAdmin(ctx, chatID, uid) {
		b.Log.Debug("skip_admin", "chat", chatID, "uid", uid)
		return
	}

	suspect, err := b.Store.IsSuspect(ctx, uid)
	if err != nil {
		b.Log.Error("suspect_lookup_error", "uid", uid, "err", err.Error())
	}

	if suspect {
		if err := b.Store.TrackMessage(ctx, chatID, uid, m.MessageID, b.Cfg.MaxTrackedMsgs); err != nil {
			b.Log.Warn("track_message_error", "uid", uid, "err", err.Error())
		}
	}

	sig := normalize.Signature(m.Text)
	if sig != "" {
		hit, err := b.Store.HasMessageSignature(ctx, sig)
		if err != nil {
			b.Log.Error("msgsig_lookup_error", "uid", uid, "err", err.Error())
		} else if hit {
			b.Log.Info("signature_hit", "chat", chatID, "uid", uid, "kind", "msg")
			b.punishAndClear(ctx, chatID, uid, b.Cfg.Action, "signature", "广告签名命中", true)
			return
		}
	}

	if !suspect {
		return
	}

	// Suspect, not a known signature → AI.
	b.aiCalls.Add(1)
	v, ok := b.AI(ctx, m.Text)
	if !ok {
		b.aiFails.Add(1)
		b.Log.Warn("ai_undecided_message", "uid", uid)
		return
	}
	b.Log.Info("ai_message_verdict", "uid", uid, "is_ad", v.IsAd, "category", v.Category, "confidence", v.Confidence)
	if v.IsAd {
		// delete all known messages → ban/kick → write msg signature → remove
		// from watchlist → record + notify.
		ids, _ := b.Store.TrackedMessages(ctx, chatID, uid)
		b.deleteMessages(ctx, chatID, ids)
		b.ban(ctx, chatID, uid, b.Cfg.Action)
		b.Store.AddMessageSignature(ctx, sig, normalize.Normalize(m.Text))
		b.Store.RemoveSuspect(ctx, uid)
		b.Store.ClearTrackedMessages(ctx, chatID, uid)
		b.recordPunishment(ctx, chatID, uid, b.Cfg.Action, v.Category, v.Reason)
		b.notify(ctx, chatID, uid, v.Category)
	}
}

// isAdmin reports whether uid is a group owner/administrator.
func (b *Bot) isAdmin(ctx context.Context, chatID, uid int64) bool {
	m, err := b.TG.GetChatMember(ctx, chatID, uid)
	if err != nil {
		b.Log.Warn("getchatmember_error", "uid", uid, "err", err.Error())
		return false
	}
	return m.Status == "creator" || m.Status == "administrator"
}

// punish deletes known messages then bans/kicks, records, notifies. It is
// the join signature-hit path (no tracked messages list to drain from store,
// caller passes an explicit id list).
func (b *Bot) punish(ctx context.Context, chatID, uid int64, action, category, reason string, msgIDs []int64) {
	b.deleteMessages(ctx, chatID, msgIDs)
	b.ban(ctx, chatID, uid, action)
	b.Store.RemoveSuspect(ctx, uid)
	b.recordPunishment(ctx, chatID, uid, action, category, reason)
	b.notify(ctx, chatID, uid, category)
}

// punishAndClear is the message signature-hit path which drains the store's
// tracked list before punishing.
func (b *Bot) punishAndClear(ctx context.Context, chatID, uid int64, action, category, reason string, clearTracked bool) {
	ids, _ := b.Store.TrackedMessages(ctx, chatID, uid)
	b.punish(ctx, chatID, uid, action, category, reason, ids)
	if clearTracked {
		b.Store.ClearTrackedMessages(ctx, chatID, uid)
	}
}

func (b *Bot) deleteMessages(ctx context.Context, chatID int64, ids []int64) {
	for _, id := range ids {
		if err := b.TG.DeleteMessage(ctx, chatID, id); err != nil {
			// >48h old messages or non-admin bot fail here — warn, keep going.
			b.Log.Warn("delete_message_error", "chat", chatID, "message_id", id, "err", err.Error())
		}
	}
	b.Log.Info("punish_delete", "chat", chatID, "deleted", len(ids))
}

func (b *Bot) ban(ctx context.Context, chatID, uid int64, action string) {
	if err := b.TG.BanChatMember(ctx, chatID, uid); err != nil {
		b.Log.Error("ban_error", "chat", chatID, "uid", uid, "err", err.Error())
		return
	}
	if action == "kick" {
		if err := b.TG.UnbanChatMember(ctx, chatID, uid); err != nil {
			b.Log.Warn("unban_error", "chat", chatID, "uid", uid, "err", err.Error())
		}
	}
	b.punishCount.Add(1)
	b.Log.Info("punished", "chat", chatID, "uid", uid, "action", action)
}

func (b *Bot) recordPunishment(ctx context.Context, chatID, uid int64, action, category, reason string) {
	p := store.Punishment{UID: uid, ChatID: chatID, Action: action, Category: category, Reason: reason, At: time.Now()}
	if err := b.Store.RecordPunishment(ctx, p); err != nil {
		b.Log.Error("record_punishment_error", "uid", uid, "err", err.Error())
	}
}

func (b *Bot) notify(ctx context.Context, chatID, uid int64, category string) {
	if b.Cfg.LogChat == 0 {
		return
	}
	text := fmt.Sprintf("🚫 广告处置：uid=%d chat=%d category=%s", uid, chatID, category)
	if _, err := b.TG.SendMessage(ctx, b.Cfg.LogChat, text); err != nil {
		b.Log.Warn("notify_error", "err", err.Error())
	}
}
