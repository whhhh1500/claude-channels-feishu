package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/whhhh1500/claude-channels-feishu/internal/config"
	"github.com/whhhh1500/claude-channels-feishu/internal/feishu"
	"github.com/whhhh1500/claude-channels-feishu/internal/hostinfo"
	"github.com/whhhh1500/claude-channels-feishu/internal/mcp"
	"github.com/whhhh1500/claude-channels-feishu/internal/session"
)

func TestPermReplyRegex(t *testing.T) {
	for s, ok := range map[string]bool{
		"y abcde": true, "YES abcde": true, "n qwert": true, "允许 abcde": true,
		"yes abcdl": false, "yes abcd": false, "ok abcde": false, "yes abcde please": false,
	} {
		if got := permReplyRe.MatchString(s); got != ok {
			t.Errorf("%q: got %v want %v", s, got, ok)
		}
	}
}

func TestPermissionCardJSON(t *testing.T) {
	req := mcp.PermissionRequest{RequestID: "abcde", ToolName: "Bash", Description: "list *files*", InputPreview: `{"command":"ls ` + "```" + `"}`}
	for _, d := range []string{"", "allow", "deny"} {
		var v map[string]any
		if err := json.Unmarshal([]byte(permissionCard(req, d)), &v); err != nil {
			t.Fatalf("%q: %v", d, err)
		}
		hasButtons := strings.Contains(permissionCard(req, d), `"callback"`)
		if hasButtons != (d == "") {
			t.Errorf("decided=%q buttons=%v", d, hasButtons)
		}
	}
}

