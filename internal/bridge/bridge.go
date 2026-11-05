// Package bridge 把飞书和一个 Claude Code 会话接起来：收发消息、审批卡片、连接开关和本机控制端口。
package bridge

// 桥接核心：会话状态、飞书入站消息的过滤和转交、出站白名单。

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/whhhh1500/claude-channels-feishu/internal/access"
	"github.com/whhhh1500/claude-channels-feishu/internal/config"
	"github.com/whhhh1500/claude-channels-feishu/internal/feishu"
	"github.com/whhhh1500/claude-channels-feishu/internal/hostinfo"
	"github.com/whhhh1500/claude-channels-feishu/internal/mcp"
)

// 匹配 "y abcde" / "yes abcde" / "n abcde" / "no abcde"，也接受中文"允许/拒绝"。
var permReplyRe = regexp.MustCompile(`(?i)^\s*(y|yes|n|no|允许|同意|拒绝)\s+([a-km-z]{5})\s*$`)

const (
	dedupTTL      = 12 * time.Hour
	staleMsgAfter = 30 * time.Minute
)

type ackEntry struct{ msgID, reactionID string }

type Bridge struct {
	cfg    *config.Config
	log    *slog.Logger
	fs     *feishu.Client
	mcp    *mcp.Server
	access *access.Store
	inbox  string

	mu         sync.Mutex
	seen       map[string]time.Time  // 去重：message_id / event_id
	knownChats map[string]bool       // 出站白名单：只能往收到过合法消息的会话发
	sent       map[string]bool       // 机器人自己发的消息，edit_message 只能改这些
	acks       map[string][]ackEntry // chat_id → 待撤掉的"处理中"表情
	perms      map[string]*permEntry // request_id → 审批状态
	owned      map[string]time.Time  // 属于本会话的消息，多会话时用户引用它们回复就送到本会话（router.go）
	pending    map[string]*pendingRoute

	conn        *Connector
	pairCode    string // 会话里生成的配对码，用户从飞书私聊发给机器人即完成配对
	pairExpires time.Time
	flag        hostinfo.Flag
	flagKnown   bool
	host        string

	monitors   monitorHub              // Monitor 监听的订阅（没有 channel 参数时用它投递）
	hooks      map[string]*hookRequest // 等待飞书回复的 hook 审批
	hookSeen   bool                    // 收到过 PermissionRequest hook：说明插件 hook 在工作，Channel 自带的审批转发就不用了
	lastTurnID string                  // 上次回合结束时发过的最后一条 assistant 记录，防止重复发
	echo       *promptEcho             // 电脑上提交时已发到飞书的"💻 打的字"，等回合结束引用它发回答
	stopHook   bool                    // 收到过 Stop hook：说明回合结束自动发 r📨 段落能用
	msgs       msgCounter              // 本次连接转给 Claude 的消息数（记进连接历史）
	mode       string                  // 本次连接收消息的方式：channel / monitor
}

// New 创建桥接。srv 是本会话的 MCP 服务，飞书消息通过它推给 Claude。
func New(cfg *config.Config, log *slog.Logger, fs *feishu.Client, srv *mcp.Server) *Bridge {
	b := &Bridge{
		cfg: cfg, log: log, fs: fs, mcp: srv,
		access:     access.NewStore(cfg.StateDir),
		inbox:      filepath.Join(cfg.StateDir, "inbox"),
		seen:       map[string]time.Time{},
		knownChats: map[string]bool{},
		sent:       map[string]bool{},
		acks:       map[string][]ackEntry{},
		perms:      map[string]*permEntry{},
		owned:      map[string]time.Time{},
		pending:    map[string]*pendingRoute{},
	}
	b.conn = NewConnector(log, b)
	go transcript.sessionInfo() // 先把对话记录读一遍，第一次回复和面板查询就不用等
	return b
}

// AutoConnect 会话启动时直接连接飞书（FEISHU_AUTOCONNECT）。
// 只在带 channel 参数启动时生效：没带参数时消息要靠 Claude 在会话里启动的监听接收，
// 启动时自动连上也收不到，消息会丢。
func (b *Bridge) AutoConnect(ctx context.Context) error {
	if b.channelFlag() == hostinfo.FlagMissing {
		return fmt.Errorf("FEISHU_AUTOCONNECT 只在用 --channels 或 --dangerously-load-development-channels 启动时生效；这个会话请在里面说\"连接飞书\"")
	}
	_, err := b.conn.Connect(ctx)
	if b.conn.Active() {
		b.setMode("channel")
		b.recordConnect("channel")
	}
	return err
}

