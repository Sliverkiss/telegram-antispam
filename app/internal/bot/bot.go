// Package bot holds the antispam decision logic: member-join bio screening,
// in-group message screening (text + photo multimodal), signature-library
// direct hits (sha1 exact + simhash near-duplicate), AI chain classification
// with confidence-triggered concurrent vote verification, the fixed-interval
// background retry queue, the local rule engine terminal fallback, group
// whitelisting + auto-leave, the slash commands, and the shared punishment
// flow (delete only the triggering message → ban/kick → record → notify).
//
// Hardening contract (amend3 B): every update is dispatched under a recover
// guard (one panicking message must never kill the process); deleteMessage
// tolerates 429 retry_after (≤3 retries) and 400 message-can't-be-deleted
// (warn, continue); dry-run performs zero writes; non-whitelisted chats get
// zero AI calls and zero writes.
package bot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"adbot/internal/ai"
	"adbot/internal/config"
	"adbot/internal/localrules"
	"adbot/internal/logx"
	"adbot/internal/normalize"
	"adbot/internal/notify"
	"adbot/internal/retryq"
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
	// Image judgment path (amend3 A.3).
	GetFileMetadata(ctx context.Context, fileID string) (telegram.FileMeta, error)
	DownloadFile(ctx context.Context, meta telegram.FileMeta, maxBytes int64) ([]byte, error)
}

// AIClassifier is the pluggable judgment engine (ai.Client in production,
// a fake in tests). ok=false means the whole chain failed → retry queue.
type AIClassifier interface {
	Classify(ctx context.Context, content ai.Content, mode string) (ai.ClassifyResult, bool)
}

// Verifier runs one concurrent vote round (ai.Verify in production).
type Verifier func(ctx context.Context, content ai.Content) ai.VerifyResult

// Bot binds the store, telegram, AI, and config into the decision flow.
type Bot struct {
	Store store.Store
	TG    TelegramClient
	AI    AIClassifier
	// Verify is nil unless confidence-triggered voting is configured.
	Verify Verifier
	Cfg    config.Config
	Log    *logx.Logger

	// RetryQ is the shared pending queue; main wires it after New (the retry
	// closure needs the bot itself).
	RetryQ *retryq.Queue

	// LocalRules is the terminal fallback engine; zero value uses defaults.
	LocalRules localrules.Engine

	// ImageJudge verdict cache + hash dedup (same file_unique_id judged once).
	Images *store.ImageCache

	AIStats *ai.Stats

	// Notifier is the amend4 push subsystem; nil (or disabled) → no pushes.
	// main wires it after New. Every notification goes through it; the old
	// inline notify() is gone.
	Notifier *notify.Notifier

	// adminCache caches deterministic getChatMember results (amend4 §4.5);
	// nil → no caching (direct API call each time).
	adminCache *adminCache

	// judgeClock is stubbed in tests to control timing-sensitive flows.
	judgeClock func() time.Time

	startedAt time.Time

	// Stats (atomic for /statusz while the poll loop is running).
	pollCount    atomic.Int64
	punishCount  atomic.Int64
	deleteCount  atomic.Int64
	deleteFails  atomic.Int64
	aiCalls      atomic.Int64
	aiFails      atomic.Int64
	localRuleCnt atomic.Int64 // local rule terminal decisions
	simhashHits  atomic.Int64
}

