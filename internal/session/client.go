package session

// hook 和监听进程用的客户端。它们和本会话的飞书服务是同一个 Claude Code 进程拉起的兄弟进程，
// 通过登记文件里的 claudePid 找到对方。
//
//	feishu-channel listen   监听：订阅飞书消息，每条打印一行（由 Monitor 工具转成给 Claude 的通知）
//	feishu-channel hook     PermissionRequest / PreToolUse hook：把审批转到飞书，打印决定

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/whhhh1500/claude-channels-feishu/internal/config"
	"github.com/whhhh1500/claude-channels-feishu/internal/hostinfo"
)

// ClaudeCandidates 返回可能是"拉起我的 Claude Code"的进程号。
func ClaudeCandidates(deep bool) []int {
	var out []int
	add := func(p int) {
		if p > 1 {
			for _, x := range out {
				if x == p {
					return
				}
			}
			out = append(out, p)
		}
	}
	ppid := os.Getppid()
	add(ppid)
	if v, err := strconv.Atoi(os.Getenv("CLAUDE_PID")); err == nil {
		add(v)
	}
	if deep { // Monitor 跑在 shell 里，Claude 是祖父进程
		pid := ppid
		for i := 0; i < 3; i++ {
			_, parent, ok := hostinfo.ProcessInfo(pid)
			if !ok {
				break
			}
			add(parent)
			pid = parent
		}
	}
	return out
}

// FindOwn 找到同一个 Claude Code 进程拉起的飞书服务。
func FindOwn(cands []int) *Info {
	dir := Dir(config.StateDir())
	entries, _ := os.ReadDir(dir)
	var best *Info
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := ReadInfo(filepath.Join(dir, e.Name()))
		if err != nil || info.ClaudePID == 0 {
			continue
		}
		for _, c := range cands {
			if info.ClaudePID == c && (best == nil || info.Started.After(best.Started)) {
				best = info
			}
		}
	}
	return best
}

func Dial(info *Info, cmd string, payload json.RawMessage) (net.Conn, *bufio.Reader, error) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", info.Port), 2*time.Second)
	if err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(Request{Token: info.Token, Cmd: cmd, Payload: payload})
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		conn.Close()
		return nil, nil, err
	}
	_ = conn.SetWriteDeadline(time.Time{})
	return conn, bufio.NewReader(conn), nil
}

// listenGiveUp：监听进程连续这么久找不到本会话的飞书服务就退出。
const listenGiveUp = 10 * time.Minute

// 后台等待命令（listen --wait）的退出码：Claude 按它决定下一步。
const (
	WaitNewMessage   = 0 // 有新消息：用 Monitor 重新启动监听
	WaitDisconnected = 3 // 飞书已断开：什么都不用做
	WaitNoServer     = 4 // 找不到本会话的飞书服务：什么都不用做
	ListenNotMonitor = 5 // 不带 --wait 的 listen 没有在 Monitor 里运行
)

const listenNeedsMonitor = "飞书监听只能用 Monitor 工具启动（按 connect 返回的参数），不要用 Bash / PowerShell 直接运行 listen：" +
	"命令行工具会把输出写进文件，进程不结束就不会通知，飞书消息都会收不到。没有新消息时不用手动去拉，消息到了 Monitor 会通知。"

// isRegularFile 报告 f 是不是普通文件（不是管道、控制台）。
func isRegularFile(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode().IsRegular()
}

