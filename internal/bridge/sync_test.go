package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func tTool(name string, input map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "name": name, "input": input}
}

func human() map[string]any { return map[string]any{"origin": map[string]any{"kind": "human"}} }

// 干活时插进来的话（桌面 App 记成 queued_command 附件）
func tQueued(prompt, kind string) string {
	b, _ := json.Marshal(map[string]any{"type": "attachment", "attachment": map[string]any{
		"type": "queued_command", "prompt": prompt, "origin": map[string]any{"kind": kind}}})
	return string(b)
}

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

// 一段贴近真实记录的对话：电脑上打字、干活中插话、飞书中途来消息、后台通知、子任务报告、下一步提问框、群消息
func TestSyncLines(t *testing.T) {
	st := &syncState{target: syncOut{dm: true}}
	type want struct {
		dm          bool
		chat, reply string
		text        string
		tag         bool
	}
	sidechain := `{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"子任务里的话"}]}}`
	steps := []struct {
		line string
		want []want
	}{
		{tUser("推测试服，检查完再上生产", human()), []want{{dm: true, text: "💻 推测试服，检查完再上生产", tag: true}}},
		{tAssistant("a1", "tool_use", tText("先看一下要推哪些提交。"), tTool("Bash", map[string]any{"command": "git log"})),
			[]want{{dm: true, text: "先看一下要推哪些提交。"}}},
		{tUser([]map[string]any{{"type": "tool_result", "content": "ok"}}, nil), nil},
		{tQueued("那几个测试数据不用管", "human"), []want{{dm: true, text: "💻 那几个测试数据不用管", tag: true}}},
		{tQueued(feishuNote("oc_1", "om_1", "现在做到哪一步了"), "task-notification"), nil},
		{tAssistant("a2", "tool_use", tText("📨 现在做到哪一步了\n\n进度：6 步走到第 4 步。")),
			[]want{{chat: "oc_1", reply: "om_1", text: "进度：6 步走到第 4 步。", tag: true}}},
		{tAssistant("a3", "tool_use", map[string]any{"type": "thinking", "thinking": "想一想"}), nil},
		{`{"type":"assistant","thinkingDisplay":"updates","message":{"content":[{"type":"thinking","thinking":""}]}}`, nil},
		{`{"type":"assistant","thinkingDisplay":"updates","message":{"content":[{"type":"thinking","thinking":"容器还没换新，接着等。\n\n"}]}}`,
			[]want{{chat: "oc_1", text: "容器还没换新，接着等。"}}},
		{`{"type":"assistant","thinkingDisplay":"summarized","message":{"content":[{"type":"thinking","thinking":"折叠的推理"}]}}`, nil},
		{tAssistant("a4", "end_turn", tText("已推测试服，等重建完再逐项检查。")), []want{{chat: "oc_1", text: "已推测试服，等重建完再逐项检查。"}}},
		{tUser("<task-notification>\n<status>completed</status>\n<summary>Background command done</summary>\n</task-notification>",
			map[string]any{"origin": map[string]any{"kind": "task-notification"}}), nil},
		{tAssistant("a5", "end_turn", tText("测试服重建完了。")), []want{{chat: "oc_1", text: "测试服重建完了。"}}},
		{tUser("[Subagent hand-back] 报告内容", map[string]any{"origin": map[string]any{"kind": "peer"}}), nil},
		{sidechain, nil},
		{tUser("<system-reminder>x</system-reminder>", map[string]any{"isMeta": true}), nil},
		{tUser("看看结果", human()), []want{{dm: true, text: "💻 看看结果", tag: true}}},
		{tAssistant("a6", "tool_use", tTool("mcp__ccd_turn__wrap_up", map[string]any{"recap": "都好了", "ask": "要上生产吗？",
			"actions": []map[string]any{{"label": "上生产"}, {"label": "先不"}}})), []want{{dm: true, text: "❓ 要上生产吗？（上生产 / 先不）"}}},
		{tAssistant("a7", "end_turn", tText("**r📨:** 旧格式的回复")), []want{{dm: true, text: "旧格式的回复"}}},
		{tUser(`<channel source="feishu" chat_id="oc_g" message_id="om_g" chat_type="group">在吗</channel>`, nil), nil},
		{tAssistant("a8", "end_turn", tText("📨 在吗\n在。")), []want{{chat: "oc_g", reply: "om_g", text: "在。", tag: true}}},
		{tAssistant("a9", "end_turn", tText("📨 只有回显")), nil},
	}
	for i, s := range steps {
		got := st.line([]byte(s.line))
		if len(got) != len(s.want) {
			t.Fatalf("step %d: got %+v, want %+v", i, got, s.want)
		}
		for j, w := range s.want {
			g := got[j]
			if g.dm != w.dm || g.chatID != w.chat || g.replyTo != w.reply || g.text != w.text || g.tag != w.tag {
				t.Errorf("step %d: got %+v, want %+v", i, g, w)
			}
		}
	}
	if !st.fromFeishu {
		t.Error("最近一次说话来自飞书群，fromFeishu 应为 true")
	}
}

