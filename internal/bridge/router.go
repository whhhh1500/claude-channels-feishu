package bridge

// 多个会话同时连着飞书时的消息分发。
//
// 飞书长连接每条事件只推给一个连接，由拿到锁的会话（hub）接收，再决定交给谁：
//   - 只有它自己在线：直接交给自己，不用引用。
//   - 多个会话在线：看消息引用的是谁的消息（parent_id / root_id）。每个会话记得自己发过和收到过的消息，
//     hub 逐个问"这是你的吗"，认领的会话拿到这条消息。配对码、y/n 文字审批也由持有它的会话认领。
//   - 没人认领：发一张卡片让用户点选发给哪个会话。
//
// 审批卡片的按钮回调同样先到 hub，按卡片里记的进程号转给发卡片的会话。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/whhhh1500/claude-channels-feishu/internal/access"
	"github.com/whhhh1500/claude-channels-feishu/internal/feishu"
	"github.com/whhhh1500/claude-channels-feishu/internal/session"
)

const (
	ownedTTL    = 7 * 24 * time.Hour
	ownedMax    = 20000
	pendingTTL  = 30 * time.Minute
	peerTimeout = 2 * time.Second
)

var selfPID = strconv.Itoa(os.Getpid())

// claimQuery 是 hub 问各会话"这条消息是不是你的"时带的信息。
type claimQuery struct {
	Parent   string `json:"parent,omitempty"`
	Root     string `json:"root,omitempty"`
	Text     string `json:"text,omitempty"`
	ChatType string `json:"chatType,omitempty"`
}

// pendingRoute 是没人认领、等用户在卡片上选会话的消息。
type pendingRoute struct {
	data    *larkim.P2MessageReceiveV1Data
	targets map[string]string // 进程号 → 会话标识
	at      time.Time
	done    bool
}

// ---------------------------------------------------------------- 本会话认领的消息

// own 记下属于本会话的消息（发出去的、收到的、卡片），用户引用它们回复时就能找回本会话。
func (b *Bridge) own(ids ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if len(b.owned) > ownedMax {
		for k, t := range b.owned {
			if now.Sub(t) > ownedTTL || len(b.owned) > ownedMax/2 {
				delete(b.owned, k)
			}
		}
	}
	for _, id := range ids {
		if id != "" {
			b.owned[id] = now
		}
	}
}

func (b *Bridge) owns(id string) bool {
	if id == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.owned[id]
	return ok
}

// claims 判断一条消息该不该交给本会话。
func (b *Bridge) claims(q claimQuery) bool {
	if b.owns(q.Parent) || b.owns(q.Root) {
		return true
	}
	text := strings.TrimSpace(q.Text)
	if text == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if q.ChatType == "p2p" && b.pairCode != "" && time.Now().Before(b.pairExpires) && strings.EqualFold(text, b.pairCode) {
		return true
	}
	if mm := permReplyRe.FindStringSubmatch(text); mm != nil {
		_, ok := b.perms[strings.ToLower(mm[2])]
		return ok
	}
	return false
}

// ---------------------------------------------------------------- hub：分发消息

// onlinePeers 返回除本会话以外打开了飞书开关的会话。
func (b *Bridge) onlinePeers() []session.Peer {
	var out []session.Peer
	for _, p := range session.Peers(b.config().StateDir) {
		if p.Status.Active && p.Info.PID != os.Getpid() {
			out = append(out, p)
		}
	}
	return out
}

// route 是 hub 收到消息后的入口：决定交给自己还是别的会话。
func (b *Bridge) route(ev *larkim.P2MessageReceiveV1) {
	m := ev.Event.Message
	if !b.inboundOK(ev) {
		return
	}
	peers := b.onlinePeers()
	if len(peers) == 0 { // 只有自己在线
		b.handleMessage(ev)
		return
	}
	q := claimQuery{
		Parent:   larkcore.StringValue(m.ParentId),
		Root:     larkcore.StringValue(m.RootId),
		ChatType: larkcore.StringValue(m.ChatType),
	}
	if larkcore.StringValue(m.MessageType) == "text" {
		text, _ := feishu.RenderContent("text", larkcore.StringValue(m.Content))
		q.Text = b.resolveMentions(text, m.Mentions)
	}
	if b.claims(q) {
		b.handleMessage(ev)
		return
	}
	if p := claimPeer(peers, q); p != nil {
		if err := forwardMessage(p.Info, ev.Event); err == nil {
			b.log.Info("routed message", "message_id", larkcore.StringValue(m.MessageId), "to_pid", p.Info.PID)
			return
		} else {
			b.log.Warn("forward message", "pid", p.Info.PID, "err", err)
		}
	}
	// 没人认领：不在白名单的照常走本地流程（丢弃或提示配对），白名单内的让用户选会话。
	if !b.senderAllowed(ev) {
		b.handleMessage(ev)
		return
	}
	b.askRoute(ev.Event, peers)
}

