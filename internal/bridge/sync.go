package bridge

// 同步：电脑上的聊天界面（A）和飞书（B）保持一致。
// 插件跟读本会话的对话记录，A 里新出现的内容按顺序发到飞书：
//   - 电脑上打的字（包括 Claude 干活时排队插进去的）发成"💻 打的字"；
//   - Claude 写的每段文字照原样发；开头回显飞书消息的"📨 …"行去掉（那条消息本来就在飞书里）；
//   - 飞书发来的消息不再发；工具调用、后台任务通知不发。
// 飞书消息开始的回合发回那个飞书会话（第一条引用对方的消息）；电脑上打字开始的回合私聊发给白名单里的人。
// 只在连着飞书时跟读，从连接那一刻往后同步，不补发以前的记录。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// replyHow 跟着每条飞书消息送给 Claude 的回复方式提示。
const replyHow = "这是飞书发来的消息。回答开头写一行「📨 消息正文」（让电脑前的人看到对方说了什么；飞书里本来就有这条，插件不会再发），然后像平常一样回答：你在电脑上写的每段文字都会按顺序同步到飞书。不要用 reply 工具发文字回复，也不要写「已在飞书回复」之类的话。"

const (
	syncPoll   = 500 * time.Millisecond
	syncTagGap = 5 * time.Minute // 隔这么久没发过，再发时重新带会话标识
	syncSeek   = 4 << 20         // 换到一个已经很大的记录文件时，只从最后 4MB 开始找
)

// syncOut 是一条要发到飞书的消息。
type syncOut struct {
	dm      bool   // 私聊发给白名单里的每个人
	chatID  string // dm=false 时发到这个会话
	replyTo string // 引用这条消息
	text    string
	tag     bool // 带会话标识
}

// syncState 是跟读时的回合状态：当前回答发到哪里。
type syncState struct {
	target     syncOut // 只用 dm / chatID / replyTo
	fromFeishu bool    // 最近一次有人说话来自飞书
	tagNext    bool    // 下一条要带会话标识（换了说话的人之后）
	mirrorOff  bool    // FEISHU_MIRROR=off：电脑上打字的回合不同步
}