// New builds a Bot with defaults filled in.
func New(st store.Store, tg TelegramClient, classify AIClassifier, verify Verifier, cfg config.Config, log *logx.Logger) *Bot {
	if log == nil {
		log = logx.New(nil, logx.Info, "text", "bot")
	}
	if m, ok := st.(*store.Memory); ok {
		m.SetSimhashDistance(cfg.SimhashDistance)
	}
	return &Bot{
		Store:      st,
		TG:         tg,
		AI:         classify,
		Verify:     verify,
		Cfg:        cfg,
		Log:        log,
		LocalRules: localrules.Engine{Threshold: cfg.LocalRuleThreshold, ExtraKeywords: cfg.LocalKeywords},
		Images:     store.NewImageCache(5000),
		AIStats:    ai.NewStats(),
		adminCache: newAdminCache(cfg.AdminCacheTTL, cfg.AdminCacheMax),
		judgeClock: time.Now,
		startedAt:  time.Now(),
	}
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

// ExtraStats returns the amend3 counters for /status and /statusz.
func (b *Bot) ExtraStats() (localFallback, simhashHits int64) {
	return b.localRuleCnt.Load(), b.simhashHits.Load()
}

// NotifySnapshotRaw is kept (see serverstats.go); the public NotifySnapshot
// name is bound to the server view adapter.

// now returns the judge clock reading (real time unless a test froze it).
func (b *Bot) now() time.Time {
	if b.judgeClock == nil {
		return time.Now()
	}
	return b.judgeClock()
}

// NotePoll bumps the poll counter (called once per getUpdates round).
func (b *Bot) NotePoll() { b.pollCount.Add(1) }

// PendingQueueLen returns the retry queue length (0 when unwired).
func (b *Bot) PendingQueueLen() int {
	if b.RetryQ == nil {
		return 0
	}
	return b.RetryQ.Len()
}

// ---------------------------------------------------------------------------
// Update dispatch (panic-isolated)
// ---------------------------------------------------------------------------

// HandleUpdate dispatches one Telegram update under a recover guard: a panic
// in one update logs ERROR + stack and never kills the process.
func (b *Bot) HandleUpdate(ctx context.Context, u telegram.Update) {
	defer b.recoverUpdate(u.UpdateID)
	b.handleUpdate(ctx, u)
}

func (b *Bot) recoverUpdate(updateID int64) {
	if r := recover(); r != nil {
		b.Log.Error("update_panic",
			"update_id", updateID,
			"panic", fmt.Sprint(r),
			"stack", string(debug.Stack()))
	}
}

func (b *Bot) handleUpdate(ctx context.Context, u telegram.Update) {
	// Commands take priority and are exempt from ad screening.
	if u.Message != nil {
		if cmd, args, ok := parseCommand(u.Message.Text); ok {
			b.HandleCommand(ctx, *u.Message, cmd, args)
			return
		}
	}
	if u.EditedMessage != nil {
		// Edited messages re-run screening on the new content but never
		// re-punish an already-punished signature hit twice for the same id:
		// the flow is identical to a fresh message; dedup happens at the
		// punishment layer via the store (suspect removal).
		b.HandleMessage(ctx, *u.EditedMessage)
		return
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
		b.notifyTGWriteError(cm.Chat.ID, 0, 0, "leaveChat", apiErrOrNil(err), "")
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

	// Signature check over bio + username + first_name (normalized), with
	// simhash near-duplicate matching (防改字绕过).
	for _, s := range []string{bio, u.Username, u.FirstName} {
		if strings.TrimSpace(s) == "" {
			continue
		}
		norm := normalize.Normalize(s)
		if norm == "" {
			continue
		}
		if hit := b.searchBioSignature(ctx, norm); hit {
			b.Log.Info("signature_hit", "chat", chatID, "uid", u.ID, "kind", "bio", "near", "simhash")
			b.punish(ctx, punishInfo{
				chatID:    chatID,
				uid:       u.ID,
				username:  u.Username,
				firstName: u.FirstName,
				text:      bio,
			}, b.Cfg.Action, "signature", "bio", notifyDetails{source: "signature", score: 100}, nil)
			if b.Cfg.DeleteJoinMsg && joinMsgID > 0 {
				b.deleteOne(ctx, chatID, joinMsgID)
			}
			return
		}
	}

	// Not hit + non-empty bio → AI (bio mode: the same must-kill list plus the
	// self-promotion clause).
	if strings.TrimSpace(bio) == "" {
		return
	}
	b.judge(ctx, judgmentInput{
		key:       joinKey(u.ID),
		kind:      "join",
		chatID:    chatID,
		messageID: joinMsgID,
		uid:       u.ID,
		username:  u.Username,
		firstName: u.FirstName,
		text:      bio,
		mode:      "bio",
		punish:    true,
		writeBio:  true,
	})
}

// ---------------------------------------------------------------------------
// Message screening (text + photo multimodal)
// ---------------------------------------------------------------------------

// mediaContent extracts the judgment content from a message: text, or
// caption+image for photos. imageFileID is the largest photo (or image
// document) to download; empty when the message has no media.
func (b *Bot) mediaContent(m telegram.Message) (text, imageFileID, fileUniqueID string) {
	text = m.Text
	if text == "" {
		text = m.Caption
	}
	if len(m.Photo) > 0 {
		// Largest size by area (Bot API orders ascending, but be robust).
		best := m.Photo[0]
		for _, p := range m.Photo {
			if p.Width*p.Height > best.Width*best.Height {
				best = p
			}
		}
		return text, best.FileID, best.FileUniqueID
	}
	if m.Document != nil && isImageMime(m.Document.MimeType) {
		return text, m.Document.FileID, m.Document.FileID
	}
	return text, "", ""
}

func isImageMime(m string) bool {
	return strings.HasPrefix(m, "image/")
}

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
	if b.Cfg.IgnoreAdmins && b.isAdmin(ctx, chatID, uid) {
		b.Log.Debug("skip_admin", "chat", chatID, "uid", uid)
		return
	}
	// Admin users (BOT_ADMIN_USERS) are never judged.
	if b.Cfg.IsAdminUser(uid) {
		b.Log.Debug("skip_admin_user", "chat", chatID, "uid", uid)
		return
	}

	text, imageFileID, fileUniqueID := b.mediaContent(m)

	// Judgeability (amend5 D.3): only "judgeable content" counts as the first
	// message — text (incl. caption) that leaves real content after
	// normalization, or an image. Pure emoji / sticker / voice / join-leave
	// system messages carry nothing to judge: no AI call, no watchlist
	// consumption — the user's first-message opportunity stays intact until
	// they post something judgeable.
	norm := normalize.Normalize(text)
	if norm == "" && imageFileID == "" {
		return
	}

	suspect, err := b.Store.IsSuspect(ctx, uid)
	if err != nil {
		b.Log.Error("suspect_lookup_error", "uid", uid, "err", err.Error())
	}

	// Signature check on the text part (exact + simhash) — applies to ALL
	// users, watchlist or not (the only old-user defense left after watchlist
	// removal, at zero AI cost).
	if norm != "" {
		if hit := b.searchMessageSignature(ctx, norm); hit {
			b.Log.Info("signature_hit", "chat", chatID, "uid", uid, "kind", "msg", "near", "simhash")
			b.punishAndClear(ctx, punishInfo{
				chatID:    chatID,
				uid:       uid,
				username:  m.From.Username,
				firstName: m.From.FirstName,
				msgID:     m.MessageID,
				text:      text,
				writeSig:  true,
			}, b.Cfg.Action, "signature", "广告签名命中", notifyDetails{source: "signature", score: 100})
			return
		}
	}

	if !suspect {
		return
	}

	// Suspect, not a known signature → AI (photo multimodal when present).
	b.judge(ctx, judgmentInput{
		key:          msgKey(chatID, m.MessageID),
		kind:         "message",
		chatID:       chatID,
		messageID:    m.MessageID,
		uid:          uid,
		username:     m.From.Username,
		firstName:    m.From.FirstName,
		text:         text,
		imageFileID:  imageFileID,
		fileUniqueID: fileUniqueID,
		mode:         "message",
		punish:       true,
		writeMsg:     true,
	})
}

// ---------------------------------------------------------------------------
// Judgment pipeline: AI chain → (low confidence) vote → retry queue → local rules
// ---------------------------------------------------------------------------

// judgmentInput is everything one judgment needs, shared by the live path and
// the retry worker.
type judgmentInput struct {
	key       string
	kind      string // message | join
	chatID    int64
	messageID int64 // the triggering message to delete
	uid       int64
	// username/firstName feed the 👤 line of the disposition notice.
	username     string
	firstName    string
	text         string
	imageFileID  string // when set, the image is downloaded and judged multimodally
	fileUniqueID string
	mode         string // prompt mode: message | bio
	punish       bool   // run the punishment flow on an ad verdict
	writeMsg     bool   // write the message signature on an ad verdict
	writeBio     bool   // write the bio signature on an ad verdict
	retries      int    // AI retry rounds already consumed (🔁 line)
}

func msgKey(chatID, messageID int64) string {
	return fmt.Sprintf("msg:%d:%d", chatID, messageID)
}

func joinKey(uid int64) string {
	return fmt.Sprintf("join:%d", uid)
}

// judge runs the primary judgment; failures and unresolved votes go to the
// retry queue (the poll loop is never blocked).
func (b *Bot) judge(ctx context.Context, in judgmentInput) {
	start := b.startJudge()
	verdict, model, ok, imageURL := b.runAI(ctx, in)
	if !ok {
		b.aiFails.Add(1)
		b.enqueueRetry(in, "chain_failed")
		return
	}
	b.finishJudgment(ctx, in, verdict, model, imageURL, b.endJudge(start))
}

// runAI performs the AI round (with image download + cache) and returns the
// verdict; ok=false means the chain failed (retry).
func (b *Bot) runAI(ctx context.Context, in judgmentInput) (verdict ai.Verdict, model string, ok bool, imageURL string) {
	content := ai.Content{Text: in.text}
	if in.imageFileID != "" {
		dataURL, v, cached := b.imageFor(ctx, in.imageFileID, in.fileUniqueID)
		if dataURL != "" {
			content.ImageURL = dataURL
			if cached {
				// File-hash dedup: the same image was already judged.
				if v != nil {
					return *v, "image_cache", true, dataURL
				}
			}
		} else {
			// Download/encode failure → same retry queue, never dropped.
			b.Log.Warn("image_download_failed", "file_unique_id", in.fileUniqueID)
			b.enqueueRetry(in, "image_download")
			return ai.Verdict{}, "", false, ""
		}
	}
	b.aiCalls.Add(1)
	res, ok := b.AI.Classify(ctx, content, in.mode)
	if !ok {
		b.aiFails.Add(1)
		return ai.Verdict{}, "", false, content.ImageURL
	}
	b.AIStats.NoteAttempt(res.Model, true)
	for _, at := range res.Attempts {
		if at.Status != "ok" && at.Status != "" {
			b.AIStats.NoteAttempt(at.Model, false)
		}
	}
	if res.Fallback {
		b.AIStats.NoteFallback()
	}
	if in.imageFileID != "" && content.ImageURL != "" {
		// Cache the verdict for this file_unique_id.
		vj, _ := json.Marshal(res.Verdict)
		b.Images.Put(in.fileUniqueID, string(vj), content.ImageURL)
	}
	return res.Verdict, res.Model, true, content.ImageURL
}

// imageFor downloads and encodes one image (≤20MB), or returns the cached
// verdict+data URL for a repeated file_unique_id.
func (b *Bot) imageFor(ctx context.Context, fileID, fileUniqueID string) (du string, cachedVerdict *ai.Verdict, cached bool) {
	if fileUniqueID != "" {
		if vj, du, ok := b.Images.Get(fileUniqueID); ok {
			var v ai.Verdict
			json.Unmarshal([]byte(vj), &v)
			return du, &v, true
		}
	}
	meta, err := b.TG.GetFileMetadata(ctx, fileID)
	if err != nil {
		b.Log.Warn("getfile_error", "err", err.Error())
		return "", nil, false
	}
	data, err := b.TG.DownloadFile(ctx, meta, 20<<20)
	if err != nil {
		b.Log.Warn("download_error", "err", err.Error())
		return "", nil, false
	}
	return imageDataURL(data), nil, false
}

// finishJudgment applies the verdict: ad with enough confidence → punish;
// ad below the threshold → concurrent vote; not ad → done. elapsed is the
// message→disposal total (⏱️ line of the disposition notice).
func (b *Bot) finishJudgment(ctx context.Context, in judgmentInput, verdict ai.Verdict, model string, imageURL string, elapsed time.Duration) {
	b.Log.Info("ai_verdict",
		"kind", in.kind,
		"chat", in.chatID,
		"message_id", in.messageID,
		"uid", in.uid,
		"is_ad", verdict.IsAd,
		"category", verdict.Category,
		"confidence", verdict.Confidence,
		"basis", verdictBasis(verdict),
		"model", model,
		"prompt_version", ai.PromptVersion)

	if !verdict.IsAd {
		// One judgment = one consumption (amend5 D): a completed non-ad verdict
		// removes the user from the watchlist. Vote-unresolved and retrying
		// judgments keep observing (the verdict is not final yet). Join-bio
		// judgments never consume the slot (amend5 D.2: bio is an independent
		// screen; the first-message opportunity stays intact).
		b.consumeWatchlist(ctx, in)
		return
	}

	// Confidence gate: ≥ threshold → punish; below → concurrent vote round.
	if verdict.Confidence >= b.Cfg.MinConfidence || b.Verify == nil {
		if b.Verify == nil && verdict.Confidence < b.Cfg.MinConfidence {
			// No verifier wired (tests): treat low-confidence as needing the
			// vote path but with none available — punish anyway per the
			// "漏杀是严重错误" orientation.
			b.Log.Warn("verify_unavailable", "uid", in.uid, "confidence", verdict.Confidence)
		}
		b.commitAd(ctx, in, verdict, elapsed, in.retries)
		return
	}

	// Low confidence → concurrent flash vote.
	b.aiCalls.Add(1)
	content := ai.Content{Text: in.text, ImageURL: imageURL}
	res := b.Verify(ctx, content)
	b.AIStats.NoteVerify(false)
	b.Log.Info("ai_verify",
		"key", in.key,
		"chat", in.chatID,
		"message_id", in.messageID,
		"votes_total", res.VotesTotal,
		"votes_ad", res.VotesAd,
		"votes_abstain", res.Abstentions,
		"final", res.Final,
		"total_ms", res.TotalMS,
		"per_vote", res.Format())
	switch res.Final {
	case "ad":
		b.AIStats.NoteVerify(false)
		b.commitAd(ctx, in, verdict, elapsed, in.retries)
	case "not_ad":
		b.AIStats.NoteVerify(true) // vote overrode the primary verdict
		// Vote concluded "not ad": the judgment is final, so the one-shot
		// watchlist slot is consumed (amend5 D; join/bio excluded per D.2).
		b.consumeWatchlist(ctx, in)
	default:
		// Short of majority / tie / too many abstentions → not released:
		// re-enqueue for another round after the fixed interval.
		b.aiFails.Add(1)
		b.enqueueRetry(in, "vote_unresolved")
	}
}

// commitAd punishes the ad verdict (delete the triggering message → ban/kick
// → signature write → watchlist removal → record + notify).
func (b *Bot) commitAd(ctx context.Context, in judgmentInput, verdict ai.Verdict, elapsed time.Duration, retries int) {
	category := verdict.ControlledCategory()
	b.punishAndClear(ctx, punishInfo{
		chatID:    in.chatID,
		uid:       in.uid,
		username:  in.username,
		firstName: in.firstName,
		msgID:     in.messageID,
		text:      in.text,
		writeSig:  in.writeMsg || in.writeBio,
		bioKind:   in.kind == "join",
	}, b.Cfg.Action, category, verdict.Reason,
		notifyDetails{source: verdictSource(verdict), score: notify.ConfidenceScore(verdict.Confidence), elapsed: elapsed, retries: retries})
}

// verdictSource maps an ad verdict to the disposition-notice source: the
// upstream sensitive interception is its own basis (amend5 A.4, "· 上游敏感
// 拦截" suffix on the last line); everything else is the ordinary AI path.
func verdictSource(verdict ai.Verdict) string {
	if verdict == ai.SensitiveVerdict {
		return "upstream_sensitive"
	}
	return "ai"
}

// enqueueRetry pushes a judgment into the pending queue (dedup by key).
func (b *Bot) enqueueRetry(in judgmentInput, reason string) {
	if b.RetryQ == nil {
		b.Log.Error("ai_unresolved", "key", in.key, "reason", reason, "summary", "重试队列未接线（测试/自检模式），本次放弃")
		return
	}
	b.RetryQ.Enqueue(retryq.Item{
		Key:       in.key,
		Kind:      in.kind,
		ChatID:    in.chatID,
		MessageID: in.messageID,
		UID:       in.uid,
		Username:  in.username,
		FirstName: in.firstName,
		Text:      in.text,
		Mode:      in.mode,
		Attempts:  0,
		Reason:    reason,
	})
}

// RetryRound is the retryq closure: one full retry round. ok=true means the
// item reached a terminal state (verdict applied or local fallback decided).
func (b *Bot) RetryRound(ctx context.Context, it retryq.Item) (bool, string) {
	start := b.startJudge()
	in := judgmentInput{
		key:       it.Key,
		kind:      it.Kind,
		chatID:    it.ChatID,
		messageID: it.MessageID,
		uid:       it.UID,
		username:  it.Username,
		firstName: it.FirstName,
		text:      it.Text,
		mode:      it.Mode,
		punish:    true,
		writeMsg:  it.Kind == "message",
		writeBio:  it.Kind == "join",
		retries:   it.Attempts,
	}
	verdict, model, ok, _ := b.runAI(ctx, in)
	if ok {
		b.finishJudgment(ctx, in, verdict, model, "", b.endJudge(start))
		return true, ""
	}
	// Attempts exhausted (the queue bumps on return) → local rule fallback
	// decides the terminal state. The queue calls RetryRound once per round;
	// we detect the last round via it.Attempts == max-1.
	if it.Attempts >= b.Cfg.RetryMax-1 {
		b.notifyAIChainFailed(ctx, it, "chain_failed")
		b.localFallback(ctx, in, it.LastErrCode)
		return true, "local_fallback"
	}
	return false, "chain_failed"
}

// notifyAIChainFailed pushes the "AI 连续 N 次失败 → 转本地兜底" event.
func (b *Bot) notifyAIChainFailed(ctx context.Context, it retryq.Item, lastErrCode string) {
	if b.Notifier == nil {
		return
	}
	elapsed := b.endJudge(b.startJudge())
	// Approximation note: the retry span is measured from the moment the last
	// round started; the queue owns the real enqueue→terminal timeline and we
	// deliberately do not reach into it from here.
	span := time.Duration(it.Attempts) * b.retryInterval()
	b.Notifier.Error(notify.ErrorEvent{
		Kind:      "ai_chain",
		Level:     logx.Error,
		Attempts:  it.Attempts,
		Method:    fmt.Sprintf("🔁 已重试 %d 次 · %s · 转本地兜底", it.Attempts, humanDur(span)),
		Detail:    lastErrCode,
		ChatID:    it.ChatID,
		UID:       it.UID,
		MessageID: it.MessageID,
		LastSeen:  lastErrCode,
	})
	_ = ctx
	_ = elapsed
}

// retryInterval is the fixed retry cadence (guarded against a zero config).
func (b *Bot) retryInterval() time.Duration {
	if b.Cfg.RetryInterval <= 0 {
		return 10 * time.Second
	}
	return b.Cfg.RetryInterval
}

// humanDur renders a duration the way the templates use it ("52s", "3 分钟").
func humanDur(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return strconv.Itoa(int(d.Minutes())) + " 分钟"
	case d >= time.Second:
		return strconv.Itoa(int(d.Seconds())) + "s"
	default:
		return strconv.Itoa(int(d.Milliseconds())) + "ms"
	}
}