// Shutdown 会话结束时断开飞书。
func (b *Bridge) Shutdown() { b.conn.DisconnectFor("exit") }

// client 返回当前飞书客户端（凭据可能在面板里被改过）。
func (b *Bridge) client() *feishu.Client {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fs
}

func (b *Bridge) config() *config.Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg
}

// reloadConfig 重新读取凭据；凭据变了就换一个新客户端。
func (b *Bridge) reloadConfig() (*config.Config, *feishu.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg == nil || cfg.AppID != b.cfg.AppID || cfg.AppSecret != b.cfg.AppSecret || cfg.Domain != b.cfg.Domain {
		b.fs = feishu.New(cfg, b.log)
	}
	b.cfg = cfg
	return cfg, b.fs, nil
}

func (b *Bridge) setHost(h string) {
	b.mu.Lock()
	b.host = h
	b.mu.Unlock()
}

// Host 返回 desktop / cli / sdk / unknown。
func (b *Bridge) Host() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.host == "" {
		return hostinfo.Kind()
	}
	return b.host
}

func (b *Bridge) setFlag(f hostinfo.Flag) {
	b.mu.Lock()
	b.flag, b.flagKnown = f, true
	b.mu.Unlock()
}

func (b *Bridge) channelFlag() hostinfo.Flag {
	b.mu.Lock()
	f, ok := b.flag, b.flagKnown
	b.mu.Unlock()
	if ok {
		return f
	}
	f = hostinfo.ChannelFlag()
	b.setFlag(f)
	return f
}

// markSeen 返回 true 表示第一次见到。
func (b *Bridge) markSeen(keys ...string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if len(b.seen) > 5000 {
		for k, t := range b.seen {
			if now.Sub(t) > dedupTTL {
				delete(b.seen, k)
			}
		}
	}
	for _, k := range keys {
		if k == "" {
			continue
		}
		if t, ok := b.seen[k]; ok && now.Sub(t) <= dedupTTL {
			return false
		}
	}
	for _, k := range keys {
		if k != "" {
			b.seen[k] = now
		}
	}
	return true
}

// ---------------------------------------------------------------- 入站消息

const pairTTL = 10 * time.Minute

// NewPairCode 生成一个新的会话配对码（旧的作废）。
func (b *Bridge) NewPairCode() string {
	code := access.NewPairCode()
	b.mu.Lock()
	b.pairCode, b.pairExpires = code, time.Now().Add(pairTTL)
	b.mu.Unlock()
	return code
}

// tryPair 检查私聊消息是不是当前有效的配对码，是的话把发送人加进白名单。
func (b *Bridge) tryPair(text, senderID, chatID string) bool {
	b.mu.Lock()
	code, exp := b.pairCode, b.pairExpires
	ok := code != "" && time.Now().Before(exp) && strings.EqualFold(strings.TrimSpace(text), code)
	if ok {
		b.pairCode = "" // 一次性
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	err := b.access.Update(func(a *access.Access) error {
		if !slices.Contains(a.AllowFrom, senderID) {
			a.AllowFrom = append(a.AllowFrom, senderID)
		}
		return nil
	})
	ctx := context.Background()
	if err != nil {
		b.log.Error("save pairing", "err", err)
		_, _ = b.client().SendText(ctx, chatID, "", "配对失败："+err.Error())
		return true
	}
	b.mu.Lock()
	b.knownChats[chatID] = true
	b.mu.Unlock()
	if id, err := b.client().SendText(ctx, chatID, "", withTag("配对成功，现在可以直接和 Claude 对话了。")); err == nil {
		b.own(id)
	}
	b.notify("飞书用户已通过配对码完成配对，之后他发的消息会进入本会话。可以 reply 一句简短的欢迎。",
		map[string]string{"event": "paired", "chat_id": chatID, "user_id": senderID})
	b.log.Info("paired", "open_id", senderID)
	return true
}

// OnMessage 是 im.message.receive_v1 的处理函数。必须尽快返回，否则飞书会重发。
func (b *Bridge) OnMessage(_ context.Context, ev *larkim.P2MessageReceiveV1) error {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil || ev.Event.Sender == nil {
		return nil
	}
	eventID := ""
	if ev.EventV2Base != nil && ev.EventV2Base.Header != nil {
		eventID = ev.EventV2Base.Header.EventID
	}
	msgID := larkcore.StringValue(ev.Event.Message.MessageId)
	var keys []string
	if eventID != "" {
		keys = append(keys, "evt:"+eventID)
	}
	if msgID != "" {
		keys = append(keys, "msg:"+msgID)
	}
	if !b.markSeen(keys...) {
		return nil
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.log.Error("handle message panic", "panic", r)
			}
		}()
		b.route(ev)
	}()
	return nil
}