type transcriptEntry struct {
	Type             string `json:"type"`
	Timestamp        string `json:"timestamp"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	ThinkingDisplay  string `json:"thinkingDisplay"` // updates：思考块里是干活时的进度说明，桌面 App 当正文显示
	Origin           *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Attachment *struct {
		Type   string `json:"type"`
		Prompt string `json:"prompt"`
		Origin *struct {
			Kind string `json:"kind"`
		} `json:"origin"`
	} `json:"attachment"`
}

type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

// line 处理对话记录的一行，返回要发到飞书的消息。
func (st *syncState) line(raw []byte) []syncOut {
	if !bytes.Contains(raw, []byte(`"type":"user"`)) && !bytes.Contains(raw, []byte(`"type":"assistant"`)) &&
		!bytes.Contains(raw, []byte(`"queued_command"`)) {
		return nil
	}
	var e transcriptEntry
	if json.Unmarshal(raw, &e) != nil || e.IsSidechain || e.IsMeta || e.IsCompactSummary {
		return nil
	}
	switch e.Type {
	case "user":
		if e.Message == nil {
			return nil
		}
		kind := ""
		if e.Origin != nil {
			kind = e.Origin.Kind
		}
		return st.input(inputKind(&e), kind)
	case "attachment": // Claude 干活时插进来的话
		if e.Attachment == nil || e.Attachment.Type != "queued_command" {
			return nil
		}
		kind := ""
		if e.Attachment.Origin != nil {
			kind = e.Attachment.Origin.Kind
		}
		content, _ := json.Marshal(e.Attachment.Prompt)
		e.Message = &struct {
			Content json.RawMessage `json:"content"`
		}{content}
		return st.input(inputKind(&e), kind)
	case "assistant":
		if e.Message == nil {
			return nil
		}
		var blocks []contentBlock
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			return nil
		}
		var out []syncOut
		for _, b := range blocks {
			var text string
			switch {
			case b.Type == "text":
				text = syncText(b.Text)
			case b.Type == "thinking" && e.ThinkingDisplay == "updates":
				text = syncText(b.Thinking) // 桌面 App 把它当正文显示；其他思考块是折叠的推理，不发
			case b.Type == "tool_use" && strings.HasSuffix(b.Name, "turn__wrap_up"):
				text = wrapUpAsk(b.Input) // 桌面 App 的下一步提问框
			}
			if text == "" || (st.mirrorOff && st.target.dm) {
				continue
			}
			o := st.target
			o.text, o.tag = text, st.tagNext
			out = append(out, o)
			st.target.replyTo = "" // 只有第一条引用对方的消息
			st.tagNext = false
		}
		return out
	}
	return nil
}

// input 处理一条"有人说话"的记录。originKind 是记录里的来源（human / task-notification / peer …）。
func (st *syncState) input(in *turnInput, originKind string) []syncOut {
	if in == nil {
		return nil
	}
	switch in.kind {
	case "feishu":
		st.target = syncOut{chatID: in.chatID, replyTo: in.msgID}
		st.fromFeishu, st.tagNext = true, true
	case "typed":
		if originKind != "" && originKind != "human" { // 子任务交回的报告之类，不是人说的
			return nil
		}
		st.target = syncOut{dm: true}
		st.fromFeishu, st.tagNext = false, false
		if in.text == "" || st.mirrorOff {
			return nil
		}
		return []syncOut{{dm: true, text: "💻 " + in.text, tag: true}}
	}
	return nil // 后台任务通知：回答还发到原来的地方
}

// syncText 整理 Claude 的一段文字：去掉开头回显飞书消息的"📨 …"行，去掉旧做法的"r📨:"标记。
func syncText(t string) string {
	lines := strings.Split(t, "\n")
	for len(lines) > 0 {
		s := strings.TrimSpace(lines[0])
		if s != "" && !strings.HasPrefix(s, "📨") {
			break
		}
		lines = lines[1:]
	}
	for i, l := range lines {
		if rest, ok := cutReplyMark(l); ok {
			lines[i] = rest
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// wrapUpAsk 把桌面 App 下一步提问框里的问题写成一行文字。
func wrapUpAsk(raw json.RawMessage) string {
	var in struct {
		Ask     string `json:"ask"`
		Actions []struct {
			Label string `json:"label"`
		} `json:"actions"`
	}
	if json.Unmarshal(raw, &in) != nil || strings.TrimSpace(in.Ask) == "" {
		return ""
	}
	var labels []string
	for _, a := range in.Actions {
		if a.Label != "" {
			labels = append(labels, a.Label)
		}
	}
	s := "❓ " + strings.TrimSpace(in.Ask)
	if len(labels) > 0 {
		s += "（" + strings.Join(labels, " / ") + "）"
	}
	return s
}

// cutReplyMark 认"r📨:"开头的行（冒号全角半角都行，前面允许空格和加粗）。
func cutReplyMark(line string) (string, bool) {
	s := strings.TrimLeft(line, " \t*")
	rest, ok := strings.CutPrefix(s, "r📨")
	if !ok {
		return "", false
	}
	rest = strings.TrimLeft(rest, "*")
	for _, c := range []string{":", "："} {
		if r, ok := strings.CutPrefix(rest, c); ok {
			return strings.TrimLeft(strings.TrimPrefix(r, "**"), " "), true
		}
	}
	return "", false
}

// ---------------------------------------------------------------- 跟读

type syncer struct {
	b    *Bridge
	send func(syncOut) // 测试时替换

	mu      sync.Mutex
	hint    string // hook 给的对话记录路径（/clear 之后会换文件）
	active  bool
	since   time.Time // 连接时刻，换文件时只同步这之后的记录
	path    string
	off     int64
	midLine bool      // off 落在一行中间（换文件时从末尾往前 4MB 开始）
	located time.Time // 上次按会话 ID 找记录文件的时间
	st      syncState

	lastSent map[string]time.Time // 目标 → 上次发出时间，隔久了重新带会话标识
	queue    chan syncOut
}

func newSyncer(b *Bridge) *syncer {
	s := &syncer{b: b, lastSent: map[string]time.Time{}, queue: make(chan syncOut, 512)}
	s.send = s.deliver
	return s
}

// run 一直跟读，直到进程退出。没连飞书时只看一眼开关，不读文件。
func (s *syncer) run() {
	go func() {
		for o := range s.queue {
			s.send(o)
		}
	}()
	for {
		s.poll()
		time.Sleep(syncPoll)
	}
}

func (s *syncer) setHint(path string) {
	if path == "" {
		return
	}
	s.mu.Lock()
	s.hint = path
	s.mu.Unlock()
}

// fromFeishu 报告最近一次有人说话是不是来自飞书（计划确认发到哪一边用）。
func (s *syncer) fromFeishu() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active && s.st.fromFeishu
}

func (s *syncer) poll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.b.conn == nil || !s.b.conn.Active() {
		s.active = false
		return
	}
	path := s.hint
	if path == "" && time.Since(s.located) >= 5*time.Second { // 按会话 ID 找文件要列目录，别每次都找
		s.located = time.Now()
		path = transcript.filePath()
	}
	if path == "" {
		path = s.path
	}
	if path == "" {
		return
	}
	if !s.active { // 刚连上：从现在的文件末尾开始；还没人说话时回答私聊发
		s.active, s.since = true, time.Now()
		s.st = syncState{target: syncOut{dm: true}, mirrorOff: os.Getenv("FEISHU_MIRROR") == "off"}
		s.path, s.off, s.midLine = path, fileSize(path), false
		return
	}
	if path != s.path && sameFile(path, s.path) { // 同一个文件换了种写法（hook 给的路径）
		s.path = path
	}
	if path != s.path { // 换了记录文件（/clear）：新文件从头读，只同步连接之后的记录
		s.path = path
		s.off = max(fileSize(path)-syncSeek, 0)
		s.midLine = s.off > 0
	}
	for _, o := range s.readNew() {
		select {
		case s.queue <- o:
		default:
			s.b.log.Warn("sync queue full, dropping message")
		}
	}
}

// readNew 读出上次之后新写完的行（调用方持有 s.mu）。
func (s *syncer) readNew() []syncOut {
	f, err := os.Open(s.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == s.off {
		return nil
	}
	if st.Size() < s.off { // 文件被重写过：从末尾重新开始
		s.off = st.Size()
		return nil
	}
	if _, err := f.Seek(s.off, io.SeekStart); err != nil {
		return nil
	}
	rd := bufio.NewReaderSize(f, 1<<20)
	if s.midLine { // 从一行中间开始：丢掉不完整的第一行
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return nil
		}
		s.off += int64(len(line))
		s.midLine = false
	}
	var out []syncOut
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil { // 最后一行还没写完，下次再读
			return out
		}
		s.off += int64(len(line))
		if ts, ok := timestampOf(line); ok && ts.Before(s.since) {
			continue
		}
		out = append(out, s.st.line(line)...)
	}
}

// 顶层的 "timestamp"（正文里的会被转义成 \"timestamp\"，匹配不上）
var timestampRe = regexp.MustCompile(`"timestamp":"([^"\\]+)"`)

