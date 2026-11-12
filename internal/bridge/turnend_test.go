package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 拼对话记录的辅助函数
func tUser(content any, extra map[string]any) string {
	m := map[string]any{"type": "user", "uuid": "u", "message": map[string]any{"role": "user", "content": content}}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func tAssistant(uuid, stop string, blocks ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": uuid,
		"message": map[string]any{"role": "assistant", "content": blocks, "stop_reason": stop}})
	return string(b)
}

func tText(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func feishuNote(chat, msg, text string) string {
	ev, _ := json.Marshal(map[string]string{"chat_id": chat, "message_id": msg, "text": text})
	return "<task-notification>\n<task-id>x</task-id>\n<summary>Monitor event</summary>\n<event>飞书消息 " + string(ev) + "</event>\n</task-notification>"
}

func writeTranscript(t *testing.T, lines ...string) string {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadLastTurnFeishu(t *testing.T) {
	p := writeTranscript(t,
		tUser("电脑上打的字", nil),
		tAssistant("a0", "end_turn", tText("旧回答\nr📨: 旧的")),
		tUser(feishuNote("oc_1", "om_1", "现在咋样了"), nil),
		tUser("<system-reminder>x</system-reminder>", map[string]any{"isMeta": true}),
		tAssistant("a1", "tool_use", tText("📨 现在咋样了\n\n我先看一下日志。"), map[string]any{"type": "tool_use", "name": "Bash"}),
		tUser([]map[string]any{{"type": "tool_result", "content": "ok"}}, nil),
		tAssistant("a2", "end_turn", tText("查完了。\n\nr📨: 都跑通了，**406 项**测试通过。\n还差代理没配。")),
	)
	r, err := readLastTurn(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.chatID != "oc_1" || r.replyTo != "om_1" || !r.complete || r.lastID != "a2" {
		t.Fatalf("turn = %+v", r)
	}
	if want := "都跑通了，**406 项**测试通过。\n还差代理没配。"; r.text != want {
		t.Errorf("text = %q, want %q", r.text, want)
	}
}

// 后台任务通知开始的回合：最近一次说话来自飞书就发，但不引用
func TestReadLastTurnBackgroundAfterFeishu(t *testing.T) {
	p := writeTranscript(t,
		tUser(feishuNote("oc_1", "om_1", "跑一下"), nil),
		tAssistant("a1", "end_turn", tText("r📨: 开始跑了")),
		tUser("<task-notification>\n<task-id>bg</task-id>\n<status>completed</status>\n</task-notification>", nil),
		tAssistant("a2", "end_turn", tText("跑完了。\nr：不是标记\nr📨：全部通过")),
	)
	r, _ := readLastTurn(p)
	if r.chatID != "oc_1" || r.replyTo != "" || r.text != "全部通过" {
		t.Fatalf("turn = %+v", r)
	}
}

// 后台通知开始的回合没写 r📨（比如监听到期重启）：不发
func TestReadLastTurnBackgroundNoMark(t *testing.T) {
	p := writeTranscript(t,
		tUser(feishuNote("oc_1", "om_1", "hi"), nil),
		tAssistant("a1", "end_turn", tText("r📨: hi")),
		tUser("<task-notification><task-id>m</task-id></task-notification>", nil),
		tAssistant("a2", "end_turn", tText("监听到期了，已经重新开起来。")),
	)
	r, _ := readLastTurn(p)
	if r.text != "" {
		t.Fatalf("housekeeping text forwarded: %+v", r)
	}
}

func TestReadLastTurnTyped(t *testing.T) {
	p := writeTranscript(t,
		tUser(feishuNote("oc_1", "om_1", "hi"), nil),
		tAssistant("a1", "end_turn", tText("r📨: hi")),
		tUser([]map[string]any{{"type": "text", "text": "电脑上问的"}}, nil),
		tAssistant("a2", "end_turn", tText("r📨: 这条不该发")),
	)
	r, _ := readLastTurn(p)
	if r.chatID != "" || r.typed != "电脑上问的" || r.text != "r📨: 这条不该发" {
		t.Fatalf("typed turn: %+v", r)
	}
}

// 电脑上打字的回合：同步打的字（去掉系统提示）和最后一次调用工具之后的文字
func TestReadLastTurnTypedMirror(t *testing.T) {
	p := writeTranscript(t,
		tUser([]map[string]any{
			{"type": "text", "text": "<system-reminder>\n内部提示\n</system-reminder>\n"},
			{"type": "text", "text": "为啥没转发"},
			{"type": "image"},
		}, nil),
		tAssistant("a1", "tool_use", tText("我查一下。"), map[string]any{"type": "tool_use", "name": "Bash"}),
		tUser([]map[string]any{{"type": "tool_result", "content": "ok"}}, nil),
		tAssistant("a2", "tool_use", tText("找到了。"), map[string]any{"type": "tool_use", "name": "Edit"}),
		tUser([]map[string]any{{"type": "tool_result", "content": "ok"}}, nil),
		tAssistant("a3", "end_turn", tText("改好了。"), tText("**结论**：现在会同步。")),
	)
	r, _ := readLastTurn(p)
	if r.typed != "为啥没转发\n[图片]" || r.text != "改好了。\n\n**结论**：现在会同步。" || r.lastID != "a3" || !r.complete {
		t.Fatalf("turn = %+v", r)
	}
}

// 电脑上打字后，后台任务完成开始的回合也同步；飞书监听自己的通知不同步
func TestReadLastTurnBackgroundAfterTyped(t *testing.T) {
	head := []string{
		tUser("帮我构建镜像", nil),
		tAssistant("a1", "end_turn", tText("已在后台构建。")),
	}
	p := writeTranscript(t, append(head,
		tUser("<task-notification>\n<status>completed</status>\n<summary>Background command \"构建\" completed</summary>\n</task-notification>", nil),
		tAssistant("a2", "end_turn", tText("构建完成，镜像 v2 已推送。")))...)
	r, _ := readLastTurn(p)
	if !r.mirror || r.typed != "" || r.text != "构建完成，镜像 v2 已推送。" {
		t.Fatalf("turn = %+v", r)
	}
	p = writeTranscript(t, append(head,
		tUser("<task-notification>\n<status>completed</status>\n<summary>Monitor \"飞书消息\" stream ended</summary>\n</task-notification>", nil),
		tAssistant("a2", "end_turn", tText("监听到期，已重启。")))...)
	if r, _ = readLastTurn(p); r.mirror || r.text != "" {
		t.Fatalf("housekeeping mirrored: %+v", r)
	}
	// 后台等待命令结束（有新消息、重启 Monitor）也是维护操作，不同步
	p = writeTranscript(t, append(head,
		tUser("<task-notification>\n<status>completed</status>\n<summary>Background command \"等待飞书消息\" completed (exit code 0)</summary>\n</task-notification>", nil),
		tAssistant("a2", "end_turn", tText("已重新启动监听。")))...)
	if r, _ = readLastTurn(p); r.mirror || r.text != "" {
		t.Fatalf("wait housekeeping mirrored: %+v", r)
	}
}

// 回合很长（开头离文件末尾超过 1MB）时扩大到 8MB 去找
func TestReadLastTurnLongTurn(t *testing.T) {
	big := strings.Repeat("x", turnFirstBytes+1000)
	p := writeTranscript(t,
		tUser("跑个大任务", nil),
		tAssistant("a1", "tool_use", map[string]any{"type": "tool_use", "name": "Bash"}),
		tUser([]map[string]any{{"type": "tool_result", "content": big}}, nil),
		tAssistant("a2", "end_turn", tText("跑完了。")),
	)
	r, _ := readLastTurn(p)
	if !r.mirror || r.typed != "跑个大任务" || r.text != "跑完了。" {
		t.Fatalf("turn = %+v", r)
	}
}

// 提问和计划确认只在最近一次说话来自飞书时发卡片
func TestLastInputFromFeishu(t *testing.T) {
	ask := tAssistant("a9", "tool_use", map[string]any{"type": "tool_use", "name": "AskUserQuestion"})
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"电脑上打字后提问", []string{tUser(feishuNote("oc_1", "om_1", "hi"), nil), tAssistant("a1", "end_turn", tText("r📨: hi")),
			tUser("推测试服吧", nil), ask}, false},
		{"飞书消息后提问", []string{tUser("电脑上的话", nil), tAssistant("a1", "end_turn", tText("好")),
			tUser(feishuNote("oc_1", "om_2", "推吗"), nil), ask}, true},
		{"后台通知开始的回合，最近说话来自飞书", []string{tUser(feishuNote("oc_1", "om_3", "跑一下"), nil), tAssistant("a1", "end_turn", tText("r📨: 在跑")),
			tUser("<task-notification><status>completed</status></task-notification>", nil), ask}, true},
	}
	for _, c := range cases {
		if got := lastInputFromFeishu(writeTranscript(t, c.lines...)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if !lastInputFromFeishu("") { // 读不到记录：照旧发卡片
		t.Error("empty path should default to feishu")
	}
}

// 飞书连着时：选择框提问被拦下改成文字；计划确认在电脑上打字时留在电脑上；开关关着照常放行
func TestQuestionAndPlanHooks(t *testing.T) {
	b, _ := testBridge(t)
	typed := writeTranscript(t, tUser("推测试服吧", nil),
		tAssistant("a1", "tool_use", map[string]any{"type": "tool_use", "name": "AskUserQuestion"}))
	hook := func(tool string) string {
		raw, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": tool,
			"tool_input": map[string]any{}, "transcript_path": typed})
		return b.HandleHook(raw)
	}
	if out := hook("AskUserQuestion"); out != "" {
		t.Fatalf("offline question should pass through: %s", out)
	}
	allowUser(t, b, "ou_user")
	setOnline(b)
	out := hook("AskUserQuestion")
	if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "编号") {
		t.Fatalf("question not turned into text: %s", out)
	}
	if out := hook("ExitPlanMode"); out != "" {
		t.Fatalf("plan after typed input should stay on computer: %s", out)
	}
}

