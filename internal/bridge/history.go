package bridge

// 连接历史：连接、断开时各记一条，网页面板据此列出连过飞书的会话。

import (
	"os"
	"sync/atomic"

	"github.com/whhhh1500/claude-channels-feishu/internal/session"
)

// msgCounter 统计本次连接期间转给 Claude 的飞书消息数。
type msgCounter struct{ n atomic.Int64 }

func (b *Bridge) historyBase(event string) session.HistoryEvent {
	cwd, _ := os.Getwd()
	return session.HistoryEvent{
		Event:         event,
		ClaudeSession: os.Getenv("CLAUDE_CODE_SESSION_ID"),
		Cwd:           cwd,
		Host:          b.Host(),
		PID:           os.Getpid(),
	}
}

func (b *Bridge) recordConnect(mode string) {
	b.msgs.n.Store(0)
	ev := b.historyBase("connect")
	ev.Mode = mode
	if err := session.AppendHistory(b.config().StateDir, ev); err != nil {
		b.log.Debug("record connect", "err", err)
	}
}

func (b *Bridge) recordDisconnect(reason string) {
	ev := b.historyBase("disconnect")
	ev.Reason = reason
	ev.Messages = int(b.msgs.n.Swap(0))
	if err := session.AppendHistory(b.config().StateDir, ev); err != nil {
		b.log.Debug("record disconnect", "err", err)
	}
}
