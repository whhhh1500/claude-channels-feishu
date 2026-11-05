package panel

// 面板的"连接历史"：本程序记录的连接历史 + 从 Claude 对话记录里找到的旧会话，再标上每个会话现在的状态。

import (
	"runtime"
	"strings"
	"time"

	"github.com/whhhh1500/claude-channels-feishu/internal/session"
)

// 对话记录只往回找这么久，免得每次都读全部历史文件。
const transcriptLookback = 30 * 24 * time.Hour

type historyRow struct {
	session.HistoryEntry
	State     string `json:"state"`         // online：正连着飞书；open：会话还开着；gone：会话已退出
	PID       int    `json:"pid,omitempty"` // 会话还开着时，飞书服务的进程号（面板开关用）
	ResumeCmd string `json:"resumeCmd,omitempty"`
}

func buildHistory(stateDir string) ([]historyRow, error) {
	events, err := session.ReadHistory(stateDir)
	if err != nil {
		return nil, err
	}
	entries := session.MergeHistory(
		session.SummarizeHistory(events),
		session.ScanTranscripts(session.ProjectsDir(), time.Now().Add(-transcriptLookback)),
	)

	live := map[string]session.Status{}
	for _, st := range session.List(stateDir) {
		if st.ClaudeSession == "" {
			continue
		}
		// 同一个会话 ID 有多个进程时（同一会话开了两个终端），优先取连着飞书的那个
		if old, ok := live[st.ClaudeSession]; !ok || (st.Active && !old.Active) {
			live[st.ClaudeSession] = st
		}
	}

	rows := make([]historyRow, 0, len(entries))
	for _, e := range entries {
		row := historyRow{HistoryEntry: e, State: "gone"}
		if st, ok := live[e.ClaudeSession]; ok && e.ClaudeSession != "" {
			row.PID = st.PID
			row.State = "open"
			if st.Active {
				row.State = "online"
			}
		}
		if row.State == "gone" {
			row.ResumeCmd = resumeCommand(e.Cwd, e.ClaudeSession)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// resumeCommand 生成在终端里恢复这个会话的命令（Windows 上是 PowerShell 写法）。
func resumeCommand(cwd, id string) string {
	if id == "" {
		return ""
	}
	cmd := "claude --resume " + id
	if cwd == "" {
		return cmd
	}
	if runtime.GOOS == "windows" {
		return "cd '" + strings.ReplaceAll(cwd, "'", "''") + "'; " + cmd
	}
	return "cd '" + strings.ReplaceAll(cwd, "'", `'\''`) + "' && " + cmd
}
