package feishu

// 把飞书收到的各类消息内容转成给 Claude 看的纯文本 / markdown。

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Attachment struct {
	Kind string // image / file / audio / media / sticker
	Key  string
	Name string
}

// RenderContent 解析消息 content JSON，返回文本和附件列表。
func RenderContent(msgType, content string) (string, []Attachment) {
	switch msgType {
	case "text":
		var c struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(content), &c)
		return c.Text, nil
	case "post":
		return RenderPost(content)
	case "image":
		var c struct {
			ImageKey string `json:"image_key"`
		}
		_ = json.Unmarshal([]byte(content), &c)
		return "(图片)", []Attachment{{Kind: "image", Key: c.ImageKey}}
	case "file":
		var c struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		_ = json.Unmarshal([]byte(content), &c)
		return fmt.Sprintf("(文件: %s)", c.FileName), []Attachment{{Kind: "file", Key: c.FileKey, Name: c.FileName}}
	case "audio":
		var c struct {
			FileKey string `json:"file_key"`
		}
		_ = json.Unmarshal([]byte(content), &c)
		return "(语音消息)", []Attachment{{Kind: "audio", Key: c.FileKey}}
	case "media":
		var c struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		_ = json.Unmarshal([]byte(content), &c)
		return "(视频)", []Attachment{{Kind: "media", Key: c.FileKey, Name: c.FileName}}
	case "sticker":
		return "(表情包)", nil
	case "interactive":
		return "(卡片消息)", nil
	case "merge_forward":
		return "(合并转发的聊天记录)", nil
	case "share_chat":
		return "(分享的群名片)", nil
	case "share_user":
		return "(分享的个人名片)", nil
	case "location":
		var c struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal([]byte(content), &c)
		return "(位置: " + c.Name + ")", nil
	default:
		return fmt.Sprintf("(%s 类型消息)", msgType), nil
	}
}

type postElem struct {
	Tag      string   `json:"tag"`
	Text     string   `json:"text"`
	Href     string   `json:"href"`
	UserID   string   `json:"user_id"`
	UserName string   `json:"user_name"`
	ImageKey string   `json:"image_key"`
	FileKey  string   `json:"file_key"`
	Language string   `json:"language"`
	Style    []string `json:"style"`
}

type postBody struct {
	Title   string       `json:"title"`
	Content [][]postElem `json:"content"`
}

func RenderPost(content string) (string, []Attachment) {
	var body postBody
	// 收到的 post 可能是扁平结构，也可能套一层语言 {"zh_cn": {...}}
	if err := json.Unmarshal([]byte(content), &body); err != nil || body.Content == nil {
		var wrapped map[string]postBody
		if json.Unmarshal([]byte(content), &wrapped) == nil {
			for _, lang := range []string{"zh_cn", "en_us", "ja_jp"} {
				if b, ok := wrapped[lang]; ok {
					body = b
					break
				}
			}
			if body.Content == nil {
				for _, b := range wrapped {
					body = b
					break
				}
			}
		}
	}
	var sb strings.Builder
	var atts []Attachment
	if body.Title != "" {
		sb.WriteString("**" + body.Title + "**\n\n")
	}
	for i, para := range body.Content {
		if i > 0 {
			sb.WriteString("\n")
		}
		for _, e := range para {
			switch e.Tag {
			case "text":
				sb.WriteString(styled(e.Text, e.Style))
			case "md":
				sb.WriteString(e.Text)
			case "a":
				fmt.Fprintf(&sb, "[%s](%s)", e.Text, e.Href)
			case "at":
				if e.UserName != "" {
					sb.WriteString("@" + e.UserName)
				} else {
					sb.WriteString(e.UserID) // @_user_N 占位符，后面统一处理
				}
			case "img":
				sb.WriteString("(图片)")
				atts = append(atts, Attachment{Kind: "image", Key: e.ImageKey})
			case "media":
				sb.WriteString("(视频)")
				atts = append(atts, Attachment{Kind: "media", Key: e.FileKey})
			case "code_block":
				fmt.Fprintf(&sb, "\n```%s\n%s\n```\n", strings.ToLower(e.Language), strings.TrimRight(e.Text, "\n"))
			case "hr":
				sb.WriteString("\n---\n")
			case "emotion":
				sb.WriteString(e.Text)
			default:
				sb.WriteString(e.Text)
			}
		}
	}
	return strings.TrimSpace(sb.String()), atts
}

func styled(s string, style []string) string {
	for _, st := range style {
		switch st {
		case "bold":
			s = "**" + s + "**"
		case "italic":
			s = "*" + s + "*"
		case "lineThrough":
			s = "~~" + s + "~~"
		}
	}
	return s
}