func testBridge(t *testing.T) (*Bridge, *bufio.Reader) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FEISHU_STATE_DIR", dir)
	outR, outW := io.Pipe()
	t.Cleanup(func() { outW.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := &mcp.Server{}
	m.SetOutput(outW, log)
	cfg := &config.Config{StateDir: dir}
	b := New(cfg, log, feishu.New(cfg, log), m)
	return b, bufio.NewReader(outR)
}

func TestSessionPairCode(t *testing.T) {
	b, rd := testBridge(t)
	code := b.NewPairCode()
	b.mu.Lock()
	b.pairExpires = time.Now().Add(-time.Second) // 过期
	b.mu.Unlock()
	if b.tryPair(code, "ou_a", "oc_1") {
		t.Error("expired code accepted")
	}
	code = b.NewPairCode()
	if b.tryPair("wrong1", "ou_a", "oc_1") {
		t.Error("wrong code accepted")
	}
	// 正确配对码：会尝试给飞书发消息（假凭据会失败，忽略）并往会话推通知
	done := make(chan bool, 1)
	go func() { done <- b.tryPair(" "+strings.ToUpper(code)+" ", "ou_a", "oc_1") }()
	line, err := rd.ReadString('\n')
	if err != nil || !strings.Contains(line, `"event":"paired"`) {
		t.Fatalf("push = %q %v", line, err)
	}
	if !<-done {
		t.Fatal("correct code rejected")
	}
	acc, _ := b.access.Load()
	if len(acc.AllowFrom) != 1 || acc.AllowFrom[0] != "ou_a" {
		t.Errorf("allowFrom = %v", acc.AllowFrom)
	}
	if b.tryPair(code, "ou_b", "oc_2") {
		t.Error("code reused")
	}
}

func TestControlRoundTrip(t *testing.T) {
	b, _ := testBridge(t)
	ctl, err := StartControl(b, b.log)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	list := session.List(b.cfg.StateDir)
	if len(list) != 1 || list[0].PID != ctl.info.PID || list[0].Active {
		t.Fatalf("sessions = %+v", list)
	}
	// 口令错误被拒
	bad := ctl.info
	bad.Token = "nope"
	if r, err := session.Call(&bad, "status", time.Second); err != nil || r.OK {
		t.Errorf("bad token: %+v %v", r, err)
	}
	// 没连接时生成配对码应报错
	if r, _ := session.Command(b.cfg.StateDir, ctl.info.PID, "pair"); r == nil || r.OK {
		t.Errorf("pair while disconnected: %+v", r)
	}
	// 没凭据时连接应报错
	if r, _ := session.Command(b.cfg.StateDir, ctl.info.PID, "connect"); r == nil || r.OK || r.Error == "" {
		t.Errorf("connect without creds: %+v", r)
	}
	ctl.Close()
	if len(session.List(b.cfg.StateDir)) != 0 {
		t.Error("closed session still listed")
	}
}

func TestHookAnswers(t *testing.T) {
	perm := &hookRequest{kind: "permission", in: hookInput{
		ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"rm -rf x","description":"删除 x"}`),
		PermissionSuggests: json.RawMessage(`[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"rm -rf x"}],"behavior":"allow","destination":"localSettings"}]`),
	}}
	out, _, err := hookAnswer(perm, "always", nil)
	if err != nil || !strings.Contains(out, `"behavior":"allow"`) || !strings.Contains(out, `"updatedPermissions"`) {
		t.Errorf("always: %s %v", out, err)
	}
	out, label, _ := hookAnswer(perm, "deny", map[string]any{"reason": "用 trash 代替"})
	if !strings.Contains(out, `"behavior":"deny"`) || !strings.Contains(out, "用 trash 代替") || !strings.Contains(label, "用 trash 代替") {
		t.Errorf("deny: %s", out)
	}
	if out, _, _ := hookAnswer(perm, "skip", nil); out != "" {
		t.Errorf("skip should be empty: %s", out)
	}

	q := &hookRequest{kind: "question", in: hookInput{ToolName: "AskUserQuestion", ToolInput: json.RawMessage(`{"questions":[
		{"question":"用哪个框架？","header":"框架","multiSelect":false,"options":[{"label":"React"},{"label":"Vue"}]},
		{"question":"要哪些部分？","header":"部分","multiSelect":true,"options":[{"label":"A"},{"label":"B"},{"label":"C"}]},
		{"question":"叫什么名字？","header":"名字","multiSelect":false,"options":[{"label":"foo"},{"label":"bar"}]}]}`)}}
	if _, _, err := hookAnswer(q, "submit", map[string]any{"q0": "React"}); err == nil {
		t.Error("missing answers should error")
	}
	out, _, err = hookAnswer(q, "submit", map[string]any{"q0": "Vue", "q1": []any{"A", "C"}, "q2": "foo", "other2": "自定义名"})
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		H struct {
			Decision string         `json:"permissionDecision"`
			Input    map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	ans := parsed.H.Input["answers"].(map[string]any)
	if parsed.H.Decision != "allow" || ans["用哪个框架？"] != "Vue" || ans["要哪些部分？"] != "A, C" || ans["叫什么名字？"] != "自定义名" {
		t.Errorf("answers = %v", ans)
	}
	if _, ok := parsed.H.Input["questions"]; !ok {
		t.Error("questions must be echoed back")
	}

	plan := &hookRequest{kind: "plan", in: hookInput{ToolName: "ExitPlanMode", ToolInput: json.RawMessage(`{"plan":"# 计划\n1. 做 A"}`)}}
	out, _, _ = hookAnswer(plan, "revise", map[string]any{"reason": "先做 B"})
	if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "先做 B") {
		t.Errorf("plan revise: %s", out)
	}
	out, _, _ = hookAnswer(plan, "approve", nil)
	if !strings.Contains(out, `"permissionDecision":"allow"`) || !strings.Contains(out, "做 A") {
		t.Errorf("plan approve: %s", out)
	}

	for _, r := range []*hookRequest{perm, q, plan} {
		r.id = "abcdef"
		c, err := hookCard(r)
		if err != nil || !json.Valid([]byte(c)) || !json.Valid([]byte(resolvedCard(r, "✅ ok"))) {
			t.Errorf("%s card invalid: %v", r.kind, err)
		}
	}
}

func TestMonitorAndHookOverControl(t *testing.T) {
	b, _ := testBridge(t)
	ctl, err := StartControl(b, b.log)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	if s := session.FindOwn([]int{os.Getppid()}); s == nil || s.PID != ctl.info.PID {
		t.Fatalf("findOwnSession = %+v", s)
	}
	conn, rd, err := session.Dial(&ctl.info, "subscribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if ack, _ := rd.ReadString('\n'); !strings.Contains(ack, `"ok":true`) {
		t.Fatalf("ack = %q", ack)
	}
	if !b.waitMonitor(2 * time.Second) {
		t.Fatal("monitor not registered")
	}
	b.setFlag(hostinfo.FlagMissing)
	b.deliver("你好", map[string]string{"chat_id": "oc_1"})
	line, err := rd.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "飞书消息 ") || !strings.Contains(line, `"chat_id":"oc_1"`) || !strings.Contains(line, `"text":"你好"`) {
		t.Errorf("line = %q %v", line, err)
	}
	// 开关没开：hook 立即返回空（不做决定）
	hc, hrd, err := session.Dial(&ctl.info, "hook", json.RawMessage(`{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	_ = hc.SetDeadline(time.Now().Add(3 * time.Second))
	resp, err := hrd.ReadString('\n')
	if err != nil || strings.Contains(resp, `"output"`) {
		t.Errorf("hook resp = %q %v", resp, err)
	}
}

