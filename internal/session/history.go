package session

// 连接历史：每次连接、断开（手动 / 被接管 / 会话退出）都往 <状态目录>/history.jsonl 追加一行，
// 网页面板据此列出"连过飞书的会话"，即使那些会话早已退出。

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
)

const (
	historyFile     = "history.jsonl"
	historyMaxBytes = 2 << 20 // 超过 2MB 时只保留后一半
)

// HistoryEvent 是 history.jsonl 里的一行。
type HistoryEvent struct {
	Time          time.Time `json:"time"`
	Event         string    `json:"event"`            // connect / disconnect
	Reason        string    `json:"reason,omitempty"` // disconnect 的原因：manual / takeover / exit
	ClaudeSession string    `json:"claudeSession,omitempty"`
	Cwd           string    `json:"cwd,omitempty"`
	Host          string    `json:"host,omitempty"`
	Mode          string    `json:"mode,omitempty"` // channel / monitor
	PID           int       `json:"pid,omitempty"`
	Messages      int       `json:"messages,omitempty"` // 本次连接期间转给 Claude 的飞书消息数（disconnect 时记录）
}

// HistoryEntry 是按会话汇总后的一条历史。
type HistoryEntry struct {
	ClaudeSession    string    `json:"claudeSession"`
	Cwd              string    `json:"cwd"`
	Host             string    `json:"host"`
	FirstConnected   time.Time `json:"firstConnected"`
	LastConnected    time.Time `json:"lastConnected"`
	LastDisconnected time.Time `json:"lastDisconnected,omitempty"`
	LastReason       string    `json:"lastReason,omitempty"`
	Connects         int       `json:"connects"`
	Messages         int       `json:"messages"`
	Source           string    `json:"source"` // log：本程序记录的；transcript：从 Claude 对话记录里找到的
}

var historyMu sync.Mutex

// AppendHistory 追加一条连接历史。失败只影响面板显示，不影响收发，所以调用方可以忽略错误。
func AppendHistory(stateDir string, ev HistoryEvent) error {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(stateDir, historyFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(raw, '\n'))
	st, serr := f.Stat()
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	if serr == nil && st.Size() > historyMaxBytes {
		trimHistory(path)
	}
	return nil
}

// trimHistory 只保留后一半的行。
func trimHistory(path string) {
	lines, err := readLines(path)
	if err != nil || len(lines) < 2 {
		return
	}
	lines = lines[len(lines)/2:]
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for _, l := range lines {
		_, _ = w.WriteString(l + "\n")
	}
	if w.Flush() != nil || f.Close() != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, path)
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if l := sc.Text(); l != "" {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// ReadHistory 读取全部连接历史事件（按时间顺序）。文件不存在时返回空。
func ReadHistory(stateDir string) ([]HistoryEvent, error) {
	historyMu.Lock()
	defer historyMu.Unlock()
	lines, err := readLines(filepath.Join(stateDir, historyFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]HistoryEvent, 0, len(lines))
	for _, l := range lines {
		var ev HistoryEvent
		if json.Unmarshal([]byte(l), &ev) == nil && ev.Event != "" {
			out = append(out, ev)
		}
	}
	return out, nil
}

// historyKey 区分会话：有 Claude 会话 ID 用它，没有（旧版本）就用进程号。
func historyKey(ev HistoryEvent) string {
	if ev.ClaudeSession != "" {
		return ev.ClaudeSession
	}
	return "pid:" + strconv.Itoa(ev.PID)
}

// SummarizeHistory 按会话汇总，最近连过的排在前面。
func SummarizeHistory(events []HistoryEvent) []HistoryEntry {
	byKey := map[string]*HistoryEntry{}
	for _, ev := range events {
		k := historyKey(ev)
		e := byKey[k]
		if e == nil {
			e = &HistoryEntry{ClaudeSession: ev.ClaudeSession, Source: "log"}
			byKey[k] = e
		}
		if ev.Cwd != "" {
			e.Cwd = ev.Cwd
		}
		if ev.Host != "" {
			e.Host = ev.Host
		}
		switch ev.Event {
		case "connect":
			e.Connects++
			if e.FirstConnected.IsZero() || ev.Time.Before(e.FirstConnected) {
				e.FirstConnected = ev.Time
			}
			if ev.Time.After(e.LastConnected) {
				e.LastConnected = ev.Time
			}
		case "disconnect":
			e.Messages += ev.Messages
			if ev.Time.After(e.LastDisconnected) {
				e.LastDisconnected, e.LastReason = ev.Time, ev.Reason
			}
		}
	}
	out := make([]HistoryEntry, 0, len(byKey))
	for _, e := range byKey {
		if e.Connects > 0 {
			out = append(out, *e)
		}
	}
	sortEntries(out)
	return out
}

func sortEntries(es []HistoryEntry) {
	slices.SortFunc(es, func(a, b HistoryEntry) int { return b.LastConnected.Compare(a.LastConnected) })
}

// MergeHistory 合并本程序的记录和从对话记录里找到的会话；同一会话以本程序记录为准，只补全缺的字段。
func MergeHistory(logged, found []HistoryEntry) []HistoryEntry {
	idx := map[string]int{}
	out := slices.Clone(logged)
	for i, e := range out {
		if e.ClaudeSession != "" {
			idx[e.ClaudeSession] = i
		}
	}
	for _, f := range found {
		i, ok := idx[f.ClaudeSession]
		if !ok {
			out = append(out, f)
			continue
		}
		e := &out[i]
		if e.Cwd == "" {
			e.Cwd = f.Cwd
		}
		if e.Host == "" {
			e.Host = f.Host
		}
		if f.FirstConnected.Before(e.FirstConnected) {
			e.FirstConnected = f.FirstConnected
		}
	}
	sortEntries(out)
	return out
}