func (b *Bridge) handleMessage(ev *larkim.P2MessageReceiveV1) {
	m := ev.Event.Message
	sender := ev.Event.Sender
	senderType := larkcore.StringValue(sender.SenderType)
	senderID := ""
	if sender.SenderId != nil {
		senderID = larkcore.StringValue(sender.SenderId.OpenId)
	}
	if senderType != "user" || senderID == "" || senderID == b.client().BotOpenID() {
		return
	}
	// 断线重连后飞书会补发旧消息，太旧的直接丢掉。
	if ms, err := strconv.ParseInt(larkcore.StringValue(m.CreateTime), 10, 64); err == nil {
		if time.Since(time.UnixMilli(ms)) > staleMsgAfter {
			b.log.Info("drop stale message", "message_id", larkcore.StringValue(m.MessageId))
			return
		}
	}

	msgID := larkcore.StringValue(m.MessageId)
	chatID := larkcore.StringValue(m.ChatId)
	chatType := larkcore.StringValue(m.ChatType)
	msgType := larkcore.StringValue(m.MessageType)
	b.log.Info("received message", "chat_id", chatID, "chat_type", chatType, "msg_type", msgType, "sender", senderID)

	mentionedBot := b.mentionsBot(m.Mentions)

	acc, err := b.access.Load()
	if err != nil {
		b.log.Error("load access.json", "err", err)
		return
	}
	ctx := context.Background()

	text, atts := feishu.RenderContent(msgType, larkcore.StringValue(m.Content))
	text = b.resolveMentions(text, m.Mentions)

	// 会话配对码：只认私聊，且必须在白名单检查之前。
	if chatType == "p2p" && msgType == "text" && b.tryPair(text, senderID, chatID) {
		return
	}

	switch acc.Gate(senderID, chatID, chatType, mentionedBot) {
	case access.Drop:
		b.log.Info("dropped: sender or chat not allowed", "sender", senderID, "chat_id", chatID, "mentioned_bot", mentionedBot)
		return
	case access.Pair:
		code, resend, err := b.access.StartPairing(senderID, chatID, "")
		if err != nil {
			b.log.Warn("pairing", "err", err)
			return
		}
		lead := "需要先配对"
		if resend {
			lead = "配对还没完成"
		}
		_, _ = b.client().SendText(ctx, chatID, "", fmt.Sprintf(
			"%s。请在运行 Claude Code 的电脑上执行：\n\nfeishu-channel access pair %s\n\n配对码 1 小时内有效。", lead, code))
		return
	}

	// 文字审批："y abcde" / "n abcde"
	if mm := permReplyRe.FindStringSubmatch(text); mm != nil {
		// 群内的额外发言白名单不授予本机工具审批权限。
		if !slices.Contains(acc.AllowFrom, senderID) {
			return
		}
		w := strings.ToLower(mm[1])
		allow := w == "y" || w == "yes" || w == "允许" || w == "同意"
		b.resolvePermission(strings.ToLower(mm[2]), allow, senderID)
		emoji := "OK"
		if !allow {
			emoji = "No"
		}
		_, _ = b.client().AddReaction(ctx, msgID, emoji)
		return
	}

	b.mu.Lock()
	b.knownChats[chatID] = true
	b.mu.Unlock()
	b.own(msgID)

	// "处理中"表情，回复后撤掉。
	if emoji := acc.Ack(); emoji != "" {
		go func() {
			rid, err := b.client().AddReaction(ctx, msgID, emoji)
			if err != nil {
				b.log.Debug("ack reaction", "err", err)
				return
			}
			if acc.ClearAck() {
				b.mu.Lock()
				b.acks[chatID] = append(b.acks[chatID], ackEntry{msgID, rid})
				b.mu.Unlock()
			}
		}()
	}

	meta := map[string]string{
		"chat_id":    chatID,
		"message_id": msgID,
		"chat_type":  chatType,
		"user_id":    senderID,
	}
	if ms, err := strconv.ParseInt(larkcore.StringValue(m.CreateTime), 10, 64); err == nil {
		meta["ts"] = time.UnixMilli(ms).Format(time.RFC3339)
	}
	if t := larkcore.StringValue(m.ThreadId); t != "" {
		meta["thread_id"] = t
	}

	// 图片直接下载，路径放 meta（放正文里会被伪造）。其他附件按需用工具下载。
	var images []string
	var files []feishu.Attachment
	for _, a := range atts {
		if a.Kind == "image" && a.Key != "" {
			p, err := b.client().Download(ctx, msgID, a.Key, "image", b.inbox, "")
			if err != nil {
				b.log.Warn("download image", "err", err)
				continue
			}
			images = append(images, p)
		} else if a.Key != "" {
			files = append(files, a)
		}
	}
	if len(images) > 0 {
		meta["image_path"] = images[0]
		if len(images) > 1 {
			meta["image_paths"] = strings.Join(images, "|")
		}
	}
	if len(files) > 0 {
		a := files[0]
		meta["attachment_kind"] = a.Kind
		meta["attachment_file_key"] = a.Key
		if a.Name != "" {
			meta["attachment_name"] = feishu.SafeAttr(a.Name)
		}
	}

	// 引用回复：把被引用的内容带上。
	if pid := larkcore.StringValue(m.ParentId); pid != "" {
		if q, err := b.client().GetMessageText(ctx, pid); err == nil && strings.TrimSpace(q) != "" {
			q = feishu.Truncate(strings.ReplaceAll(strings.TrimSpace(q), "\n", "\n> "), 1000)
			text = "> " + q + "\n\n" + text
			meta["reply_to_message_id"] = pid
		}
	}

	if strings.TrimSpace(text) == "" {
		text = "(空消息)"
	}
	b.mu.Lock()
	stopHook := b.stopHook
	b.mu.Unlock()
	if stopHook {
		// 每条消息都带上回复方式：会话历史里旧的做法很多时，光靠开头的说明 Claude 常常照着历史来
		meta["reply_how"] = replyHow
	}
	b.deliver(feishu.Truncate(text, 100000), meta)
	b.log.Info("forwarded message", "chat_id", chatID, "message_id", msgID, "type", msgType)
}

