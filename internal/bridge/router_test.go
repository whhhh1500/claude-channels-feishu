package bridge

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"encoding/json"
	"github.com/whhhh1500/claude-channels-feishu/internal/config"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/whhhh1500/claude-channels-feishu/internal/access"
	"github.com/whhhh1500/claude-channels-feishu/internal/mcp"
	"github.com/whhhh1500/claude-channels-feishu/internal/session"
)

func setOnline(b *Bridge) {
	b.conn.mu.Lock()
	b.conn.cancel = func() {}
	b.conn.mu.Unlock()
}

func allowUser(t *testing.T, b *Bridge, openID string) {
	t.Helper()
	if err := b.access.Update(func(a *access.Access) error {
		a.AllowFrom = append(a.AllowFrom, openID)
		a.AckReaction = new(string) // 不加"处理中"表情，免得测试里调飞书接口
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClaims(t *testing.T) {
	b, _ := testBridge(t)
	b.own("om_mine")
	if !b.claims(claimQuery{Parent: "om_mine"}) || !b.claims(claimQuery{Parent: "om_x", Root: "om_mine"}) {
		t.Error("own message not claimed")
	}
	if b.claims(claimQuery{Parent: "om_other", Text: "hello"}) {
		t.Error("claimed a foreign message")
	}
	code := b.NewPairCode()
	if !b.claims(claimQuery{ChatType: "p2p", Text: strings.ToUpper(code)}) || b.claims(claimQuery{ChatType: "group", Text: code}) {
		t.Error("pair code claim")
	}
	b.perms["abcde"] = &permEntry{req: mcp.PermissionRequest{RequestID: "abcde"}, at: time.Now()}
	if !b.claims(claimQuery{Text: "y abcde"}) || b.claims(claimQuery{Text: "y qwert"}) {
		t.Error("permission reply claim")
	}
}

func textEvent(msgID, root, text string) *larkim.P2MessageReceiveV1Data {
	content, _ := json.Marshal(map[string]string{"text": text})
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)
	s := func(v string) *string { return &v }
	return &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: s("ou_user")}, SenderType: s("user")},
		Message: &larkim.EventMessage{
			MessageId: s(msgID), RootId: s(root), ChatId: s("oc_dm"), ChatType: s("p2p"),
			MessageType: s("text"), Content: s(string(content)), CreateTime: s(now),
		},
	}
}

// hub 问"是不是你的"、把消息转过去，对方照常投递给 Claude。
func TestForwardMessageToPeer(t *testing.T) {
	peer, out := testBridge(t)
	allowUser(t, peer, "ou_user")
	ctl, err := StartControl(peer, peer.log)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	peers := []session.Peer{{Info: &ctl.info}}

	// 开关关着的会话不认领
	peer.own("om_reply")
	if claimPeer(peers, claimQuery{Root: "om_reply"}) != nil {
		t.Fatal("offline session claimed a message")
	}
	setOnline(peer)
	if claimPeer(peers, claimQuery{Root: "om_other"}) != nil {
		t.Fatal("claimed a foreign message")
	}
	if claimPeer(peers, claimQuery{Root: "om_reply"}) == nil {
		t.Fatal("peer did not claim its own message")
	}

	data := textEvent("om_new", "om_reply", "转给我")
	if err := forwardMessage(&ctl.info, data); err != nil {
		t.Fatal(err)
	}
	line, err := out.ReadString('\n')
	if err != nil || !strings.Contains(line, "转给我") || !strings.Contains(line, "om_new") {
		t.Fatalf("delivered = %q %v", line, err)
	}
	// 收到的消息也归它：之后引用这条也能找回来；重复转发不会再投递
	if !peer.owns("om_new") {
		t.Error("received message not owned")
	}
	if err := forwardMessage(&ctl.info, data); err != nil {
		t.Fatal(err)
	}
	if err := peer.checkChat("oc_dm"); err != nil {
		t.Errorf("chat should be allowed for replies: %v", err)
	}
}