// FEISHU_MIRROR=off：电脑上打字的回合不同步，飞书来的照常回
func TestSyncMirrorOff(t *testing.T) {
	st := &syncState{target: syncOut{dm: true}, mirrorOff: true}
	if out := st.line([]byte(tUser("打的字", human()))); len(out) != 0 {
		t.Fatalf("typed synced: %+v", out)
	}
	if out := st.line([]byte(tAssistant("a1", "end_turn", tText("回答")))); len(out) != 0 {
		t.Fatalf("typed turn answer synced: %+v", out)
	}
	st.line([]byte(tUser(feishuNote("oc_1", "om_1", "hi"), nil)))
	if out := st.line([]byte(tAssistant("a2", "end_turn", tText("📨 hi\n你好")))); len(out) != 1 || out[0].chatID != "oc_1" {
		t.Fatalf("feishu turn not synced: %+v", out)
	}
}

// 跟读文件：连上时从末尾开始；只处理写完的行；/clear 换文件后只同步连接之后的记录
func TestSyncPoll(t *testing.T) {
	b, _ := testBridge(t)
	s := b.mirror
	dir := t.TempDir()
	p1 := filepath.Join(dir, "one.jsonl")
	if err := os.WriteFile(p1, []byte(tUser("连接之前的话", human())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.setHint(p1)
	s.poll() // 没连飞书：不读
	if s.active {
		t.Fatal("active while offline")
	}
	setOnline(b)
	s.poll() // 刚连上：记下末尾
	stamp := func(line string, at time.Time) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(line), &m)
		m["timestamp"] = at.UTC().Format(time.RFC3339Nano)
		raw, _ := json.Marshal(m)
		return string(raw)
	}
	appendTo := func(p, text string) {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(text)
		f.Close()
	}
	full := stamp(tUser("连上之后打的字", human()), time.Now())
	appendTo(p1, full[:20]) // 只写了半行
	s.poll()
	if n := len(s.queue); n != 0 {
		t.Fatalf("half line sent: %d", n)
	}
	appendTo(p1, full[20:]+"\n")
	s.poll()
	if o := <-s.queue; o.text != "💻 连上之后打的字" {
		t.Fatalf("got %+v", o)
	}

	// 同一个文件换了种写法：不当成换文件，不重发
	s.setHint(filepath.Join(dir, ".", "one.jsonl"))
	s.poll()
	if n := len(s.queue); n != 0 {
		t.Fatalf("same file resent: %d", n)
	}

	// /clear 之后换了新文件：连接之前的记录不发，之后的发
	p2 := filepath.Join(dir, "two.jsonl")
	appendTo(p2, stamp(tUser("连接之前写进新文件的话", human()), time.Now().Add(-time.Hour))+"\n"+
		stamp(tUser("新文件里的话", human()), time.Now())+"\n")
	s.setHint(p2)
	s.poll()
	if n := len(s.queue); n != 1 {
		t.Fatalf("queue = %d, want 1", n)
	}
	if o := <-s.queue; o.text != "💻 新文件里的话" {
		t.Fatalf("got %+v", o)
	}
}

// 配对码只能在电脑上看到：同步到飞书时遮掉
func TestRedactPairCode(t *testing.T) {
	b, _ := testBridge(t)
	if got := b.redactPairCode("配对码 abcdef"); got != "配对码 abcdef" {
		t.Fatalf("redacted without code: %q", got)
	}
	b.pairCode = "abcdef"
	if got := b.redactPairCode("配对码：ABCDEF，私聊发给机器人"); strings.Contains(strings.ToLower(got), "abcdef") {
		t.Fatalf("code leaked: %q", got)
	}
}

// 每条飞书消息都带新的回复方式提示
func TestReplyHowOnEveryMessage(t *testing.T) {
	b, out := testBridge(t)
	allowUser(t, b, "ou_user")
	setOnline(b)
	b.acceptForwarded(textEvent("om_a", "", "第一条"))
	if line, _ := out.ReadString('\n'); !strings.Contains(line, "reply_how") || strings.Contains(line, "r📨") {
		t.Fatalf("hint missing or outdated: %s", line)
	}
}

// 飞书连着时：选择框提问被拦下改成文字；计划确认在电脑上打字时留在电脑上；开关关着照常放行
func TestQuestionAndPlanHooks(t *testing.T) {
	b, _ := testBridge(t)
	typed := writeTranscript(t, tUser("推测试服吧", human()),
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
