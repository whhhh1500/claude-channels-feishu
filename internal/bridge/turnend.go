package bridge

// 回合结束（Stop hook）时，把 Claude 在聊天界面里写的"r📨: ……"那段原样发到飞书。
// 电脑上像聊天窗口一样："📨 用户的话" / "r📨: 回复"，看到的和飞书收到的是同一份内容。
// 只在"最近一次有人说话是从飞书来的"时才发；电脑上直接打字的回合不转发。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// replyHow 跟着每条飞书消息送给 Claude 的回复方式提示。
const replyHow = "先在回答开头写一行「📨 消息正文」；给对方的回复写成一段以「r📨: 」开头的文字放在最后，回合结束时插件原样发到飞书。答案只写一次、就写在 r📨 里，不要先用普通文字写一遍再在 r📨 里重复。不要用 reply 工具发回复，也不要写「我已在飞书回复」之类的话。"

const (
	turnFirstBytes = 1 << 20 // 先看对话记录最后 1MB，多数回合都在里面
	turnTailBytes  = 8 << 20 // 最多看最后 8MB
	turnFlushWait  = 2 * time.Second
)

// turnResult 是从对话记录里读出的最后一回合。
type turnResult struct {
	text     string // 要发到飞书的内容（这一回合的 r📨 段落）
	lastID   string // 最后一条 assistant 记录的 uuid，用来去重
	chatID   string // 最近一条飞书消息所在的会话；最近一次输入来自电脑时为空
	replyTo  string // 这一回合是由那条飞书消息开始的，就引用它
	typed    string // 这一回合是电脑上打字开始的：打的内容
	mirror   bool   // 同步到飞书私聊：电脑上打字的回合，或电脑上打字之后由后台任务通知开始的回合
	complete bool   // 最后一条 assistant 记录已经是回合结尾（不是等工具结果）
}

