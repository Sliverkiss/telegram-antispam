package telegram

// Hermes 独立验收（amend12 解析层）：用【真实 Bot API JSON 原文】验证触发字段，
// 不复用执行体的表驱动测试（他们直接构造库结构体，容易漏掉 json tag 层面的问题）。

import "testing"

// amend12 的真实形状：群里回复关联频道帖子的自动转发。
const h12RealCommentJSON = `{
  "update_id": 900001,
  "message": {
    "message_id": 101,
    "from": {"id": 9301, "is_bot": false, "first_name": "阿三", "username": "spam3", "language_code": "zh-hans"},
    "chat": {"id": -1003888828164, "title": "鸡有鸡窝", "type": "supergroup", "username": "checkinHome"},
    "date": 1770000000,
    "text": "沙发 看看我的主页",
    "reply_to_message": {
      "message_id": 100,
      "sender_chat": {"id": -1001324890214, "title": "Sliverkissの废弃文化研究所", "type": "channel", "username": "sliverkiss_blog"},
      "chat": {"id": -1003888828164, "title": "鸡有鸡窝", "type": "supergroup", "username": "checkinHome"},
      "date": 1769999000,
      "is_automatic_forward": true,
      "text": "频道帖子正文"
    }
  }
}`

// 对照：回复群里普通成员的消息（不该触发）。
const h12PlainReplyJSON = `{
  "update_id": 900002,
  "message": {
    "message_id": 103,
    "from": {"id": 9302, "is_bot": false, "first_name": "小李"},
    "chat": {"id": -1003888828164, "title": "鸡有鸡窝", "type": "supergroup"},
    "date": 1770000100,
    "text": "回复你一下",
    "reply_to_message": {
      "message_id": 102,
      "from": {"id": 9303, "is_bot": false, "first_name": "老王"},
      "chat": {"id": -1003888828164, "title": "鸡有鸡窝", "type": "supergroup"},
      "date": 1770000050,
      "text": "普通消息"
    }
  }
}`

// 对照：频道帖子在群里的自动转发本身（bot 必须不被它触发，也不该当成人的消息）。
const h12AutoForwardJSON = `{
  "update_id": 900003,
  "message": {
    "message_id": 100,
    "sender_chat": {"id": -1001324890214, "title": "Sliverkissの废弃文化研究所", "type": "channel", "username": "sliverkiss_blog"},
    "chat": {"id": -1003888828164, "title": "鸡有鸡窝", "type": "supergroup"},
    "date": 1769999000,
    "is_automatic_forward": true,
    "text": "频道帖子正文"
  }
}`

func TestH12ParseRealChannelPostComment(t *testing.T) {
	u, err := convertUpdateJSON(h12RealCommentJSON)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if u.Message == nil {
		t.Fatal("message 丢了")
	}
	m := *u.Message
	if m.Chat.ID != -1003888828164 {
		t.Fatalf("群 id 解析错：%d", m.Chat.ID)
	}
	if m.From == nil || m.From.ID != 9301 {
		t.Fatalf("from 解析错：%+v", m.From)
	}
	if m.ReplyToMessage == nil {
		t.Fatal("reply_to_message 丢了 → 触发器永远为假")
	}
	if !m.ReplyToMessage.IsAutomaticForward {
		t.Fatal("is_automatic_forward 没映射出来 → 触发器永远为假")
	}
	if m.ReplyToMessage.SenderChat == nil {
		t.Fatal("sender_chat 没映射出来（频道来源就认不出）")
	}
	if m.ReplyToMessage.SenderChat.Type != "channel" || m.ReplyToMessage.SenderChat.ID != -1001324890214 {
		t.Fatalf("sender_chat 解析错：%+v", *m.ReplyToMessage.SenderChat)
	}
	if m.Text == "" {
		t.Fatal("正文丢了")
	}
}

func TestH12ParsePlainReplyDoesNotTrigger(t *testing.T) {
	u, err := convertUpdateJSON(h12PlainReplyJSON)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	m := *u.Message
	if m.ReplyToMessage == nil {
		t.Fatal("reply_to_message 丢了")
	}
	if m.ReplyToMessage.IsAutomaticForward {
		t.Fatal("普通回复被误判成频道帖子评论 → 会误触发审简介")
	}
	if m.ReplyToMessage.SenderChat != nil {
		t.Fatalf("普通回复不该有 sender_chat：%+v", *m.ReplyToMessage.SenderChat)
	}
}

func TestH12ParseAutoForwardItself(t *testing.T) {
	u, err := convertUpdateJSON(h12AutoForwardJSON)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	m := *u.Message
	if !m.IsAutomaticForward {
		t.Fatal("自动转发自身的标记没映射出来")
	}
	if m.From != nil {
		t.Fatalf("自动转发没有真人 From，应为 nil：%+v", *m.From)
	}
	if m.SenderChat == nil || m.SenderChat.Type != "channel" {
		t.Fatal("自动转发必须以频道身份暴露（否则会被当成真人消息处理）")
	}
	// 它自身不该被当成"评论"（它的 ReplyToMessage 为空）。
	if m.ReplyToMessage != nil {
		t.Fatal("自动转发自身不该带 reply_to_message")
	}
}