// inboundOK 过滤机器人自己的消息和断线重连后补发的旧消息（handleMessage 里还会再查一遍）。
func (b *Bridge) inboundOK(ev *larkim.P2MessageReceiveV1) bool {
	m, sender := ev.Event.Message, ev.Event.Sender
	senderID := ""
	if sender.SenderId != nil {
		senderID = larkcore.StringValue(sender.SenderId.OpenId)
	}
	if larkcore.StringValue(sender.SenderType) != "user" || senderID == "" || senderID == b.client().BotOpenID() {
		return false
	}
	if ms, err := strconv.ParseInt(larkcore.StringValue(m.CreateTime), 10, 64); err == nil && time.Since(time.UnixMilli(ms)) > staleMsgAfter {
		return false
	}
	return true
}

func (b *Bridge) senderAllowed(ev *larkim.P2MessageReceiveV1) bool {
	acc, err := b.access.Load()
	if err != nil {
		return false
	}
	m := ev.Event.Message
	senderID := larkcore.StringValue(ev.Event.Sender.SenderId.OpenId)
	return acc.Gate(senderID, larkcore.StringValue(m.ChatId), larkcore.StringValue(m.ChatType), b.mentionsBot(m.Mentions)) == access.Allow
}

// claimPeer 同时问所有在线会话，返回第一个认领的。
func claimPeer(peers []session.Peer, q claimQuery) *session.Peer {
	found := make(chan *session.Peer, len(peers))
	var wg sync.WaitGroup
	for i := range peers {
		wg.Add(1)
		go func(p *session.Peer) {
			defer wg.Done()
			if resp, err := session.CallWith(p.Info, "claim", q, peerTimeout); err == nil && resp.OK {
				found <- p
			}
		}(&peers[i])
	}
	go func() { wg.Wait(); close(found) }()
	return <-found
}