// 审批卡片的按钮回调被转给发卡片的会话处理。
func TestForwardedHookCard(t *testing.T) {
	peer, _ := testBridge(t)
	allowUser(t, peer, "ou_user")
	ctl, err := StartControl(peer, peer.log)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	req := &hookRequest{id: "abcde", kind: "permission", in: hookInput{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`)}, result: make(chan string, 1)}
	peer.hooks = map[string]*hookRequest{"abcde": req}

	ev := &callback.CardActionTriggerRequest{
		Operator: &callback.Operator{OpenID: "ou_user"},
		Action:   &callback.CallBackAction{Value: map[string]interface{}{"kind": "hook", "id": "abcde", "action": "allow", "pid": selfPID}},
	}
	resp, err := session.CallWith(&ctl.info, "card", ev, 2*time.Second)
	if err != nil || !resp.OK || !strings.Contains(resp.Output, "已提交") {
		t.Fatalf("card resp = %+v %v", resp, err)
	}
	if out := <-req.result; !strings.Contains(out, `"behavior":"allow"`) {
		t.Errorf("hook output = %s", out)
	}
	// 不在白名单的人点按钮被拒
	ev.Operator.OpenID = "ou_stranger"
	resp, _ = session.CallWith(&ctl.info, "card", ev, 2*time.Second)
	if resp == nil || !strings.Contains(resp.Output, "没有审批权限") {
		t.Errorf("stranger resp = %+v", resp)
	}
}

// 选会话卡片：点了之后消息交给选中的会话（这里是自己），只能点一次。
func TestRouteCardToSelf(t *testing.T) {
	b, out := testBridge(t)
	allowUser(t, b, "ou_user")
	setOnline(b)
	b.pending["qwert"] = &pendingRoute{data: textEvent("om_pick", "", "选我"), targets: map[string]string{selfPID: "me"}, at: time.Now()}
	ev := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Operator: &callback.Operator{OpenID: "ou_user"},
		Action:   &callback.CallBackAction{Value: map[string]interface{}{"kind": "route", "id": "qwert", "pid": selfPID}},
	}}
	if r := b.onRouteCardAction(ev); r.Card == nil {
		t.Fatalf("resp = %+v", r.Toast)
	}
	if line, err := out.ReadString('\n'); err != nil || !strings.Contains(line, "选我") {
		t.Fatalf("delivered = %q %v", line, err)
	}
	if r := b.onRouteCardAction(ev); r.Card != nil {
		t.Error("route card accepted twice")
	}
	ev.Event.Action.Value["id"] = "zzzzz"
	if r := b.onRouteCardAction(ev); r.Toast == nil || !strings.Contains(r.Toast.Content, "失效") {
		t.Error("unknown route id")
	}
}

// 拿不到锁的在线会话等着；锁被放开后它接手负责接收，关掉开关时把锁还回去。
func TestStandbyTakesOverLock(t *testing.T) {
	addr := "127.0.0.1:47899"
	t.Setenv("FEISHU_LOCK_ADDR", addr)
	held, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skip("port busy:", err)
	}
	b, _ := testBridge(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	b.conn.mu.Lock()
	b.conn.cancel, b.conn.done = cancel, done
	b.conn.mu.Unlock()
	go b.conn.run(ctx, &config.Config{}, nil, make(chan struct{}, 1), done)

	time.Sleep(lockRetry + 500*time.Millisecond)
	if b.conn.Hub() {
		t.Fatal("became hub while lock was held")
	}
	if !b.conn.Ready() {
		t.Error("standby session should report ready")
	}
	held.Close()
	deadline := time.Now().Add(2*lockRetry + time.Second)
	for !b.conn.Hub() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !b.conn.Hub() {
		t.Fatal("did not take over the lock")
	}
	if !b.conn.Disconnect() {
		t.Fatal("disconnect")
	}
	if b.conn.Hub() {
		t.Error("still hub after disconnect")
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal("lock not released:", err)
	}
	l.Close()
}

func TestSessionTag(t *testing.T) {
	cases := []struct{ title, start, id, cwd, want string }{
		{"report-bot", `D:\work\project\acme\shop`, "1a2b3c4d-4c6f", `D:\work\project\acme\shop\api`, `report-bot#1a2b3c4d#...acme\shop\api`},
		{"", `D:\work\project\acme\shop`, "1a2b3c4d-4c6f", "/d/work/project/acme/shop/api", `shop#1a2b3c4d#...acme\shop\api`},
		{"", `D:\cc`, "abc", `D:\cc`, `cc#abc#D:\cc`},
		{"", `D:\cc`, "", "", "cc"},
	}
	if runtime.GOOS != "windows" {
		t.Skip("paths are Windows-style")
	}
	for _, c := range cases {
		if got := makeTag(c.title, c.start, c.id, c.cwd); got != c.want {
			t.Errorf("makeTag(%q,%q,%q,%q) = %q, want %q", c.title, c.start, c.id, c.cwd, got, c.want)
		}
	}
}

func TestTranscriptState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	proj := filepath.Join(dir, "projects", "p")
	os.MkdirAll(proj, 0o755)
	file := filepath.Join(proj, "sid.jsonl")
	write := func(s string) {
		f, _ := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		f.WriteString(s)
		f.Close()
	}
	write(`{"type":"user","cwd":"D:\\a\\b","message":{"content":[{"type":"tool_use","input":{"cwd":"X"}}]}}` + "\n")
	write(`{"type":"custom-title","customTitle":"old","sessionId":"sid"}` + "\n")
	st := &transcriptState{id: "sid"}
	if title, cwd := st.sessionInfo(); title != "old" || cwd != `D:\a\b` {
		t.Fatalf("got %q %q", title, cwd)
	}
	write(`{"type":"custom-title","customTitle":"new"}` + "\n" + `{"type":"assistant","cwd":"D:\\a\\c"}` + "\n" + `{"type":"assistant","cwd":"D:\\half`)
	st.checked = time.Time{}
	if title, cwd := st.sessionInfo(); title != "new" || cwd != `D:\a\c` {
		t.Fatalf("got %q %q", title, cwd)
	}
}
