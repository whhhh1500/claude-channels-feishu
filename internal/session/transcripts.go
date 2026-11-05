package session

// 从 Claude Code 的对话记录（~/.claude/projects/<项目>/<会话ID>.jsonl）里找出调用过飞书 connect 工具的会话，
// 用来补全连接历史功能上线之前的记录。只读本机文件，结果按文件修改时间缓存。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxTranscriptBytes = 64 << 20 // 太大的记录文件跳过

// 飞书 connect 工具的名字：手动注册时是 mcp__feishu__connect，插件里是 mcp__plugin_feishu_feishu__connect。
var connectToolRe = regexp.MustCompile(`^mcp__[A-Za-z0-9_]*feishu__connect$`)

// ProjectsDir 返回 Claude Code 存放对话记录的目录。
func ProjectsDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

type scanCacheEntry struct {
	off   int64        // 已扫描到的位置（最后一个完整行的末尾）；记录文件只会追加，下次从这里接着读
	entry HistoryEntry // Connects 为 0 表示还没找到连接记录
}

var (
	scanMu    sync.Mutex
	scanCache = map[string]*scanCacheEntry{}
)

// ScanTranscripts 找出 since 之后修改过、并且调用过飞书 connect 工具的会话。
// 每个文件只读新增的部分，面板定时刷新时不会反复读整个大文件。
func ScanTranscripts(projectsDir string, since time.Time) []HistoryEntry {
	if projectsDir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(projectsDir, "*", "*.jsonl"))
	scanMu.Lock()
	defer scanMu.Unlock()
	var out []HistoryEntry
	for _, path := range files {
		st, err := os.Stat(path)
		if err != nil || st.ModTime().Before(since) {
			continue
		}
		c := scanCache[path]
		if c == nil || st.Size() < c.off { // 新文件，或文件被重写过：从头读
			if st.Size() > maxTranscriptBytes {
				continue
			}
			c = &scanCacheEntry{entry: HistoryEntry{
				ClaudeSession: strings.TrimSuffix(filepath.Base(path), ".jsonl"),
				Source:        "transcript",
			}}
			scanCache[path] = c
		}
		if st.Size() > c.off {
			c.off = scanTranscript(path, c.off, &c.entry)
		}
		if c.entry.Connects > 0 {
			out = append(out, c.entry)
		}
	}
	sortEntries(out)
	return out
}

type transcriptLine struct {
	Type       string    `json:"type"`
	Timestamp  time.Time `json:"timestamp"`
	Cwd        string    `json:"cwd"`
	SessionID  string    `json:"sessionId"`
	Entrypoint string    `json:"entrypoint"`
	Message    *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// scanTranscript 从 off 处接着读对话记录，只认真正的 tool_use 调用（工具清单里出现名字不算）。
// 只处理完整的行（正在写的最后一行留到下次），返回新的读取位置。
func scanTranscript(path string, off int64, e *HistoryEntry) int64 {
	f, err := os.Open(path)
	if err != nil {
		return off
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off
	}
	rd := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil { // 没读到换行：文件结尾或还没写完的行
			return off
		}
		off += int64(len(line))
		scanLine(line, e)
	}
}

func scanLine(line []byte, e *HistoryEntry) {
	// 先做便宜的字节检查，绝大多数行在这里就跳过了
	fill := e.Cwd == "" || e.Host == ""
	hasConnect := bytes.Contains(line, []byte(`feishu__connect"`)) && bytes.Contains(line, []byte(`"tool_use"`))
	if !hasConnect && !fill {
		return
	}
	var tl transcriptLine
	if json.Unmarshal(line, &tl) != nil {
		return
	}
	if e.Cwd == "" && tl.Cwd != "" {
		e.Cwd = tl.Cwd
	}
	if e.Host == "" && tl.Entrypoint != "" {
		e.Host = hostFromEntrypoint(tl.Entrypoint)
	}
	if !hasConnect || tl.Type != "assistant" || tl.Message == nil {
		return
	}
	var items []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(tl.Message.Content, &items) != nil {
		return
	}
	for _, it := range items {
		if it.Type == "tool_use" && connectToolRe.MatchString(it.Name) {
			if tl.Cwd != "" { // 会话中途可能换过目录，以连接那一刻的目录为准
				e.Cwd = tl.Cwd
			}
			e.Connects++
			if e.FirstConnected.IsZero() || tl.Timestamp.Before(e.FirstConnected) {
				e.FirstConnected = tl.Timestamp
			}
			if tl.Timestamp.After(e.LastConnected) {
				e.LastConnected = tl.Timestamp
			}
		}
	}
}

func hostFromEntrypoint(ep string) string {
	switch {
	case ep == "claude-desktop":
		return "desktop"
	case ep == "cli":
		return "cli"
	case strings.HasPrefix(ep, "sdk"):
		return "sdk"
	}
	return ep
}