func forwardMessage(info *session.Info, data *larkim.P2MessageReceiveV1Data) error {
	resp, err := session.CallWith(info, "message", data, peerTimeout)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

// askRoute 发卡片让用户选这条消息给哪个会话。
func (b *Bridge) askRoute(data *larkim.P2MessageReceiveV1Data, peers []session.Peer) {
	m := data.Message
	self := sessionTag()
	if self == "" { // 关了会话标识也得能分清
		cwd, _ := os.Getwd()
		self = makeTag("", cwd, transcript.id, "")
	}
	targets := map[string]string{selfPID: self}
	type choice struct{ pid, label string }
	choices := []choice{{selfPID, labelFor(self, b.Host())}}
	for _, p := range peers {
		pid := strconv.Itoa(p.Info.PID)
		tag := p.Status.Tag
		if tag == "" { // 旧版本或关了标识的会话
			tag = makeTag("", p.Status.Cwd, p.Status.ClaudeSession, "")
		}
		targets[pid] = tag
		choices = append(choices, choice{pid, labelFor(tag, p.Status.Host)})
	}
	id := access.NewPairCode()
	b.mu.Lock()
	for k, p := range b.pending {
		if time.Since(p.at) > pendingTTL {
			delete(b.pending, k)
		}
	}
	b.pending[id] = &pendingRoute{data: data, targets: targets, at: time.Now()}
	b.mu.Unlock()

	elements := []any{md(fmt.Sprintf("现在有 %d 个 Claude 会话连着飞书，这条消息没有引用任何会话的消息。点一下选择发给谁。\n以后**引用回复**某个会话发的消息，就会直接送到它那里。", len(choices)))}
	for _, c := range choices {
		elements = append(elements, map[string]any{
			"tag": "button", "type": "default", "width": "fill", "text": plain(c.label),
			"behaviors": []any{map[string]any{"type": "callback", "value": map[string]any{"kind": "route", "id": id, "pid": c.pid}}},
		})
	}
	card := cardJSON("📮 这条消息发给哪个会话？", "blue", elements)
	if _, err := b.client().SendCard(context.Background(), larkcore.StringValue(m.ChatId), larkcore.StringValue(m.MessageId), card); err != nil {
		b.log.Warn("send route card", "err", err)
	}
}

func labelFor(tag, host string) string {
	if name := map[string]string{"desktop": "桌面 App", "cli": "命令行"}[host]; name != "" {
		return tag + "（" + name + "）"
	}
	return tag
}

// onRouteCardAction 处理选会话卡片的按钮（在 hub 上）。
func (b *Bridge) onRouteCardAction(ev *callback.CardActionTriggerEvent) *callback.CardActionTriggerResponse {
	v := ev.Event.Action.Value
	id, _ := v["id"].(string)
	pid, _ := v["pid"].(string)
	b.mu.Lock()
	p := b.pending[id]
	var tag string
	ok := p != nil && !p.done
	if ok {
		tag, ok = p.targets[pid]
	}
	if ok {
		p.done = true
	}
	b.mu.Unlock()
	if p == nil {
		return toast("warning", "已失效（超过 30 分钟，或负责接收的会话换了），请引用对应会话的消息重新发")
	}
	if !ok {
		return toast("warning", "这条消息已经发出去了")
	}
	var err error
	if pid == selfPID {
		b.acceptForwarded(p.data)
	} else {
		var info *session.Info
		info, err = session.ReadInfo(filepath.Join(session.Dir(b.config().StateDir), pid+".json"))
		if err == nil {
			err = forwardMessage(info, p.data)
		}
	}
	if err != nil {
		b.mu.Lock()
		p.done = false
		b.mu.Unlock()
		return toast("error", "那个会话联系不上了（可能已退出或断开飞书），请选别的")
	}
	b.log.Info("routed message by card", "to_pid", pid)
	var data map[string]any
	_ = json.Unmarshal([]byte(cardJSON("✅ 已发给 "+tag, "green", []any{md("以后引用这个会话发的消息回复，会直接送到它那里。")})), &data)
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "已发送"},
		Card:  &callback.Card{Type: "raw", Data: data},
	}
}

// forwardCardAction 把审批卡片的按钮回调转给发卡片的会话。返回 nil 表示该由本会话处理。
func (b *Bridge) forwardCardAction(ev *callback.CardActionTriggerEvent) *callback.CardActionTriggerResponse {
	pid, _ := ev.Event.Action.Value["pid"].(string)
	if pid == "" || pid == selfPID || !b.conn.Hub() {
		return nil
	}
	info, err := session.ReadInfo(filepath.Join(session.Dir(b.config().StateDir), pid+".json"))
	if err != nil {
		return toast("warning", "发这张卡片的会话已经退出，请回电脑上处理")
	}
	resp, err := session.CallWith(info, "card", ev.Event, peerTimeout)
	if err != nil || !resp.OK {
		return toast("warning", "发这张卡片的会话联系不上，请回电脑上处理")
	}
	var out callback.CardActionTriggerResponse
	if json.Unmarshal([]byte(resp.Output), &out) != nil {
		return toast("error", "处理失败")
	}
	return &out
}

// ---------------------------------------------------------------- 被转发的一方

// acceptForwarded 处理 hub 转来的消息。
func (b *Bridge) acceptForwarded(data *larkim.P2MessageReceiveV1Data) {
	if data == nil || data.Message == nil || data.Sender == nil {
		return
	}
	if !b.markSeen("msg:" + larkcore.StringValue(data.Message.MessageId)) {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.log.Error("handle message panic", "panic", r)
			}
		}()
		b.handleMessage(&larkim.P2MessageReceiveV1{Event: data})
	}()
}

// handleForwardedCard 处理 hub 转来的卡片按钮回调，返回给飞书的响应 JSON。
func (b *Bridge) handleForwardedCard(raw json.RawMessage) string {
	var req callback.CardActionTriggerRequest
	var resp *callback.CardActionTriggerResponse
	if json.Unmarshal(raw, &req) != nil || req.Action == nil || req.Operator == nil {
		resp = toast("error", "无效操作")
	} else {
		resp = b.localCardAction(&callback.CardActionTriggerEvent{Event: &req})
	}
	out, _ := json.Marshal(resp)
	return string(out)
}

func toast(typ, msg string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: typ, Content: msg}}
}
