// Package bot holds the antispam decision logic: member-join bio screening,
// in-group message screening, signature-library direct hits, AI fallback
// classification, group whitelisting + auto-leave, the four slash commands,
// and the shared punishment flow (delete the triggering message → ban/kick →
// record → notify). It depends only on interfaces (incl. the telegram
// TelegramClient thin interface), so tests run against fakes with no network.
package bot

import (
	"context"
	"encoding/json"
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
	LeaveChat(ctx context.Context, chatID int64) error
}

// Classifier returns an ad verdict for one piece of content; ok=false means
// "undecided" (never act).
type Classifier func(ctx context.Context, content string) (ai.Verdict, bool)

// UpdCounter lets poller report poll rounds. The library drives polling, but
// keep the counter for /status.
type UpdCounter interface {
	NotePoll()
}

// Bot binds the store, telegram, AI, and config into the decision flow.
type Bot struct {
	Store store.Store
	TG    TelegramClient
	AI    Classifier
	Cfg   config.Config
	Log   *logx.Logger

	startedAt time.Time

	// Stats (atomic for /statusz while the poll loop is running).
	pollCount   atomic.Int64
	punishCount atomic.Int64
	deleteCount atomic.Int64
	deleteFails atomic.Int64
	aiCalls     atomic.Int64
	aiFails     atomic.Int64
}

// New builds a Bot with defaults filled in.
func New(st store.Store, tg TelegramClient, classify Classifier, cfg config.Config, log *logx.Logger) *Bot {
	if log == nil {
		log = logx.New(nil, logx.Info, "text", "bot")
	}
	return &Bot{Store: st, TG: tg, AI: classify, Cfg: cfg, Log: log, startedAt: time.Now()}
}

// Uptime returns the time since the bot started.
func (b *Bot) Uptime() time.Duration { return time.Since(b.startedAt) }

// Stats returns a snapshot for /statusz.
func (b *Bot) Stats() (poll, punished, aiCalls, aiFails int64) {
	return b.pollCount.Load(), b.punishCount.Load(), b.aiCalls.Load(), b.aiFails.Load()
}

// DeleteStats returns delete counters for /statusz.
func (b *Bot) DeleteStats() (deleted, failed int64) {
	return b.deleteCount.Load(), b.deleteFails.Load()
}

// NotePoll bumps the poll counter (called once per getUpdates round).
func (b *Bot) NotePoll() { b.pollCount.Add(1) }

// ---------------------------------------------------------------------------
// Update dispatch
// ---------------------------------------------------------------------------

// HandleUpdate dispatches one Telegram update to the right handler.
func (b *Bot) HandleUpdate(ctx context.Context, u telegram.Update) {
	// Command messages take priority and are exempt from ad screening.
	if u.Message != nil {
		if cmd, args, ok := parseCommand(u.Message.Text); ok {
			b.HandleCommand(ctx, *u.Message, cmd, args)
			return
		}
	}

	if u.Message != nil && len(u.Message.NewChatMembers) > 0 {
		b.HandleNewMembers(ctx, u.Message.Chat.ID, u.Message.NewChatMembers, u.Message.MessageID)
		return
	}
	if u.ChatMember != nil {
		b.handleChatMember(ctx, u.ChatMember)
		return
	}
	if u.MyChatMember != nil {
		b.handleMyChatMember(ctx, u.MyChatMember)
		return
	}
	if u.Message != nil {
		b.HandleMessage(ctx, *u.Message)
	}
}

func (b *Bot) handleChatMember(ctx context.Context, cm *telegram.ChatMemberUpdate) {
	if cm.Chat.Type == "private" || cm.Chat.Type == "channel" {
		return
	}
	// Only screen joins into authorized chats.
	if !b.Cfg.AuthorizedChat(cm.Chat.ID) {
		b.Log.Debug("ignored_chat", "chat", cm.Chat.ID, "type", cm.Chat.Type)
		return
	}
	oldStatus := cm.OldChatMember.Status
	newStatus := cm.NewChatMember.Status
	if (oldStatus == "left" || oldStatus == "") && newStatus == "member" {
		b.HandleNewMembers(ctx, cm.Chat.ID, []telegram.User{cm.NewChatMember.User}, 0)
	}
}