// localFallback is the terminal, fully automatic decision after the retry
// budget is exhausted: local rules score the normalized text; ≥ threshold →
// punish, below → keep watching (no human in the loop).
func (b *Bot) localFallback(ctx context.Context, in judgmentInput, lastErr string) {
	b.localRuleCnt.Add(1)
	b.AIStats.NoteLocalFallback()
	res := b.LocalRules.Evaluate(normalize.Normalize(in.text), false)
	if b.Cfg.LocalRulesEnabled && res.IsAd {
		b.Log.Error("local_fallback_punish",
			"key", in.key,
			"score", res.Score,
			"threshold", res.Threshold,
			"hits", strings.Join(res.Hits, ","),
			"last_error", lastErr,
			"category", "其他")
		b.punishAndClear(ctx, punishInfo{
			chatID:    in.chatID,
			uid:       in.uid,
			username:  in.username,
			firstName: in.firstName,
			msgID:     in.messageID,
			text:      in.text,
			writeSig:  in.writeMsg || in.writeBio,
			bioKind:   in.kind == "join",
		}, b.Cfg.Action, "其他", "本地规则兜底命中",
			notifyDetails{source: "local", score: res.Score, retries: in.retries})
		return
	}
	// Below threshold: no punishment. The one-shot watchlist semantics (amend5 D)
	// still consume the opportunity: a first message that reached the local
	// fallback has completed its judgment, so the user leaves the watchlist
	// (join/bio judgments excluded per D.2).
	b.Log.Error("local_fallback_pass",
		"key", in.key,
		"score", res.Score,
		"threshold", res.Threshold,
		"hits", strings.Join(res.Hits, ","),
		"last_error", lastErr)
	b.consumeWatchlist(ctx, in)
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
		"本机器人只服务白名单群，检测广告号（简介/发言/图片）并删除触发消息、封禁/踢出。"
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
	localFB, simHits := b.ExtraStats()
	sn := b.AIStats.Snapshot()
	pending := b.PendingQueueLen()

	var modelLines []string
	for _, model := range sn.SortedModels() {
		modelLines = append(modelLines, fmt.Sprintf("%s %d/%d", model, sn.ModelOKs[model], sn.ModelCalls[model]))
	}
	text := fmt.Sprintf("📊 状态\n版本 %s · 运行 %s · 提示词 %s\n后端 %s · TTL %s（滑窗 %v）\n"+
		"模型链 %s\n模型调用 %s（成功/总）· 回退 %d\n"+
		"复核 %d 次（改判 %d）· 删除 %d（失败 %d）· 处置 %d\n"+
		"AI 调用 %d / 失败 %d · 兜底 %d · SimHash 命中 %d · 待决队列 %d\n"+
		"观察名单 %d · 签名库 bio %d / msg %d · 轮询 %d\n模式 %s",
		b.Cfg.Version, b.Uptime().Round(time.Second).String(), ai.PromptVersion,
		b.Cfg.Store, b.Cfg.Retention.String(), b.Cfg.RetentionSliding,
		strings.Join(b.Cfg.AIEffectiveModels(), "→"),
		strings.Join(modelLines, " · "), sn.Fallbacks,
		sn.VerifyRounds, sn.VerifyFlipped, deleted, deleteFails, punished,
		aiCalls, aiFails, localFB, simHits, pending,
		suspects, bioSigs, msgSigs, poll,
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
	res, ok := b.AI.Classify(ctx, ai.Content{Text: args}, "message")
	elapsed := time.Since(start)
	if !ok {
		b.aiFails.Add(1)
		b.replyHelpful(ctx, m, "AI 判定失败（整条模型链不可达或输出违反契约），未触发任何处置")
		return
	}
	var voteNote string
	v := res.Verdict
	finalVerdict := v
	if v.IsAd && v.Confidence < b.Cfg.MinConfidence && b.Verify != nil {
		b.aiCalls.Add(1)
		vres := b.Verify(ctx, ai.Content{Text: args})
		b.AIStats.NoteVerify(false)
		voteNote = "\n复核：" + vres.Format()
		switch vres.Final {
		case "ad":
			finalVerdict = v
		case "not_ad":
			finalVerdict = ai.Verdict{IsAd: false, Category: "无", Confidence: 0, Reason: "复核否决"}
		}
	}
	out, _ := json.Marshal(map[string]any{
		"is_ad":          finalVerdict.IsAd,
		"category":       finalVerdict.ControlledCategory(),
		"confidence":     finalVerdict.Confidence,
		"reason":         finalVerdict.Reason,
		"model":          res.Model,
		"fallback":       res.Fallback,
		"attempts":       fmt.Sprintf("%s", formatChain(res)),
		"elapsed_ms":     elapsed.Milliseconds(),
		"prompt_version": ai.PromptVersion,
	})
	b.reply(ctx, m, string(out)+voteNote)
}

func formatChain(res ai.ClassifyResult) string {
	var parts []string
	for _, a := range res.Attempts {
		parts = append(parts, fmt.Sprintf("%s/%s", a.Model, a.Status))
	}
	return strings.Join(parts, ",")
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

// isAdmin reports whether uid is a group owner/administrator, with a TTL
// cache in front of the API call (amend4 §4.5: only deterministic results are
// cached; API errors bypass the cache so a network blip never freezes a
// member's status).
func (b *Bot) isAdmin(ctx context.Context, chatID, uid int64) bool {
	if status := b.adminCache.get(chatID, uid); status != "" {
		return status == "creator" || status == "administrator"
	}
	m, err := b.adminCache.getChatMember(ctx, b.TG, chatID, uid)
	if err != nil {
		b.Log.Warn("getchatmember_error", "uid", uid, "err", err.Error())
		return false
	}
	return m.Status == "creator" || m.Status == "administrator"
}

// startJudge stamps a judgment's start (the ⏱️ total-processing-time base).
func (b *Bot) startJudge() time.Time { return b.now() }

// endJudge computes the elapsed span since a startJudge stamp.
func (b *Bot) endJudge(start time.Time) time.Duration {
	elapsed := b.now().Sub(start)
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

// consumeWatchlist consumes the one-shot watchlist slot (amend5 D):
// "one judgment = one consumption". A message judgment that reached a
// terminal not-ad/below-threshold outcome removes the user, so later
// messages no longer hit the AI (the signature library still screens
// everyone). Join-bio judgments never consume the slot (amend5 D.2: bio is
// an independent screen; the first-message opportunity stays intact). It is
// a no-op when the user is not on the watchlist (e.g. the /check command or
// a second message after removal).
func (b *Bot) consumeWatchlist(ctx context.Context, in judgmentInput) {
	if in.kind != "message" {
		return
	}
	b.RemoveSuspectIfJoined(ctx, in.uid)
}

// RemoveSuspectIfJoined deletes the uid from the watchlist when present.
func (b *Bot) RemoveSuspectIfJoined(ctx context.Context, uid int64) {
	if ok, _ := b.Store.IsSuspect(ctx, uid); !ok {
		return
	}
	if err := b.Store.RemoveSuspect(ctx, uid); err != nil {
		b.Log.Error("remove_suspect_error", "uid", uid, "err", err.Error())
		return
	}
	b.Log.Debug("watchlist_consumed", "uid", uid, "summary", "一次判定=一次消费：观察名额已释放")
}

// verdictBasis labels the ai_verdict log with the decision source: the
// upstream sensitive interception is the 5th basis after AI/签名/投票/兜底.
func verdictBasis(verdict ai.Verdict) string {
	if verdict == ai.SensitiveVerdict {
		return "upstream_sensitive"
	}
	return "ai"
}

// searchBioSignature: exact sha1 then simhash near-match (bio library).
func (b *Bot) searchBioSignature(ctx context.Context, normalized string) bool {
	sig := normalize.SignatureOf(normalized)
	if sig == "" {
		return false
	}
	if hit, err := b.Store.HasBioSignature(ctx, sig); err == nil && hit {
		return true
	} else if err != nil {
		b.Log.Error("biosig_lookup_error", "err", err.Error())
	}
	return b.simhashNear(ctx, normalized, "bio")
}

// searchMessageSignature: exact sha1 then simhash near-match (msg library).
func (b *Bot) searchMessageSignature(ctx context.Context, normalized string) bool {
	sig := normalize.SignatureOf(normalized)
	if sig == "" {
		return false
	}
	if hit, err := b.Store.HasMessageSignature(ctx, sig); err == nil && hit {
		return true
	} else if err != nil {
		b.Log.Error("msgsig_lookup_error", "err", err.Error())
	}
	return b.simhashNear(ctx, normalized, "msg")
}

// simhashNear does the near-duplicate probe when the store supports it.
func (b *Bot) simhashNear(ctx context.Context, normalized, kind string) bool {
	sr, ok := b.Store.(store.Signaturer)
	if !ok {
		return false
	}
	var sig string
	var hit bool
	if kind == "bio" {
		sig, hit = sr.SearchBioSignature(ctx, normalized, b.Cfg.SimhashDistance)
	} else {
		sig, hit = sr.SearchMessageSignature(ctx, normalized, b.Cfg.SimhashDistance)
	}
	if hit {
		b.simhashHits.Add(1)
		_ = sig
	}
	return hit
}

// punishInfo identifies a punished user for disposal + notification.
type punishInfo struct {
	chatID    int64
	uid       int64
	username  string
	firstName string
	msgID     int64 // triggering message to delete (0 = none, join/bio path)
	text      string
	writeSig  bool
	bioKind   bool
}

// notifyDetails carries the disposition-notice specifics (score source etc.).
type notifyDetails struct {
	source       string // signature | ai | vote | local
	score        int    // 0–100 (local: rule score)
	voteFor      int    // vote lane tally
	voteTotal    int
	elapsed      time.Duration // message → disposal total
	retries      int           // AI retry rounds consumed
	deleteFailed bool          // the triggering message survived
}

// punish deletes the triggering message (if non-zero) then bans/kicks, records,
// notifies. It is the join path (no tracked list; caller passes an explicit id).
func (b *Bot) punish(ctx context.Context, pi punishInfo, action, category, reason string, nd notifyDetails, msgIDs []int64) {
	for _, id := range msgIDs {
		b.deleteOne(ctx, pi.chatID, id)
	}
	b.ban(ctx, pi.chatID, pi.uid, action)
	b.Store.RemoveSuspect(ctx, pi.uid)
	b.recordPunishment(ctx, pi.chatID, pi.uid, action, category, reason)
	b.pushPunishNotice(pi, action, category, nd)
}

// punishAndClear deletes only the triggering message (msgID), bans/kicks, writes
// the message signature (from original text when provided), removes the suspect,
// records and notifies.
func (b *Bot) punishAndClear(ctx context.Context, pi punishInfo, action, category, reason string, nd notifyDetails) {
	if pi.msgID > 0 {
		b.deleteOne(ctx, pi.chatID, pi.msgID)
	}
	b.ban(ctx, pi.chatID, pi.uid, action)
	if b.Cfg.DryRun {
		// Dry-run: zero writes — no signature, no watchlist removal, no record,
		// no notification.
		return
	}
	if pi.writeSig && strings.TrimSpace(pi.text) != "" {
		sig, norm := normalize.Signature(pi.text), normalize.Normalize(pi.text)
		if pi.bioKind {
			if err := b.Store.AddBioSignature(ctx, sig, norm); err != nil {
				b.Log.Error("add_biosig_error", "uid", pi.uid, "err", err.Error())
			}
		} else if err := b.Store.AddMessageSignature(ctx, sig, norm); err != nil {
			b.Log.Error("add_msgsig_error", "uid", pi.uid, "err", err.Error())
		}
	}
	b.Store.RemoveSuspect(ctx, pi.uid)
	b.recordPunishment(ctx, pi.chatID, pi.uid, action, category, reason)
	b.pushPunishNotice(pi, action, category, nd)
}

// pushPunishNotice renders and submits the §1.1 disposition notification.
func (b *Bot) pushPunishNotice(pi punishInfo, action, category string, nd notifyDetails) {
	if b.Notifier == nil {
		return
	}
	b.Notifier.Punish(notify.PunishDetails{
		ChatID:       pi.chatID,
		UID:          pi.uid,
		Username:     pi.username,
		FirstName:    pi.firstName,
		Text:         pi.text,
		Action:       action,
		Category:     category,
		Source:       nd.source,
		Score:        nd.score,
		VoteTotal:    nd.voteTotal,
		VoteFor:      nd.voteFor,
		Retries:      nd.retries,
		DeleteFailed: nd.deleteFailed,
		Elapsed:      nd.elapsed,
	})
}

// deleteOne deletes one message with 429 retry_after tolerance (≤3 retries)
// and 400 "message can't be deleted" as a warn-only outcome. Delete failures
// never interrupt the ban/signature flow.
func (b *Bot) deleteOne(ctx context.Context, chatID, msgID int64) {
	if b.Cfg.DryRun {
		b.Log.Info("dry_run_skip_delete", "chat", chatID, "message_id", msgID)
		return
	}
	for retry := 0; retry < 3; retry++ {
		err := b.TG.DeleteMessage(ctx, chatID, msgID)
		if err == nil {
			b.deleteCount.Add(1)
			return
		}
		var apiErr *telegram.APIError
		if errors.As(err, &apiErr) {
			switch {
			case apiErr.Code == 429 && apiErr.RetryAfter > 0:
				wait := time.Duration(apiErr.RetryAfter) * time.Second
				if wait > 10*time.Second {
					wait = 10 * time.Second
				}
				b.Log.Warn("delete_rate_limited", "chat", chatID, "message_id", msgID, "retry_after_s", apiErr.RetryAfter, "retry", retry+1)
				select {
				case <-ctx.Done():
					b.deleteFails.Add(1)
					return
				case <-time.After(wait):
				}
				continue
			case apiErr.Code == 400 && strings.Contains(strings.ToLower(apiErr.Description), "delete"):
				// "message can't be deleted": already gone / >48h — noise.
				b.Log.Warn("delete_message_not_deletable", "chat", chatID, "message_id", msgID, "err", apiErr.Description)
				b.deleteFails.Add(1)
				b.notifyTGWriteError(chatID, msgID, 0, "deleteMessage", apiErr, "超 48 小时")
				return
			case apiErr.Code == 403:
				b.Log.Error("delete_forbidden", "chat", chatID, "message_id", msgID, "err", apiErr.Description)
				b.deleteFails.Add(1)
				b.notifyTGWriteError(chatID, msgID, 0, "deleteMessage", apiErr, "")
				return
			}
		}
		b.deleteFails.Add(1)
		b.Log.Warn("delete_message_error", "chat", chatID, "message_id", msgID, "err", err.Error())
		b.notifyTGWriteError(chatID, msgID, 0, "deleteMessage", apiErrOrNil(err), "")
		return
	}
	b.deleteFails.Add(1)
	b.Log.Warn("delete_message_exhausted", "chat", chatID, "message_id", msgID)
}

// apiErrOrNil extracts *telegram.APIError when err carries one.
func apiErrOrNil(err error) *telegram.APIError {
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return nil
}

// notifyTGWriteError pushes the 📵 Telegram write failure event (4xx/5xx from
// deleteMessage / banChatMember / leaveChat / unbanChatMember). APIError-nil
// (transport error) still notifies with the raw error text.
func (b *Bot) notifyTGWriteError(chatID, messageID, uid int64, method string, apiErr *telegram.APIError, note string) {
	if b.Notifier == nil {
		return
	}
	detail := note
	code := 0
	if apiErr != nil {
		code = apiErr.Code
		if detail == "" {
			detail = apiErr.Description
		}
	}
	b.Notifier.Error(notify.ErrorEvent{
		Kind:      "telegram_write",
		Level:     logx.Error,
		ChatID:    chatID,
		UID:       uid,
		MessageID: messageID,
		Method:    method,
		Detail:    strconv.Itoa(code) + " " + detail,
		LastSeen:  method + " " + detail,
	})
}

func (b *Bot) ban(ctx context.Context, chatID, uid int64, action string) {
	if b.Cfg.DryRun {
		b.Log.Info("dry_run_skip_dispose", "chat", chatID, "uid", uid, "action", action)
		return
	}
	if err := b.TG.BanChatMember(ctx, chatID, uid); err != nil {
		b.Log.Error("ban_error", "chat", chatID, "uid", uid, "err", err.Error())
		b.notifyTGWriteError(chatID, 0, uid, "banChatMember", apiErrOrNil(err), "")
		return
	}
	if action == "kick" {
		if err := b.TG.UnbanChatMember(ctx, chatID, uid); err != nil {
			b.Log.Warn("unban_error", "chat", chatID, "uid", uid, "err", err.Error())
			b.notifyTGWriteError(chatID, 0, uid, "unbanChatMember", apiErrOrNil(err), "")
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

// imageDataURL base64-encodes downloaded bytes with a magic-byte mime sniff.
func imageDataURL(data []byte) string {
	mime := "image/jpeg"
	if len(data) >= 8 && data[0] == 0x89 && data[1] == 'P' {
		mime = "image/png"
	} else if len(data) >= 6 && data[0] == 'G' && data[1] == 'I' {
		mime = "image/gif"
	} else if len(data) > 12 && data[8] == 'W' && data[9] == 'E' {
		mime = "image/webp"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}