func timestampOf(line []byte) (time.Time, bool) {
	m := timestampRe.FindSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, string(m[1]))
	return t, err == nil
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// deliver 把一条消息发到飞书。按顺序一条条发，飞书上的顺序和电脑上一致。
func (s *syncer) deliver(o syncOut) {
	b, ctx := s.b, context.Background()
	o.text = b.redactPairCode(o.text)
	if o.dm {
		acc, err := b.access.Load()
		if err != nil {
			return
		}
		for _, openID := range acc.AllowFrom {
			msgID, chatID, err := b.client().SendMarkdownToUser(ctx, openID, s.tagged("dm:"+openID, o))
			if err != nil {
				b.log.Warn("sync to feishu", "err", err)
				continue
			}
			b.trackSent(msgID)
			b.knowChat(chatID)
		}
		return
	}
	if err := b.checkChat(o.chatID); err != nil {
		b.log.Debug("sync chat", "err", err)
		return
	}
	ids, err := b.client().SendMarkdown(ctx, o.chatID, o.replyTo, s.tagged(o.chatID, o))
	b.trackSent(ids...)
	if err != nil {
		b.log.Warn("sync to feishu", "err", err)
		return
	}
	b.clearAcks(o.chatID)
}

// tagged 换了说话的人之后、或隔了一阵没发，就在开头带上会话标识（多个会话共用一个机器人时分得清）。
func (s *syncer) tagged(dest string, o syncOut) string {
	now := time.Now()
	last := s.lastSent[dest]
	s.lastSent[dest] = now
	if o.tag || now.Sub(last) > syncTagGap {
		return withTag(o.text)
	}
	return o.text
}

// redactPairCode 遮掉还有效的配对码：配对码只能在电脑上看到，同步到飞书时不能带出去。
func (b *Bridge) redactPairCode(text string) string {
	b.mu.Lock()
	code := b.pairCode
	b.mu.Unlock()
	if code == "" {
		return text
	}
	return regexp.MustCompile(`(?i)`+regexp.QuoteMeta(code)).ReplaceAllString(text, "******")
}