func TestStripContextBlocks(t *testing.T) {
	in := "<artifact-view-context artifact=\"x\">\n{\"a\":1}\n<artifact-content-authored-by-others/>\n</artifact-view-context>\n\n链接飞书 <div>保留</div> <ide_selection>选中</ide_selection>"
	if got := strings.TrimSpace(stripContextBlocks(in)); got != "链接飞书 <div>保留</div>" {
		t.Fatalf("got %q", got)
	}
	if got := stripContextBlocks("a <no-close> b"); got != "a <no-close> b" {
		t.Fatalf("unclosed tag removed: %q", got)
	}
}

// 提交时已发过的"💻 打的字"：回合结束时对得上才拿来引用
func TestTakeEcho(t *testing.T) {
	b, _ := testBridge(t)
	done := make(chan struct{})
	close(done)
	b.echo = &promptEcho{text: "看下代码", done: done, msgs: map[string]sentMsg{"ou_1": {"om_1", "oc_1"}}}
	if m := b.takeEcho("别的话"); m != nil {
		t.Fatalf("mismatched echo taken: %v", m)
	}
	if m := b.takeEcho("看下代码\n[图片]"); m["ou_1"].msgID != "om_1" {
		t.Fatalf("echo not taken: %v", m)
	}
	if b.echo != nil {
		t.Fatal("echo not cleared")
	}
}