func (b *Bridge) mentionsBot(mentions []*larkim.MentionEvent) bool {
	bot := b.client().BotOpenID()
	for _, mt := range mentions {
		if mt.Id != nil && bot != "" && larkcore.StringValue(mt.Id.OpenId) == bot {
			return true
		}
	}
	return false
}

// resolveMentions 去掉 @机器人，其他 @_user_N 换成 @名字。
func (b *Bridge) resolveMentions(text string, mentions []*larkim.MentionEvent) string {
	for _, mt := range mentions {
		key := larkcore.StringValue(mt.Key)
		if key == "" {
			continue
		}
		repl := "@" + larkcore.StringValue(mt.Name)
		if mt.Id != nil && larkcore.StringValue(mt.Id.OpenId) == b.client().BotOpenID() {
			repl = ""
		}
		text = strings.ReplaceAll(text, key, repl)
	}
	return feishu.StripMentions(text)
}

// ---------------------------------------------------------------- 权限审批

// checkChat 出站白名单：只能往收到过合法消息的会话、或 access.json 里登记的群发消息。
func (b *Bridge) checkChat(chatID string) error {
	b.mu.Lock()
	ok := b.knownChats[chatID]
	b.mu.Unlock()
	if ok {
		return nil
	}
	if acc, err := b.access.Load(); err == nil {
		if _, ok := acc.Groups[chatID]; ok {
			return nil
		}
	}
	return fmt.Errorf("chat_id %s 不在允许范围内（只能回复收到过消息的会话）", chatID)
}

// trackSent 记下 reply 发出的消息：edit_message 只能改这些，用户引用它们回复时也认得是本会话。
func (b *Bridge) trackSent(ids ...string) {
	b.mu.Lock()
	for _, id := range ids {
		if id != "" {
			b.sent[id] = true
		}
	}
	b.mu.Unlock()
	b.own(ids...)
}

func (b *Bridge) clearAcks(chatID string) {
	b.mu.Lock()
	list := b.acks[chatID]
	delete(b.acks, chatID)
	b.mu.Unlock()
	for _, a := range list {
		go func(a ackEntry) {
			if err := b.client().DeleteReaction(context.Background(), a.msgID, a.reactionID); err != nil {
				b.log.Debug("clear ack", "err", err)
			}
		}(a)
	}
}
