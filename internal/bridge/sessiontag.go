package bridge

// 会话标识：飞书上可能同时有多个 Claude 会话，回复和卡片都带上"名字#会话ID前8位#…当前目录最后三级"，一眼分清是谁。
// 名字取用户给会话起的名字（对话记录里的 custom-title），没起名时用启动目录名；当前目录取对话记录里最新的 cwd。
// 设 FEISHU_SESSION_TAG=off 关闭。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/whhhh1500/claude-channels-feishu/internal/feishu"
	"github.com/whhhh1500/claude-channels-feishu/internal/session"
)

const tagRefresh = 3 * time.Second // 最多每 3 秒看一次对话记录有没有新内容

// transcriptState 增量读本会话的对话记录，记下最新的会话名和工作目录。
type transcriptState struct {
	mu      sync.Mutex
	id      string
	path    string
	off     int64 // 已读到的位置（只算完整的行）
	title   string
	cwd     string
	checked time.Time
}

var transcript = &transcriptState{id: os.Getenv("CLAUDE_CODE_SESSION_ID")}

// sessionInfo 返回会话名（没起名时为空）和当前目录（读不到时为空）。
func (t *transcriptState) sessionInfo() (title, cwd string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id != "" && time.Since(t.checked) >= tagRefresh {
		t.checked = time.Now()
		t.refresh()
	}
	return t.title, t.cwd
}

func (t *transcriptState) refresh() {
	if t.path == "" {
		matches, _ := filepath.Glob(filepath.Join(session.ProjectsDir(), "*", t.id+".jsonl"))
		if len(matches) == 0 {
			return
		}
		t.path = matches[0]
	}
	f, err := os.Open(t.path)
	if err != nil {
		t.path = ""
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() == t.off {
		return
	} else if st.Size() < t.off { // 文件被重写过，从头读
		t.off = 0
	}
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return
	}
	rd := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil { // 最后一行可能还没写完，下次再读
			return
		}
		t.off += int64(len(line))
		t.scan(line)
	}
}

var cwdRe = regexp.MustCompile(`"cwd":("(?:[^"\\]|\\.)*")`)

func (t *transcriptState) scan(line []byte) {
	if bytes.Contains(line, []byte(`"type":"custom-title"`)) {
		var v struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
		}
		if json.Unmarshal(line, &v) == nil && v.Type == "custom-title" {
			t.title = strings.TrimSpace(v.CustomTitle)
		}
		return
	}
	// 只取顶层的 "cwd"（工具参数里的会被转义成 \"cwd\"，匹配不上）
	if m := cwdRe.FindSubmatch(line); m != nil {
		var s string
		if json.Unmarshal(m[1], &s) == nil && s != "" {
			t.cwd = s
		}
	}
}

func sessionTag() string {
	if os.Getenv("FEISHU_SESSION_TAG") == "off" {
		return ""
	}
	start, _ := os.Getwd()
	title, cwd := transcript.sessionInfo()
	if cwd == "" {
		cwd = start
	}
	return makeTag(title, start, transcript.id, cwd)
}

// makeTag 生成"名字#会话ID前8位#…最后三级目录"。没起名时用启动目录名。
func makeTag(title, startDir, sessionID, cwd string) string {
	name := title
	if name == "" && startDir != "" {
		name = filepath.Base(startDir)
	}
	id := sessionID
	if len(id) > 8 {
		id = id[:8]
	}
	var parts []string
	for _, p := range []string{name, id, shortPath(cwd)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "#")
}

var msysDrive = regexp.MustCompile(`^/([a-zA-Z])(/|$)`)

// shortPath 只留最后三级目录，前面有省略时加"..."：D:\work\project\acme\shop\api → ...acme\shop\api
func shortPath(p string) string {
	if p == "" {
		return ""
	}
	sep := "/"
	if runtime.GOOS == "windows" {
		if m := msysDrive.FindStringSubmatch(p); m != nil { // Git Bash 风格的 /d/work → D:\work
			p = strings.ToUpper(m[1]) + ":\\" + p[len(m[0]):]
		}
		p = strings.ReplaceAll(p, "/", `\`)
		sep = `\`
	}
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) <= 3 {
		return p
	}
	return "..." + strings.Join(parts[len(parts)-3:], sep)
}

// withTag 给回复正文加一行会话标识。
func withTag(text string) string {
	t := sessionTag()
	if t == "" {
		return text
	}
	// post 消息的 md 不支持字体颜色，用全角括号，纯文本也好认。
	// 飞书 md 不把 \\ 还原成 \，路径里的反斜杠原样发（实测）。
	return "【" + feishu.MDEscape(t) + "】\n" + text
}

func titleWithTag(title string) string {
	if t := sessionTag(); t != "" {
		return title + " · " + t
	}
	return title
}