// handleMyChatMember handles the bot's own membership changes: promotions/admits
// into non-whitelisted groups trigger an immediate leave (with optional notice).
func (b *Bot) handleMyChatMember(ctx context.Context, cm *telegram.ChatMemberUpdate) {
	if cm.Chat.Type == "private" || cm.Chat.Type == "channel" {
		return
	}
	if b.Cfg.AuthorizedChat(cm.Chat.ID) {
		return // whitelisted — stay.
	}
	newStatus := cm.NewChatMember.Status
	oldStatus := cm.OldChatMember.Status
	// Only react when the bot was added/promoted (left→member/administrator).
	becameMember := (oldStatus == "left" || oldStatus == "") &&
		(newStatus == "member" || newStatus == "administrator")
	if !becameMember {
		return
	}
	if !b.Cfg.LeaveOtherChats {
		return
	}
	b.Log.Warn("leave_foreign_chat",
		"chat", cm.Chat.ID,
		"title", cm.Chat.Title,
		"from_user", cm.From.ID,
		"status", newStatus)
	if b.Cfg.LeaveNotice {
		if _, err := b.TG.SendMessage(ctx, cm.Chat.ID, "本机器人只服务于指定群组，即将退出"); err != nil {
			b.Log.Warn("leave_notice_error", "chat", cm.Chat.ID, "err", err.Error())
		}
	}
	if err := b.TG.LeaveChat(ctx, cm.Chat.ID); err != nil {
		b.Log.Warn("leave_chat_error", "chat", cm.Chat.ID, "err", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Join screening
// ---------------------------------------------------------------------------

// HandleNewMembers screens each freshly joined user: watchlist + bio check.
func (b *Bot) HandleNewMembers(ctx context.Context, chatID int64, members []telegram.User, joinMsgID int64) {
	if !b.Cfg.AuthorizedChat(chatID) {
		b.Log.Debug("ignored_chat", "chat", chatID)
		return
	}
	for _, u := range members {
		if u.IsBot {
			b.Log.Debug("join_skip_bot", "uid", u.ID)
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

// ---------------------------------------------------------------------------
// Message screening
// ---------------------------------------------------------------------------

// HandleMessage screens one group message.
func (b *Bot) HandleMessage(ctx context.Context, m telegram.Message) {
	if m.From == nil || m.From.IsBot {
		return
	}
	// DMs: commands are handled earlier; other DM text does not screen.
	if m.Chat.Type == "private" {
		return
	}
	chatID := m.Chat.ID
	uid := m.From.ID

	// Non-whitelisted group → ignore entirely (zero AI calls, zero writes).
	if !b.Cfg.AuthorizedChat(chatID) {
		b.Log.Debug("ignored_chat", "chat", chatID, "type", m.Chat.Type)
		return
	}
	if strings.TrimSpace(m.Text) == "" {
		return
	}

	if b.Cfg.IgnoreAdmins && b.isAdmin(ctx, chatID, uid) {
		b.Log.Debug("skip_admin", "chat", chatID, "uid", uid)
		return
	}
	// Admin users (BOT_ADMIN_USERS) are never judged.
	if b.Cfg.IsAdminUser(uid) {
		b.Log.Debug("skip_admin_user", "chat", chatID, "uid", uid)
		return
	}

	suspect, err := b.Store.IsSuspect(ctx, uid)
	if err != nil {
		b.Log.Error("suspect_lookup_error", "uid", uid, "err", err.Error())
	}

	sig := normalize.Signature(m.Text)
	if sig != "" {
		hit, err := b.Store.HasMessageSignature(ctx, sig)
		if err != nil {
			b.Log.Error("msgsig_lookup_error", "uid", uid, "err", err.Error())
		} else if hit {
			b.Log.Info("signature_hit", "chat", chatID, "uid", uid, "kind", "msg")
			b.punishAndClear(ctx, chatID, uid, m.MessageID, b.Cfg.Action, "signature", "广告签名命中", "", false)
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
		// Delete only the triggering message → ban/kick → write msg signature →
		// remove from watchlist → record + notify.
		b.punishAndClear(ctx, chatID, uid, m.MessageID, b.Cfg.Action, v.Category, v.Reason, m.Text, true)
	}
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// parseCommand extracts a leading /command and its raw argument.
func parseCommand(text string) (cmd string, args string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return "", "", false
	}
	// Strip a mention suffix "/status@BotName".
	fields := strings.Fields(t)
	head := fields[0]
	if i := strings.IndexByte(head, '@'); i > 0 {
		head = head[:i]
	}
	head = strings.ToLower(head)
	args = strings.TrimSpace(strings.TrimPrefix(t, fields[0]))
	return head, args, true
}

// HandleCommand routes a slash command.
func (b *Bot) HandleCommand(ctx context.Context, m telegram.Message, cmd, args string) {
	uid := m.From.ID
	// /start and /help are available to everyone, in any chat.
	switch cmd {
	case "/start":
		b.cmdStart(ctx, m)
		return
	case "/help":
		b.cmdHelp(ctx, m)
		return
	}
	// /status and /check are admin-only (BOT_ADMIN_USERS or a group owner/admin).
	if !b.Cfg.IsAdminUser(uid) && !b.isAdmin(ctx, m.Chat.ID, uid) {
		b.replyHelpful(ctx, m, "此命令仅管理员可用")
		return
	}
	switch cmd {
	case "/status":
		b.cmdStatus(ctx, m)
	case "/check":
		b.cmdCheck(ctx, m, args)
	default:
		b.Log.Debug("unknown_command", "chat", m.Chat.ID, "cmd", cmd)
	}
}

func (b *Bot) cmdStart(ctx context.Context, m telegram.Message) {
	protected := "否"
	if m.Chat.Type == "private" {
		protected = "私聊（不受群白名单限制）"
	} else if b.Cfg.AuthorizedChat(m.Chat.ID) {
		protected = "是（在保护范围内）"
	} else {
		protected = "否（非白名单群，不检测）"
	}
	mode := "正常"
	if b.Cfg.DryRun {
		mode = "干跑（只判定不处置）"
	}
	text := fmt.Sprintf("🟢 在线\n版本：%s\n运行时长：%s\n存储后端：%s\n当前模式：%s\n本聊天受保护：%s",
		b.Cfg.Version, b.Uptime().Round(time.Second).String(), b.Cfg.Store, mode, protected)
	b.reply(ctx, m, text)
}

func (b *Bot) cmdHelp(ctx context.Context, m telegram.Message) {
	text := "/start 查看状态 · /help 帮助 · /status 管理状态 · /check <文本> AI 判定测试\n" +
		"本机器人只服务白名单群，检测广告号（简介/发言）并删除触发消息、封禁/踢出。"
	b.reply(ctx, m, text)
}

func (b *Bot) cmdStatus(ctx context.Context, m telegram.Message) {
	ctxb := context.Background()
	suspects, _ := b.Store.SuspectCount(ctxb)
	bioSigs, _ := b.Store.BioSigCount(ctxb)
	msgSigs, _ := b.Store.MsgSigCount(ctxb)
	punished, _ := b.Store.Punished(ctxb)
	poll, _, aiCalls, aiFails := b.Stats()
	deleted, deleteFails := b.DeleteStats()

	text := fmt.Sprintf("📊 状态\n版本 %s · 运行 %s\n后端 %s · TTL %s（滑窗 %v）\n"+
		"轮询 %d · 处置 %d · 删除 %d（失败 %d）\nAI 调用 %d / 失败 %d\n"+
		"观察名单 %d · 签名库 bio %d / msg %d\n模式 %s",
		b.Cfg.Version, b.Uptime().Round(time.Second).String(),
		b.Cfg.Store, b.Cfg.Retention.String(), b.Cfg.RetentionSliding,
		poll, punished, deleted, deleteFails,
		aiCalls, aiFails,
		suspects, bioSigs, msgSigs,
		map[bool]string{true: "干跑", false: "正常"}[b.Cfg.DryRun])
	b.reply(ctx, m, text)
}

func (b *Bot) cmdCheck(ctx context.Context, m telegram.Message, args string) {
	if strings.TrimSpace(args) == "" {
		b.replyHelpful(ctx, m, "用法：/check <文本>")
		return
	}
	b.aiCalls.Add(1)
	start := time.Now()
	v, ok := b.AI(ctx, args)
	elapsed := time.Since(start)
	if !ok {
		b.aiFails.Add(1)
		b.replyHelpful(ctx, m, "AI 判定失败（不可达或无法解析），未触发任何处置")
		return
	}
	out, _ := json.Marshal(map[string]any{
		"is_ad":      v.IsAd,
		"category":   v.Category,
		"confidence": v.Confidence,
		"reason":     v.Reason,
		"elapsed_ms": elapsed.Milliseconds(),
	})
	b.reply(ctx, m, string(out))
}

func (b *Bot) reply(ctx context.Context, m telegram.Message, text string) {
	msgID, err := b.TG.SendMessage(ctx, m.Chat.ID, text)
	if err != nil {
		b.Log.Error("cmd_reply_error", "chat", m.Chat.ID, "err", err.Error())
		return
	}
	if b.Cfg.CmdAutodeleteSec > 0 {
		go func() {
			time.Sleep(time.Duration(b.Cfg.CmdAutodeleteSec) * time.Second)
			ctxd, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := b.TG.DeleteMessage(ctxd, m.Chat.ID, msgID); err != nil {
				b.Log.Debug("cmd_autodelete_error", "err", err.Error())
			}
		}()
	}
}

func (b *Bot) replyHelpful(ctx context.Context, m telegram.Message, text string) {
	b.reply(ctx, m, text)
}

// ---------------------------------------------------------------------------
// Decision helpers
// ---------------------------------------------------------------------------

// isAdmin reports whether uid is a group owner/administrator.
func (b *Bot) isAdmin(ctx context.Context, chatID, uid int64) bool {
	m, err := b.TG.GetChatMember(ctx, chatID, uid)
	if err != nil {
		b.Log.Warn("getchatmember_error", "uid", uid, "err", err.Error())
		return false
	}
	return m.Status == "creator" || m.Status == "administrator"
}

// punish deletes the triggering message (if non-zero) then bans/kicks, records,
// notifies. It is the join path (no tracked list; caller passes an explicit id).
func (b *Bot) punish(ctx context.Context, chatID, uid int64, action, category, reason string, msgIDs []int64) {
	for _, id := range msgIDs {
		b.deleteOne(ctx, chatID, id)
	}
	b.ban(ctx, chatID, uid, action)
	b.Store.RemoveSuspect(ctx, uid)
	b.recordPunishment(ctx, chatID, uid, action, category, reason)
	b.notify(ctx, chatID, uid, category)
}

// punishAndClear deletes only the triggering message (msgID), bans/kicks, writes
// the message signature (from original text when provided), removes the suspect,
// records and notifies.
func (b *Bot) punishAndClear(ctx context.Context, chatID, uid, msgID int64, action, category, reason, original string, writeSig bool) {
	if msgID > 0 {
		b.deleteOne(ctx, chatID, msgID)
	}
	b.ban(ctx, chatID, uid, action)
	if writeSig && original != "" {
		b.Store.AddMessageSignature(ctx, normalize.Signature(original), normalize.Normalize(original))
	}
	b.Store.RemoveSuspect(ctx, uid)
	b.recordPunishment(ctx, chatID, uid, action, category, reason)
	b.notify(ctx, chatID, uid, category)
}

func (b *Bot) deleteOne(ctx context.Context, chatID, msgID int64) {
	if b.Cfg.DryRun {
		b.Log.Info("dry_run_skip_delete", "chat", chatID, "message_id", msgID)
		return
	}
	if err := b.TG.DeleteMessage(ctx, chatID, msgID); err != nil {
		// >48h old messages or non-admin bots fail here — warn, keep going.
		b.deleteFails.Add(1)
		b.Log.Warn("delete_message_error", "chat", chatID, "message_id", msgID, "err", err.Error())
		return
	}
	b.deleteCount.Add(1)
}

func (b *Bot) ban(ctx context.Context, chatID, uid int64, action string) {
	if b.Cfg.DryRun {
		b.Log.Info("dry_run_skip_dispose", "chat", chatID, "uid", uid, "action", action)
		return
	}
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
