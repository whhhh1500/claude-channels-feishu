// Package feishu 封装飞书开放平台 API，以及飞书消息内容和 markdown 之间的转换。
package feishu

// 飞书 API：发消息（post/卡片）、改消息、表情、上传下载。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/whhhh1500/claude-channels-feishu/internal/config"
)

const (
	// 每条消息的最大字符数。post/卡片请求体上限 30KB，中文 3 字节，留足余量。
	textChunkLimit = 4000
	// 每次飞书接口调用的硬超时。SDK 偶尔会卡住不返回，不能只靠 HTTP 超时。
	apiTimeout = 20 * time.Second
)

func tctx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, apiTimeout)
}

type Client struct {
	cli       *lark.Client
	log       *slog.Logger
	botMu     sync.RWMutex
	botOpenID string
	botName   string
}

func (f *Client) BotOpenID() string {
	f.botMu.RLock()
	defer f.botMu.RUnlock()
	return f.botOpenID
}

func (f *Client) BotName() string {
	f.botMu.RLock()
	defer f.botMu.RUnlock()
	return f.botName
}

// LogAdapter 把 SDK 日志转到 slog（也就是 stderr）。
type LogAdapter struct{ L *slog.Logger }

func (a LogAdapter) Debug(_ context.Context, args ...interface{}) { a.L.Debug(fmt.Sprint(args...)) }

func (a LogAdapter) Info(_ context.Context, args ...interface{}) { a.L.Info(fmt.Sprint(args...)) }

func (a LogAdapter) Warn(_ context.Context, args ...interface{}) { a.L.Warn(fmt.Sprint(args...)) }

func (a LogAdapter) Error(_ context.Context, args ...interface{}) { a.L.Error(fmt.Sprint(args...)) }

func DomainURL(domain string) string {
	if domain == "lark" {
		return lark.LarkBaseUrl
	}
	return lark.FeishuBaseUrl
}

func New(cfg *config.Config, log *slog.Logger) *Client {
	cli := lark.NewClient(cfg.AppID, cfg.AppSecret,
		lark.WithOpenBaseUrl(DomainURL(cfg.Domain)),
		lark.WithLogger(LogAdapter{L: log.With("component", "lark")}),
		lark.WithLogLevel(larkcore.LogLevelWarn),
		lark.WithReqTimeout(30*time.Second),
	)
	return &Client{cli: cli, log: log}
}

func apiErr(op string, err error, code int, msg string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return fmt.Errorf("%s: 飞书返回 code=%d msg=%s", op, code, msg)
}