func TestMonitorBacklog(t *testing.T) {
	var h monitorHub
	if h.publish("x") {
		t.Error("no queue, no subs: should report not delivered")
	}
	h.setQueue(true)
	for i := 0; i < 3; i++ {
		if !h.publish(fmt.Sprintf("m%d", i)) {
			t.Error("queued publish should report true")
		}
	}
	ch := h.add()
	for i := 0; i < 3; i++ {
		if got := <-ch; got != fmt.Sprintf("m%d", i) {
			t.Errorf("backlog order: %s", got)
		}
	}
	h.publish("live")
	if got := <-ch; got != "live" {
		t.Errorf("live = %s", got)
	}
	h.setQueue(false)
}

// 空闲时的后台等待：有暂存消息才返回 true；断开飞书返回 false；对方退出也返回
func TestWaitBacklog(t *testing.T) {
	var h monitorHub
	h.setQueue(true)
	got := make(chan bool, 1)
	go func() { got <- h.waitBacklog(nil) }()
	select {
	case v := <-got:
		t.Fatalf("returned early: %v", v)
	case <-time.After(100 * time.Millisecond):
	}
	h.publish("m1")
	if v := <-got; !v {
		t.Fatal("not woken by message")
	}
	if !h.waitBacklog(nil) { // 已有暂存消息：立即返回
		t.Fatal("backlog present but waited")
	}
	h.add() // Monitor 启动：取走暂存消息
	go func() { got <- h.waitBacklog(nil) }()
	time.Sleep(50 * time.Millisecond)
	h.setQueue(false)
	if v := <-got; v {
		t.Fatal("disconnect should return false")
	}
	h.setQueue(true)
	done := make(chan struct{})
	go func() { got <- h.waitBacklog(done) }()
	close(done)
	if v := <-got; v {
		t.Fatal("caller gone should return false")
	}
}

// 通过控制端口的完整流程：wait 一直挂着，来消息后返回 ok
func TestWaitOverControl(t *testing.T) {
	b, _ := testBridge(t)
	ctl, err := StartControl(b, b.log)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	b.setMode("monitor")
	conn, rd, err := session.Dial(&ctl.info, "wait", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(100 * time.Millisecond)
	b.deliver("你好", map[string]string{"chat_id": "oc_1"})
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if resp, err := rd.ReadString('\n'); err != nil || !strings.Contains(resp, `"ok":true`) {
		t.Fatalf("wait resp = %q %v", resp, err)
	}
}

func TestMonitorLatestOnly(t *testing.T) {
	var h monitorHub
	a := h.add()
	b := h.add()
	h.publish("x")
	select {
	case <-a:
		t.Error("older subscriber should not receive")
	default:
	}
	if got := <-b; got != "x" {
		t.Errorf("newest got %q", got)
	}
	h.remove(b)
	h.publish("y")
	if got := <-a; got != "y" {
		t.Errorf("after newest left, older should receive, got %q", got)
	}
}
