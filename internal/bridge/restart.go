package bridge

// 重启插件进程（不重开会话）：插件只负责关闭自己（shutdown 工具）；Claude 随后调用任一插件工具时，
// Claude Code 发现进程没了就自动用磁盘上最新的程序重新拉起（实测），新进程连上后通知它刷新工具列表。
// 关闭前连着飞书时，记下连接方式和监听地址；新进程启动时看到这笔记录就自动连回飞书，监听地址不变，
// 原来的 Monitor 参数照样能用。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/whhhh1500/claude-channels-feishu/internal/mcp"
)

const restartMarkTTL = 10 * time.Minute // 记录超过这么久就不算这次重启的了

// restartMark 是关闭插件时留给新进程的记录。
type restartMark struct {
	Mode   string    `json:"mode"`   // channel / monitor
	Listen string    `json:"listen"` // Monitor 的 ws 地址（带口令），新进程尽量沿用
	At     time.Time `json:"at"`
}

func restartMarkPath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, "restart-"+sessionID+".json")
}

// toolShutdown 关闭本会话的插件进程。返回后不到 1 秒退出。
func (b *Bridge) toolShutdown() mcp.Result {
	b.mu.Lock()
	mode, exit := b.mode, b.onExit
	b.mu.Unlock()
	if exit == nil {
		return mcp.Error(fmt.Errorf("这个插件进程不能关闭"))
	}
	wasActive := b.conn.Active()
	if wasActive && transcript.id != "" {
		raw, _ := json.Marshal(restartMark{Mode: mode, Listen: b.listenURL(), At: time.Now()})
		if err := os.WriteFile(restartMarkPath(b.config().StateDir, transcript.id), raw, 0o600); err != nil {
			b.log.Warn("write restart mark", "err", err)
		}
	}
	if wasActive {
		b.conn.DisconnectFor("restart")
	}
	go func() {
		time.Sleep(500 * time.Millisecond) // 先把这次工具结果送出去
		exit()
	}()
	msg := "飞书插件马上关闭。接着调用一次飞书的 status 工具：Claude Code 会用磁盘上最新的程序重新拉起插件，并刷新工具列表。" +
		"如果 status 调用失败，请用户输入 /mcp 重连 plugin:feishu:feishu。"
	if wasActive && mode == "monitor" {
		msg += "\n关闭前连着飞书：新插件起来后自动连回飞书，监听地址不变。原来的飞书监听会断开，不用处理它的断开通知；" +
			"调完 status 后用原来的参数重新启动 Monitor，中间的飞书消息会补发。"
	} else if wasActive {
		msg += "\n关闭前连着飞书：新插件起来后自动连回飞书。"
	}
	return mcp.Text("%s", msg)
}

// SetOnExit 设置 shutdown 工具关闭进程时调用的函数（由主程序提供：关控制端口、退出）。
func (b *Bridge) SetOnExit(f func()) {
	b.mu.Lock()
	b.onExit = f
	b.mu.Unlock()
}

// takeRestartMark 读出并删掉本会话上次关闭插件时留下的记录（过期的不算）。
func (b *Bridge) takeRestartMark() *restartMark {
	if transcript.id == "" {
		return nil
	}
	p := restartMarkPath(b.config().StateDir, transcript.id)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	_ = os.Remove(p)
	var m restartMark
	if json.Unmarshal(raw, &m) != nil || time.Since(m.At) > restartMarkTTL {
		return nil
	}
	return &m
}

// ResumeAfterRestart 插件被 Claude 重新拉起后，按关闭前的记录连回飞书。没有记录时什么都不做。
// 要在 StartControl 之前调用记录读取（监听地址要沿用），所以分成 LoadRestart 和这里两步。
func (b *Bridge) ResumeAfterRestart(ctx context.Context) {
	b.mu.Lock()
	m := b.restart
	b.mu.Unlock()
	if m == nil {
		return
	}
	if _, err := b.conn.Connect(ctx); err != nil && !b.conn.Active() {
		b.log.Warn("reconnect after restart", "err", err)
		return
	}
	b.setMode(m.Mode)
	b.recordConnect(m.Mode)
	b.log.Info("reconnected after plugin restart", "mode", m.Mode)
}

// LoadRestart 读出重启记录，供监听地址沿用和随后的 ResumeAfterRestart。
func (b *Bridge) LoadRestart() {
	m := b.takeRestartMark()
	b.mu.Lock()
	b.restart = m
	b.mu.Unlock()
}

// restartListen 返回要沿用的监听地址（没有时为空）。
func (b *Bridge) restartListen() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.restart == nil {
		return ""
	}
	return b.restart.Listen
}
