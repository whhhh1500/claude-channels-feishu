package bridge

// Monitor 投递：没带 channel 参数时，飞书消息交给会话里的监听进程（feishu-channel listen）打印出来，
// Claude Code 把打印内容作为通知交给 Claude。监听没连上时消息先暂存。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/whhhh1500/claude-channels-feishu/internal/hostinfo"
)

// ---------------------------------------------------------------- Monitor 投递

type monitorHub struct {
	mu      sync.Mutex
	subs    map[chan string]struct{}
	order   []chan string   // 订阅顺序；只投递给最新的一个，同一会话里有两个监听时不会收到两遍
	queue   bool            // 走 Monitor 模式时打开：没有订阅方时先暂存
	backlog []string        // 暂存的消息，订阅方连上后补发
	waiters []chan struct{} // 空闲时的后台等待命令，有暂存消息或断开时关闭
}

const maxBacklog = 200

func (h *monitorHub) add() chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[chan string]struct{}{}
	}
	ch := make(chan string, 64+len(h.backlog))
	for _, l := range h.backlog { // 监听间隙里收到的消息补发
		ch <- l
	}
	h.backlog = nil
	h.subs[ch] = struct{}{}
	h.order = append(h.order, ch)
	return ch
}

func (h *monitorHub) remove(ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ch)
	for i, c := range h.order {
		if c == ch {
			h.order = append(h.order[:i], h.order[i+1:]...)
			break
		}
	}
}

func (h *monitorHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *monitorHub) setQueue(on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queue = on
	if !on {
		h.backlog = nil
		h.wake() // 断开飞书：后台等待命令不用再等
	}
}

// wake 叫醒所有等待暂存消息的后台等待命令（调用方持有 h.mu）。
func (h *monitorHub) wake() {
	for _, w := range h.waiters {
		close(w)
	}
	h.waiters = nil
}

// waitBacklog 给空闲时的后台等待命令（feishu-channel listen --wait）用：
// 阻塞到有暂存消息（返回 true），或飞书已断开、不再暂存（返回 false），或 done 关闭（对方退出）。
// 期间不占 CPU，也不调用模型；有消息时等待命令结束，Claude 再启动 Monitor 监听，暂存的消息马上补发。
func (h *monitorHub) waitBacklog(done <-chan struct{}) bool {
	for {
		h.mu.Lock()
		if len(h.backlog) > 0 {
			h.mu.Unlock()
			return true
		}
		if !h.queue {
			h.mu.Unlock()
			return false
		}
		ch := make(chan struct{})
		h.waiters = append(h.waiters, ch)
		h.mu.Unlock()
		select {
		case <-ch:
		case <-done:
			return false
		}
	}
}

// publish 发给所有订阅方；没有订阅方且处于 Monitor 模式时暂存。返回 true 表示已发出或已暂存。
func (h *monitorHub) publish(line string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	// 从最新的订阅方往前找第一个能收的
	for i := len(h.order) - 1; i >= 0; i-- {
		select {
		case h.order[i] <- line:
			return true
		default: // 这个订阅方卡住了，换前一个
		}
	}
	if h.queue {
		if len(h.backlog) >= maxBacklog {
			h.backlog = h.backlog[1:]
		}
		h.backlog = append(h.backlog, line)
		h.wake()
		return true
	}
	return false
}

// useMonitor 决定消息走 Monitor 还是 Channel：连接时选定的方式优先；
// 没选定时，带了 channel 参数走 Channel，否则有 Monitor 就走 Monitor。
func (b *Bridge) useMonitor() bool {
	b.mu.Lock()
	mode := b.mode
	b.mu.Unlock()
	switch mode {
	case "monitor":
		return true
	case "channel":
		return false
	}
	if b.channelFlag() == hostinfo.FlagPresent {
		return false
	}
	return b.monitors.count() > 0
}

func (b *Bridge) setMode(m string) {
	b.mu.Lock()
	b.mode = m
	b.mu.Unlock()
	b.monitors.setQueue(m == "monitor")
}

// deliver 把一条飞书消息交给 Claude。
func (b *Bridge) deliver(text string, meta map[string]string) {
	b.msgs.n.Add(1)
	if b.useMonitor() {
		m := map[string]string{"text": text}
		for k, v := range meta {
			m[k] = v
		}
		raw, _ := json.Marshal(m)
		if b.monitors.publish("飞书消息 " + string(raw)) {
			return
		}
	}
	b.mcp.PushChannel(text, meta)
}

// notify 给 Claude 一条系统通知（配对成功、被接管之类）。
func (b *Bridge) notify(text string, meta map[string]string) {
	if b.useMonitor() {
		if b.monitors.publish("飞书通知 " + text) {
			return
		}
	}
	b.mcp.PushChannel(text, meta)
}

// listenCommand 是让 Claude 用 Monitor 工具启动监听时的命令。
func listenCommand() string {
	exe, err := os.Executable()
	if err != nil {
		return "feishu-channel listen"
	}
	exe = filepath.ToSlash(exe)
	if strings.Contains(exe, " ") {
		exe = `"` + exe + `"`
	}
	return exe + " listen"
}

// waitMonitor 等 Monitor 连上来（会话刚启动时它可能还在找服务）。
func (b *Bridge) waitMonitor(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if b.monitors.count() > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- Hook 审批