func TestTypedText(t *testing.T) {
	cases := map[string]string{
		"<command-name>/feishu:connect</command-name>\n<command-message>x</command-message>\n<command-args></command-args>": "/feishu:connect",
		"<command-name>/loop</command-name><command-args>5m 看看</command-args>":                                              "/loop 5m 看看",
		"[Request interrupted by user]": "",
		"  你好 ":                         "你好",
	}
	for in, want := range cases {
		if got := typedText(in, 0); got != want {
			t.Errorf("typedText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadLastTurnChannelTag(t *testing.T) {
	p := writeTranscript(t,
		tUser(`<channel source="feishu" chat_id="oc_9" message_id="om_9" chat_type="p2p">在吗</channel>`, nil),
		tAssistant("a1", "tool_use", tText("📨 在吗")),
	)
	r, _ := readLastTurn(p)
	if r.chatID != "oc_9" || r.replyTo != "om_9" || r.complete {
		t.Fatalf("turn = %+v", r)
	}
}

func TestReplyText(t *testing.T) {
	cases := []struct {
		texts     []string
		usedReply bool
		want      string
	}{
		{[]string{"📨 问题", "r📨: 一\n二"}, false, "一\n二"},
		{[]string{"r📨: 进度", "中间说明", "**r📨:** 结果"}, false, "进度\n\n结果"},
		{[]string{"📨 问题\n最后一段没写标记"}, false, "最后一段没写标记"}, // 忘了写 r📨：退而发最后一段
		{[]string{"📨 问题", "已用 reply 发过"}, true, ""},     // 用了 reply：不重复发
		{[]string{"📨 只有回显"}, false, ""},
	}
	for _, c := range cases {
		if got := replyText(c.texts, c.usedReply); got != c.want {
			t.Errorf("replyText(%q, %v) = %q, want %q", c.texts, c.usedReply, got, c.want)
		}
	}
}

// 收到过 Stop hook 后，每条飞书消息都带上回复方式提示
func TestReplyHowAfterStopHook(t *testing.T) {
	b, out := testBridge(t)
	allowUser(t, b, "ou_user")
	setOnline(b)
	b.acceptForwarded(textEvent("om_a", "", "第一条"))
	if line, _ := out.ReadString('\n'); strings.Contains(line, "reply_how") {
		t.Fatalf("hint before stop hook: %s", line)
	}
	b.HandleHook(json.RawMessage(`{"hook_event_name":"Stop","transcript_path":""}`))
	b.acceptForwarded(textEvent("om_b", "", "第二条"))
	if line, _ := out.ReadString('\n'); !strings.Contains(line, "reply_how") || !strings.Contains(line, "r📨") {
		t.Fatalf("no hint after stop hook: %s", line)
	}
}