// RunListen 是监听进程的入口，一直运行到被停掉。
// wait=false：由 Monitor 工具启动，每条飞书消息打印一行（变成给 Claude 的通知）。
// wait=true：空闲时由后台 Bash 启动，没有时间限制，有新消息（或飞书断开）时才退出，期间不调用模型。
func RunListen(wait bool) int {
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "feishu listen: "+format+"\n", a...) }
	// Monitor 的 stdout 是管道；Bash / PowerShell 工具（包括超时后被挪到后台的）写的是文件，
	// 只在进程结束时通知一次。监听不会自己结束，消息就全写进没人看的文件里了，所以直接拒绝。
	if !wait && isRegularFile(os.Stdout) {
		fmt.Println(listenNeedsMonitor)
		logf("stdout is a file, not started by Monitor; exiting")
		return ListenNotMonitor
	}
	var deep []int
	lastOK := time.Now()
	for {
		// 会话已经没了（比如 Claude 异常退出，监听进程成了孤儿）：别一直空等
		if time.Since(lastOK) > listenGiveUp {
			logf("no feishu server for %s, exiting", listenGiveUp)
			if wait {
				fmt.Println("找不到本会话的飞书服务，不用再监听。")
				return WaitNoServer
			}
			return 0
		}
		// 先用不花时间的候选（父进程、CLAUDE_PID）找；找不到再往上翻进程树（Windows 上每层约 1 秒）
		info := FindOwn(ClaudeCandidates(false))
		if info == nil {
			if deep == nil {
				deep = ClaudeCandidates(true)
			}
			info = FindOwn(deep)
		}
		if info == nil {
			time.Sleep(time.Second)
			continue
		}
		if wait {
			conn, rd, err := Dial(info, "wait", nil)
			if err != nil {
				time.Sleep(2 * time.Second)
				continue
			}
			line, err := rd.ReadString('\n')
			conn.Close()
			if err != nil { // 服务重启了之类：接着等
				lastOK = time.Now()
				time.Sleep(2 * time.Second)
				continue
			}
			var resp Response
			if json.Unmarshal([]byte(line), &resp) == nil && resp.OK {
				fmt.Println("飞书有新消息：请用 Monitor 按原来的参数重新启动飞书监听，消息会马上补发过来。")
				return WaitNewMessage
			}
			fmt.Println("飞书已断开，不用再监听。")
			return WaitDisconnected
		}
		conn, rd, err := Dial(info, "subscribe", nil)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		ack, err := rd.ReadString('\n')
		if err != nil || !strings.Contains(ack, `"ok":true`) {
			conn.Close()
			time.Sleep(2 * time.Second)
			continue
		}
		_ = conn.SetReadDeadline(time.Time{})
		logf("subscribed to pid %d", info.PID)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				break
			}
			// 每条消息一行，打印到 stdout 就会变成给 Claude 的通知
			fmt.Fprint(os.Stdout, line)
		}
		conn.Close()
		lastOK = time.Now()
		logf("disconnected, retrying")
		time.Sleep(2 * time.Second)
	}
}

// RunHook 是 PermissionRequest / PreToolUse hook 的入口。出任何问题都静默退出 0，不挡正常流程。
func RunHook() int {
	in, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil || len(in) == 0 || !json.Valid(in) {
		return 0
	}
	info := FindOwn(ClaudeCandidates(false))
	if info == nil {
		return 0
	}
	conn, rd, err := Dial(info, "hook", in)
	if err != nil {
		return 0
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(HookWait + 30*time.Second))
	line, err := rd.ReadString('\n')
	if err != nil {
		return 0
	}
	var resp Response
	if json.Unmarshal([]byte(line), &resp) != nil {
		return 0
	}
	if resp.Output != "" {
		fmt.Fprint(os.Stdout, resp.Output)
		return 0
	}
	// 你在电脑上提交消息时，顺带看本会话的插件进程是不是比磁盘上的程序旧（hook 每次都跑最新的程序）
	var ev struct {
		Name string `json:"hook_event_name"`
	}
	if json.Unmarshal(in, &ev) == nil && ev.Name == "UserPromptSubmit" {
		if note := outdatedNote(info); note != "" {
			out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "UserPromptSubmit", "additionalContext": note}})
			fmt.Fprint(os.Stdout, string(out))
		}
	}
	return 0
}

// outdatedNote 本会话连着飞书、插件进程又比磁盘上的程序旧时，返回给 Claude 的提示；否则为空。
func outdatedNote(info *Info) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	st, err := os.Stat(exe)
	if err != nil || !st.ModTime().After(info.Started.Add(2*time.Second)) {
		return ""
	}
	if r, err := Call(info, "status", 2*time.Second); err != nil || r.Status == nil || !r.Status.Active {
		return "" // 没连飞书的会话不打扰，下次重开会话自然用上新程序
	}
	if canShutdown(info.Version) {
		return "飞书插件有新编译的版本，本会话的插件进程还是旧的。回答完用户这条消息后，按 /feishu:restart 的步骤重启飞书插件（shutdown 工具关闭插件，再调用一次 status 让它用新程序拉起），不用重开会话，也不用问用户。"
	}
	// 旧插件没有 shutdown 工具，只能请用户手动重连；同一个进程只提醒一次
	mark := filepath.Join(os.TempDir(), fmt.Sprintf("feishu-channel-outdated-%d-%d", info.PID, st.ModTime().Unix()))
	if _, err := os.Stat(mark); err == nil {
		return ""
	}
	_ = os.WriteFile(mark, nil, 0o600)
	return "飞书插件有新编译的版本，但本会话正在运行的插件太旧，不能自己重启。回答完用户这条消息后，用一句话提醒用户：在本会话输入 /mcp 重连 plugin:feishu:feishu（或重开会话），然后说\"连接飞书\"。"
}

// canShutdown 报告这个版本的插件有没有 shutdown 工具（0.3.0 起）。
func canShutdown(v string) bool {
	return v != "" && !strings.HasPrefix(v, "0.1.") && !strings.HasPrefix(v, "0.2.")
}

const HookWait = 9*time.Minute + 30*time.Second // hook 超时设为 600 秒，留点余量
