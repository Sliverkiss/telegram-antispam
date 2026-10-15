package ai

// Hermes 独立验收（amend15）：@ 解析出的“相关简介”证据必须以数据身份落在 <msg> 隔离内，
// 且运行时证据不得混进 system 提示词（防提示词注入面）。
// 断言：① user 消息 = <msg>\n…\n</msg>，证据整块在其中；
//       ② system 提示词 === BuildSystemPrompt("bio")，且不含本次运行时的 handle/简介文本。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestH15MyRelatedBlockStaysInsideMsgDelimiters(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured)
		w.Write([]byte(completionBody(verdictJSON(true, "诈骗", 0.95, "x"))))
	}))
	defer srv.Close()

	text := "简介 @gray_channel\n【相关简介】@gray_channel：棋牌资源每日更新 上分下分通道"
	c := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: "k", Models: []string{"m1"}})
	if _, ok := c.ClassifyText(context.Background(), text, "bio"); !ok {
		t.Fatal("classify 失败，无法验证请求体")
	}

	msgs, _ := captured["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("请求体缺少 messages：%v", captured)
	}
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	user, _ := msgs[1].(map[string]any)["content"].(string)

	i := strings.Index(user, "<msg>")
	j := strings.LastIndex(user, "</msg>")
	if i < 0 || j < i {
		t.Fatalf("user 内容必须含 <msg>…</msg>：%q", user)
	}
	if strings.TrimSpace(user[j+len("</msg>"):]) != "" {
		t.Fatalf("</msg> 之后不得再有内容（证据不许落在隔离区外）：%q", user)
	}
	inner := strings.Trim(user[i+len("<msg>"):j], "\n")
	if inner != text {
		t.Fatalf("证据必须整块落在 <msg> 内：\n got  %q\n want %q", inner, text)
	}
	if strings.Contains(sys, "gray_channel") || strings.Contains(sys, "棋牌资源") {
		t.Fatalf("运行时证据不得进 system 提示词")
	}
	if sys != BuildSystemPrompt("bio") {
		t.Fatalf("system 提示词必须等于 BuildSystemPrompt(bio)")
	}
}