// knowChat 把私聊会话加进出站白名单，之后 reply 也能发到这里。
func (b *Bridge) knowChat(chatID string) {
	if chatID == "" {
		return
	}
	b.mu.Lock()
	b.knownChats[chatID] = true
	b.mu.Unlock()
}

// ---------------------------------------------------------------- 识别输入

var (
	feishuEventRe = regexp.MustCompile(`飞书消息 (\{.*?\})\s*</event>`)
	channelTagRe  = regexp.MustCompile(`<channel\s[^>]*source="feishu"[^>]*>`)
	attrRe        = regexp.MustCompile(`(\w+)="([^"]*)"`)
	reminderRe    = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	commandRe     = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	cmdArgsRe     = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	contextOpenRe = regexp.MustCompile(`<([a-z][a-z0-9]*[-_][a-z0-9_-]*)(?:\s[^<>]*)?>`)
)

// turnInput 是一条"有人说话"的 user 记录。
type turnInput struct {
	kind   string // feishu / typed / background
	chatID string
	msgID  string
	text   string // typed 时是电脑上打的字（去掉系统注入的部分）
}

// inputKind 判断一条 user 记录是不是"有人说话"：飞书消息、电脑上打的字，或后台任务通知。
// 返回 nil 表示不是新输入（工具结果之类）。
func inputKind(e *transcriptEntry) *turnInput {
	if e.Message == nil {
		return nil
	}
	var text string
	images := 0
	if json.Unmarshal(e.Message.Content, &text) != nil {
		var blocks []contentBlock
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			return nil
		}
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				return nil
			case "text":
				parts = append(parts, b.Text)
			case "image":
				images++
			}
		}
		text = strings.Join(parts, "\n")
	}
	if m := feishuEventRe.FindStringSubmatch(text); m != nil {
		var v struct {
			ChatID    string `json:"chat_id"`
			MessageID string `json:"message_id"`
		}
		if json.Unmarshal([]byte(m[1]), &v) == nil && v.ChatID != "" {
			return &turnInput{kind: "feishu", chatID: v.ChatID, msgID: v.MessageID}
		}
	}
	if tag := channelTagRe.FindString(text); tag != "" {
		attrs := map[string]string{}
		for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
			attrs[m[1]] = m[2]
		}
		if attrs["chat_id"] != "" {
			return &turnInput{kind: "feishu", chatID: attrs["chat_id"], msgID: attrs["message_id"]}
		}
	}
	if strings.Contains(text, "<task-notification>") {
		return &turnInput{kind: "background"}
	}
	return &turnInput{kind: "typed", text: typedText(text, images)}
}

// typedText 取出电脑上打的字：去掉系统提示；斜杠命令显示成"/命令 参数"；图片显示成 [图片]。
func typedText(text string, images int) string {
	if m := commandRe.FindStringSubmatch(text); m != nil {
		cmd := strings.TrimSpace(m[1])
		if a := cmdArgsRe.FindStringSubmatch(text); a != nil && strings.TrimSpace(a[1]) != "" {
			cmd += " " + strings.TrimSpace(a[1])
		}
		text = cmd
	}
	text = strings.TrimSpace(stripContextBlocks(reminderRe.ReplaceAllString(text, "")))
	if strings.HasPrefix(text, "[Request interrupted") {
		return ""
	}
	if images > 0 {
		text = strings.TrimSpace(text + "\n" + strings.Repeat("[图片] ", images))
	}
	return text
}

// stripContextBlocks 去掉桌面 App 附在消息里的上下文块，如 <artifact-view-context …>…</artifact-view-context>、
// <ide_selection>…</ide_selection>。只认带 - 或 _ 的小写标签名，免得误删用户自己写的 HTML。
func stripContextBlocks(text string) string {
	for from := 0; from < len(text); {
		loc := contextOpenRe.FindStringSubmatchIndex(text[from:])
		if loc == nil {
			break
		}
		start, openEnd := from+loc[0], from+loc[1]
		closing := "</" + text[from+loc[2]:from+loc[3]] + ">"
		end := strings.Index(text[openEnd:], closing)
		if end < 0 {
			from = openEnd
			continue
		}
		text = text[:start] + text[openEnd+end+len(closing):]
		from = start
	}
	return text
}