type transcriptEntry struct {
	Type             string `json:"type"`
	UUID             string `json:"uuid"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Message          *struct {
		Content    json.RawMessage `json:"content"`
		StopReason *string         `json:"stop_reason"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

var (
	feishuEventRe = regexp.MustCompile(`飞书消息 (\{.*?\})\s*</event>`)
	channelTagRe  = regexp.MustCompile(`<channel\s[^>]*source="feishu"[^>]*>`)
	attrRe        = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

// turnInput 是一条"有人说话"的 user 记录。
type turnInput struct {
	kind   string // feishu / typed / background / housekeeping
	chatID string
	msgID  string
	text   string // typed 时是电脑上打的字（去掉系统注入的部分）
}

var (
	reminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	commandRe  = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	cmdArgsRe  = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	summaryRe  = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
)

// inputKind 判断一条 user 记录是不是"有人说话"：飞书消息、电脑上打的字，或后台任务通知。
// 返回 nil 表示不是新输入（工具结果、系统注入的内容）。
func inputKind(e *transcriptEntry) *turnInput {
	if e.IsMeta || e.IsCompactSummary || e.Message == nil {
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
		kind := "background"
		if m := summaryRe.FindStringSubmatch(text); m != nil && strings.Contains(m[1], "飞书") {
			kind = "housekeeping" // 飞书监听自己的通知（如到期结束），不算一回合正事
		}
		return &turnInput{kind: kind}
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

// readLastTurn 从对话记录末尾往前找最后一回合。先看最后 1MB，找不到这一回合的开头再看最后 8MB。
func readLastTurn(path string) (*turnResult, error) {
	r, found, err := readTurnTail(path, turnFirstBytes)
	if err != nil || found {
		return r, err
	}
	r, _, err = readTurnTail(path, turnTailBytes)
	return r, err
}

// readTurnTail 在对话记录最后 window 字节里找最后一回合。found 表示找到了最近一次有人说话（或已读到文件开头）。
func readTurnTail(path string, window int64) (r *turnResult, found bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	start := max(st.Size()-window, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false, err
	}
	found = start == 0
	if start > 0 { // 丢掉开头不完整的一行
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	lines := bytes.Split(data, []byte{'\n'})

	r = &turnResult{}
	var texts []string // 倒序收集
	finalN := -1       // 最后一次调用工具之后的文字有几段（倒序时先收集到的那几段）
	inTurn, sawAssistant, usedReply, startKind := true, false, false, ""
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if len(line) == 0 || !(bytes.Contains(line, []byte(`"type":"assistant"`)) || bytes.Contains(line, []byte(`"type":"user"`))) {
			continue
		}
		var e transcriptEntry
		if json.Unmarshal(line, &e) != nil || e.IsSidechain || e.Message == nil {
			continue
		}
		if e.Type == "assistant" {
			if !inTurn {
				continue
			}
			if !sawAssistant {
				sawAssistant = true
				r.lastID = e.UUID
				r.complete = e.Message.StopReason == nil || *e.Message.StopReason != "tool_use"
			}
			var blocks []contentBlock
			if json.Unmarshal(e.Message.Content, &blocks) == nil {
				for j := len(blocks) - 1; j >= 0; j-- {
					switch blocks[j].Type {
					case "text":
						texts = append(texts, blocks[j].Text)
					case "tool_use":
						if finalN < 0 {
							finalN = len(texts)
						}
						if strings.HasSuffix(blocks[j].Name, "feishu__reply") {
							usedReply = true
						}
					}
				}
			}
			continue
		}
		if e.Type != "user" {
			continue
		}
		in := inputKind(&e)
		if in == nil {
			continue
		}
		if inTurn { // 这一回合从这条输入开始
			inTurn = false
			startKind = in.kind
			switch in.kind {
			case "feishu":
				r.replyTo = in.msgID
			case "typed":
				r.typed = in.text
			}
		}
		if in.kind == "feishu" {
			r.chatID = in.chatID
			found = true
			break
		}
		if in.kind == "typed" { // 最近一次是在电脑上打字：r📨 不转发，整回合同步到私聊
			r.replyTo = ""
			r.mirror = r.typed != "" || startKind == "background"
			found = true
			break
		}
		// 后台任务通知：继续往前找最近一次有人说话
	}
	if r.chatID == "" {
		r.replyTo = ""
	}
	if finalN < 0 {
		finalN = len(texts)
	}
	slices.Reverse(texts)
	if r.mirror {
		// 同步最后一次调用工具之后的文字（也就是最终回答）
		r.text = strings.TrimSpace(strings.Join(texts[len(texts)-finalN:], "\n\n"))
		return r, found, nil
	}
	r.text = replyText(texts, usedReply || r.replyTo == "")
	return r, found, nil
}

// replyText 取出这一回合要发到飞书的内容：每段文字里从"r📨:"那行开始到这段结尾。
// 飞书消息开始的回合里 Claude 忘了写 r📨、也没用 reply 工具时，退而发最后一段文字（去掉 📨 回显行），免得飞书上没人回。
// noFallback：用过 reply，或回合是后台任务通知开始的（如监听到期重启），只发明确写了 r📨 的内容。
func replyText(texts []string, noFallback bool) string {
	var parts []string
	for _, t := range texts {
		lines := strings.Split(t, "\n")
		for i, l := range lines {
			if rest, ok := cutReplyMark(l); ok {
				seg := strings.TrimSpace(strings.Join(append([]string{rest}, lines[i+1:]...), "\n"))
				if seg != "" {
					parts = append(parts, seg)
				}
				break
			}
		}
	}
	if len(parts) > 0 || noFallback {
		return strings.Join(parts, "\n\n")
	}
	for i := len(texts) - 1; i >= 0; i-- {
		var keep []string
		for _, l := range strings.Split(texts[i], "\n") {
			if s := strings.TrimSpace(l); strings.HasPrefix(s, "📨") {
				continue
			}
			keep = append(keep, l)
		}
		if p := strings.TrimSpace(strings.Join(keep, "\n")); p != "" {
			return p
		}
	}
	return ""
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

// onTurnEnd 处理 Stop hook：读对话记录，把这一回合的 r📨 段落发到飞书。
func (b *Bridge) onTurnEnd(path string) {
	if path == "" || b.conn == nil || !b.conn.Active() {
		return
	}
	var r *turnResult
	deadline := time.Now().Add(turnFlushWait)
	for {
		var err error
		r, err = readLastTurn(path)
		if err != nil {
			b.log.Debug("read transcript", "err", err)
			return
		}
		// 最后一条可能还没写进文件，稍等一下
		if r.complete || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	mirror := r.mirror && os.Getenv("FEISHU_MIRROR") != "off"
	if r.lastID == "" || (!mirror && (r.chatID == "" || r.text == "")) || (mirror && r.typed == "" && r.text == "") {
		return
	}
	b.mu.Lock()
	dup := r.lastID == b.lastTurnID
	if !dup {
		b.lastTurnID = r.lastID
	}
	b.mu.Unlock()
	if dup {
		return
	}
	if mirror {
		b.mirrorTyped(r)
		return
	}
	if err := b.checkChat(r.chatID); err != nil {
		b.log.Debug("turn end chat", "err", err)
		return
	}
	ids, err := b.client().SendMarkdown(context.Background(), r.chatID, r.replyTo, withTag(r.text))
	b.trackSent(ids...)
	if err != nil {
		b.log.Warn("send turn text", "err", err)
		return
	}
	b.clearAcks(r.chatID)
	b.log.Info("turn text sent to feishu", "chat", r.chatID, "chars", len(r.text))
}

// promptEcho 是提交时已经发到飞书的"💻 打的字"，回合结束时引用它发回答。
type promptEcho struct {
	text string
	done chan struct{}      // 发完后关闭
	msgs map[string]sentMsg // open_id → 发出的消息
}

type sentMsg struct{ msgID, chatID string }

const echoWait = 10 * time.Second

// onPromptSubmit 处理 UserPromptSubmit hook：电脑上打的字立刻私聊发到飞书，不等回合结束。
func (b *Bridge) onPromptSubmit(prompt string) {
	if b.conn == nil || !b.conn.Active() || os.Getenv("FEISHU_MIRROR") == "off" {
		return
	}
	if feishuEventRe.MatchString(prompt) || channelTagRe.MatchString(prompt) || strings.Contains(prompt, "<task-notification>") {
		return
	}
	text := typedText(prompt, 0)
	if text == "" {
		return
	}
	acc, err := b.access.Load()
	if err != nil || len(acc.AllowFrom) == 0 {
		return
	}
	e := &promptEcho{text: text, done: make(chan struct{}), msgs: map[string]sentMsg{}}
	b.mu.Lock()
	b.echo = e
	b.mu.Unlock()
	go func() {
		defer close(e.done)
		for _, openID := range acc.AllowFrom {
			msgID, chatID, err := b.client().SendMarkdownToUser(context.Background(), openID, withTag("💻 "+text))
			if err != nil {
				b.log.Warn("mirror typed input", "err", err)
				continue
			}
			b.trackSent(msgID)
			b.knowChat(chatID)
			b.mu.Lock()
			e.msgs[openID] = sentMsg{msgID, chatID}
			b.mu.Unlock()
		}
	}()
}

// takeEcho 取出和这一回合对应的、提交时已发出的消息；对不上（比如 hook 没装）返回 nil。
func (b *Bridge) takeEcho(typed string) map[string]sentMsg {
	b.mu.Lock()
	e := b.echo
	b.mu.Unlock()
	if e == nil || !strings.HasPrefix(typed, e.text) {
		return nil
	}
	select {
	case <-e.done:
	case <-time.After(echoWait):
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.echo == e {
		b.echo = nil
	}
	return maps.Clone(e.msgs)
}

// mirrorTyped 把电脑上打字的一回合同步到飞书：私聊白名单里的人，"💻 打的字"（提交时没发过就现在发），再引用它发最终回答。
// 后台任务通知开始的回合（没有打的字）只发最终回答。
func (b *Bridge) mirrorTyped(r *turnResult) {
	acc, err := b.access.Load()
	if err != nil || len(acc.AllowFrom) == 0 {
		return
	}
	var echoed map[string]sentMsg
	if r.typed != "" {
		echoed = b.takeEcho(r.typed)
	}
	ctx := context.Background()
	for _, openID := range acc.AllowFrom {
		if r.typed == "" {
			msgID, chatID, err := b.client().SendMarkdownToUser(ctx, openID, withTag(r.text))
			if err != nil {
				b.log.Warn("mirror turn text", "err", err)
				continue
			}
			b.trackSent(msgID)
			b.knowChat(chatID)
			continue
		}
		m, ok := echoed[openID]
		if !ok {
			msgID, chatID, err := b.client().SendMarkdownToUser(ctx, openID, withTag("💻 "+r.typed))
			if err != nil {
				b.log.Warn("mirror typed input", "err", err)
				continue
			}
			b.trackSent(msgID)
			b.knowChat(chatID)
			m = sentMsg{msgID, chatID}
		}
		if r.text == "" || m.chatID == "" {
			continue
		}
		ids, err := b.client().SendMarkdown(ctx, m.chatID, m.msgID, r.text)
		b.trackSent(ids...)
		if err != nil {
			b.log.Warn("mirror turn text", "err", err)
			continue
		}
		b.log.Info("typed turn mirrored to feishu", "chat", m.chatID, "chars", len(r.text))
	}
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

var contextOpenRe = regexp.MustCompile(`<([a-z][a-z0-9]*[-_][a-z0-9_-]*)(?:\s[^<>]*)?>`)

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