// LoadBotInfo 获取机器人自己的 open_id，用来判断群里是否 @ 了它。
func (f *Client) LoadBotInfo(ctx context.Context) error {
	ctx, cancel := tctx(ctx)
	defer cancel()
	resp, err := f.cli.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return fmt.Errorf("获取机器人信息: %w", err)
	}
	var body struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID  string `json:"open_id"`
			AppName string `json:"app_name"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &body); err != nil {
		return fmt.Errorf("解析机器人信息: %w", err)
	}
	if body.Code != 0 {
		return apiErr("获取机器人信息", nil, body.Code, body.Msg)
	}
	f.botMu.Lock()
	f.botOpenID, f.botName = body.Bot.OpenID, body.Bot.AppName
	f.botMu.Unlock()
	return nil
}

func textContent(s string) string {
	b, _ := json.Marshal(map[string]string{"text": s})
	return string(b)
}

// send 发一条消息；replyTo 非空时用"回复"形式引用原消息。
func (f *Client) send(ctx context.Context, chatID, replyTo, msgType, content string) (string, error) {
	ctx, cancel := tctx(ctx)
	defer cancel()
	if replyTo != "" {
		// Reply API 只使用 message_id；必须先校验会话，不能绕过调用方的 chat_id 白名单。
		message, err := f.getMessage(ctx, replyTo)
		if err != nil {
			return "", err
		}
		if larkcore.StringValue(message.ChatId) != chatID {
			return "", errors.New("reply_to 不属于指定的 chat_id")
		}
		resp, err := f.cli.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().MessageId(replyTo).
			Body(larkim.NewReplyMessageReqBodyBuilder().MsgType(msgType).Content(content).Build()).Build())
		if err != nil || !resp.Success() {
			if err == nil {
				err = apiErr("回复消息", nil, resp.Code, resp.Msg)
			}
			// 原消息被撤回等情况：退回普通发送
			f.log.Warn("reply failed, falling back to create", "err", err)
		} else {
			return larkcore.StringValue(resp.Data.MessageId), nil
		}
	}
	resp, err := f.cli.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chatID).MsgType(msgType).Content(content).Build()).Build())
	if err != nil || !resp.Success() {
		if err != nil {
			return "", apiErr("发送消息", err, 0, "")
		}
		return "", apiErr("发送消息", nil, resp.Code, resp.Msg)
	}
	return larkcore.StringValue(resp.Data.MessageId), nil
}

// postMarkdown 生成富文本（post）消息体，内容用 md 标签渲染。
// 比卡片稳：卡片里超过 3 个表格会报错，post 没这个限制，而且可以编辑。
func postMarkdown(md string) string {
	b, _ := json.Marshal(map[string]any{
		"zh_cn": map[string]any{"content": [][]map[string]string{{{"tag": "md", "text": OptimizeMarkdown(md)}}}},
	})
	return string(b)
}

// SendMarkdown 发送 markdown，超长自动分多条。返回所有消息 ID。
func (f *Client) SendMarkdown(ctx context.Context, chatID, replyTo, md string) ([]string, error) {
	var ids []string
	for i, part := range SplitText(md, textChunkLimit) {
		rt := ""
		if i == 0 {
			rt = replyTo
		}
		id, err := f.send(ctx, chatID, rt, larkim.MsgTypePost, postMarkdown(part))
		if err != nil {
			return ids, fmt.Errorf("第 %d 段发送失败（已发 %d 段）: %w", i+1, len(ids), err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// SendCardToUser 按 open_id 私聊发一张卡片（审批卡片用）。
func (f *Client) SendCardToUser(ctx context.Context, openID, cardJSON string) (string, error) {
	ctx, cancel := tctx(ctx)
	defer cancel()
	resp, err := f.cli.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().ReceiveIdType("open_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(openID).MsgType(larkim.MsgTypeInteractive).Content(cardJSON).Build()).Build())
	if err != nil {
		return "", apiErr("发送卡片", err, 0, "")
	}
	if !resp.Success() {
		return "", apiErr("发送卡片", nil, resp.Code, resp.Msg)
	}
	return larkcore.StringValue(resp.Data.MessageId), nil
}

// SendMarkdownToUser 按 open_id 私聊发一条 markdown（超长截断）。返回消息 ID 和私聊会话 ID。
func (f *Client) SendMarkdownToUser(ctx context.Context, openID, md string) (msgID, chatID string, err error) {
	ctx, cancel := tctx(ctx)
	defer cancel()
	resp, err := f.cli.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().ReceiveIdType("open_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(openID).MsgType(larkim.MsgTypePost).
			Content(postMarkdown(Truncate(md, textChunkLimit))).Build()).Build())
	if err != nil {
		return "", "", apiErr("发送消息", err, 0, "")
	}
	if !resp.Success() {
		return "", "", apiErr("发送消息", nil, resp.Code, resp.Msg)
	}
	return larkcore.StringValue(resp.Data.MessageId), larkcore.StringValue(resp.Data.ChatId), nil
}

// SendCard 往会话里发一张卡片；replyTo 非空时引用回复那条消息。
func (f *Client) SendCard(ctx context.Context, chatID, replyTo, cardJSON string) (string, error) {
	return f.send(ctx, chatID, replyTo, larkim.MsgTypeInteractive, cardJSON)
}

func (f *Client) SendText(ctx context.Context, chatID, replyTo, text string) (string, error) {
	return f.send(ctx, chatID, replyTo, larkim.MsgTypeText, textContent(text))
}

// EditMarkdown 修改机器人之前发的 post 消息。
func (f *Client) EditMarkdown(ctx context.Context, messageID, md string) error {
	ctx, cancel := tctx(ctx)
	defer cancel()
	resp, err := f.cli.Im.V1.Message.Update(ctx, larkim.NewUpdateMessageReqBuilder().MessageId(messageID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().MsgType(larkim.MsgTypePost).Content(postMarkdown(Truncate(md, textChunkLimit))).Build()).Build())
	if err != nil {
		return apiErr("编辑消息", err, 0, "")
	}
	if !resp.Success() {
		return apiErr("编辑消息", nil, resp.Code, resp.Msg)
	}
	return nil
}

// PatchCard 更新一张已发出的卡片（只能改机器人自己发的卡片消息）。
func (f *Client) PatchCard(ctx context.Context, messageID, cardJSON string) error {
	ctx, cancel := tctx(ctx)
	defer cancel()
	resp, err := f.cli.Im.V1.Message.Patch(ctx, larkim.NewPatchMessageReqBuilder().MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().Content(cardJSON).Build()).Build())
	if err != nil {
		return apiErr("更新卡片", err, 0, "")
	}
	if !resp.Success() {
		return apiErr("更新卡片", nil, resp.Code, resp.Msg)
	}
	return nil
}

func (f *Client) AddReaction(ctx context.Context, messageID, emoji string) (string, error) {
	ctx, cancel := tctx(ctx)
	defer cancel()
	resp, err := f.cli.Im.V1.MessageReaction.Create(ctx, larkim.NewCreateMessageReactionReqBuilder().MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emoji).Build()).Build()).Build())
	if err != nil {
		return "", apiErr("添加表情", err, 0, "")
	}
	if !resp.Success() {
		return "", apiErr("添加表情", nil, resp.Code, resp.Msg)
	}
	return larkcore.StringValue(resp.Data.ReactionId), nil
}

func (f *Client) DeleteReaction(ctx context.Context, messageID, reactionID string) error {
	ctx, cancel := tctx(ctx)
	defer cancel()
	resp, err := f.cli.Im.V1.MessageReaction.Delete(ctx, larkim.NewDeleteMessageReactionReqBuilder().
		MessageId(messageID).ReactionId(reactionID).Build())
	if err != nil {
		return apiErr("删除表情", err, 0, "")
	}
	if !resp.Success() {
		return apiErr("删除表情", nil, resp.Code, resp.Msg)
	}
	return nil
}

// Download 下载消息里的图片/文件到 dir，返回本地路径。kind 是 image 或 file。
func (f *Client) Download(ctx context.Context, messageID, key, kind, dir, nameHint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resp, err := f.cli.Im.V1.MessageResource.Get(ctx, larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).FileKey(key).Type(kind).Build())
	if err != nil {
		return "", apiErr("下载附件", err, 0, "")
	}
	if !resp.Success() {
		return "", apiErr("下载附件", nil, resp.Code, resp.Msg)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := resp.FileName
	if name == "" {
		name = nameHint
	}
	if name == "" {
		name = key
		if kind == "image" {
			name += ".png"
		}
	}
	name = filepath.Base(safeFileName(name))
	out, err := os.CreateTemp(dir, "*-"+name)
	if err != nil {
		return "", err
	}
	path := out.Name()
	complete := false
	defer func() {
		out.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	if _, err := io.Copy(out, resp.File); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	complete = true
	return path, nil
}

func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*', '\r', '\n', ';', '[', ']':
			return '_'
		}
		return r
	}, s)
}

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".ico": true, ".tiff": true, ".heic": true}

// SendFile 上传本地文件并发到会话：图片按图片发，其他按文件发。
func (f *Client) SendFile(ctx context.Context, chatID, path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	ext := strings.ToLower(filepath.Ext(path))
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	upCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if imageExts[ext] && st.Size() <= 10<<20 {
		resp, err := f.cli.Im.V1.Image.Create(upCtx, larkim.NewCreateImageReqBuilder().
			Body(larkim.NewCreateImageReqBodyBuilder().ImageType("message").Image(file).Build()).Build())
		if err != nil || !resp.Success() {
			if err == nil {
				err = apiErr("上传图片", nil, resp.Code, resp.Msg)
			}
			return "", err
		}
		b, _ := json.Marshal(map[string]string{"image_key": larkcore.StringValue(resp.Data.ImageKey)})
		return f.send(ctx, chatID, "", larkim.MsgTypeImage, string(b))
	}
	if st.Size() > 30<<20 {
		return "", errors.New("文件超过 30MB，飞书不支持")
	}
	fileType := "stream"
	switch ext {
	case ".pdf":
		fileType = "pdf"
	case ".doc", ".docx":
		fileType = "doc"
	case ".xls", ".xlsx":
		fileType = "xls"
	case ".ppt", ".pptx":
		fileType = "ppt"
	case ".mp4":
		fileType = "mp4"
	case ".opus":
		fileType = "opus"
	}
	resp, err := f.cli.Im.V1.File.Create(upCtx, larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().FileType(fileType).FileName(filepath.Base(path)).File(file).Build()).Build())
	if err != nil || !resp.Success() {
		if err == nil {
			err = apiErr("上传文件", nil, resp.Code, resp.Msg)
		}
		return "", err
	}
	b, _ := json.Marshal(map[string]string{"file_key": larkcore.StringValue(resp.Data.FileKey)})
	return f.send(ctx, chatID, "", larkim.MsgTypeFile, string(b))
}

// GetMessageText 读取一条消息的纯文本（用于引用回复时带上被引用内容）。
func (f *Client) GetMessageText(ctx context.Context, messageID string) (string, error) {
	m, err := f.getMessage(ctx, messageID)
	if err != nil {
		return "", err
	}
	if m.Body == nil {
		return "", errors.New("消息不存在")
	}
	text, _ := RenderContent(larkcore.StringValue(m.MsgType), larkcore.StringValue(m.Body.Content))
	return text, nil
}

func (f *Client) getMessage(ctx context.Context, messageID string) (*larkim.Message, error) {
	ctx, cancel := tctx(ctx)
	defer cancel()
	resp, err := f.cli.Im.V1.Message.Get(ctx, larkim.NewGetMessageReqBuilder().MessageId(messageID).Build())
	if err != nil {
		return nil, apiErr("读取消息", err, 0, "")
	}
	if !resp.Success() {
		return nil, apiErr("读取消息", nil, resp.Code, resp.Msg)
	}
	if resp.Data == nil || len(resp.Data.Items) == 0 || resp.Data.Items[0] == nil {
		return nil, errors.New("消息不存在")
	}
	return resp.Data.Items[0], nil
}
