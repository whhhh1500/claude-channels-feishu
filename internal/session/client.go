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
)

// RunListen 是监听进程的入口，一直运行到被停掉。
// wait=false：由 Monitor 工具启动，每条飞书消息打印一行（变成给 Claude 的通知）。
// wait=true：空闲时由后台 Bash 启动，没有时间限制，有新消息（或飞书断开）时才退出，期间不调用模型。
func RunListen(wait bool) int {
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "feishu listen: "+format+"\n", a...) }
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
	if json.Unmarshal([]byte(line), &resp) != nil || resp.Output == "" {
		return 0
	}
	fmt.Fprint(os.Stdout, resp.Output)
	return 0
}

const HookWait = 9*time.Minute + 30*time.Second // hook 超时设为 600 秒，留点余量
