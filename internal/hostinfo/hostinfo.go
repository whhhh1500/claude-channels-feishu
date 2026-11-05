// Package hostinfo 判断拉起本进程的 Claude Code 是桌面端、命令行还是 SDK，以及有没有带 channel 启动参数。
package hostinfo

// 检查拉起本进程的 Claude Code 是否带了 channel 启动参数。
// 没带的话 Claude Code 会静默丢掉推送的消息，连上飞书也收不到。
// 只是尽力判断：查不到命令行时返回 unknown，不拦着用户。

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Flag int

const (
	FlagUnknown Flag = iota
	FlagPresent
	FlagMissing
)

func ChannelFlag() Flag {
	f, _ := Inspect()
	return f
}

// Inspect 沿父进程往上找到拉起本进程的 Claude Code，
// 从它的命令行判断：有没有带 channel 参数，以及它是桌面端、命令行还是 SDK。
// 只靠环境变量不可靠：在桌面 App 的终端面板里启动的命令行会继承桌面端的 CLAUDE_CODE_ENTRYPOINT。
func Inspect() (Flag, string) {
	skipFlag := os.Getenv("FEISHU_SKIP_FLAG_CHECK") != ""
	pid := os.Getppid()
	// 往上找几层：Windows 上 npm 安装的 claude 可能套了 cmd/node 壳。
	for i := 0; i < 4 && pid > 1; i++ {
		cmdline, parent, ok := ProcessInfo(pid)
		if !ok {
			break
		}
		low := strings.ToLower(cmdline)
		if !strings.Contains(low, "claude") {
			pid = parent
			continue
		}
		flag := FlagMissing
		if strings.Contains(cmdline, "--dangerously-load-development-channels") || strings.Contains(cmdline, "--channels") {
			flag = FlagPresent
		}
		if skipFlag {
			flag = FlagUnknown
		}
		return flag, Classify(low)
	}
	return FlagUnknown, Kind()
}

func Classify(lowCmdline string) string {
	path := strings.ReplaceAll(lowCmdline, "\\", "/")
	switch {
	case strings.Contains(lowCmdline, "stream-json"):
		if strings.Contains(path, "/claude/claude-code/") || os.Getenv("CLAUDE_CODE_DESKTOP_APP_VERSION") != "" {
			return "desktop"
		}
		return "sdk"
	case strings.Contains(lowCmdline, " -p ") || strings.HasSuffix(lowCmdline, " -p") || strings.Contains(lowCmdline, "--print"):
		return "sdk"
	}
	return "cli"
}

func ProcessInfo(pid int) (cmdline string, parent int, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "windows":
		script := "$p = Get-CimInstance Win32_Process -Filter \"ProcessId=" + strconv.Itoa(pid) + "\"; if ($p) { \"$($p.ParentProcessId)\"; $p.CommandLine }"
		out, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
		if err != nil {
			return "", 0, false
		}
		first, rest, _ := strings.Cut(strings.ReplaceAll(string(out), "\r", ""), "\n")
		ppid, err := strconv.Atoi(strings.TrimSpace(first))
		if err != nil {
			return "", 0, false
		}
		return strings.TrimSpace(rest), ppid, true
	case "linux":
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		if err != nil {
			return "", 0, false
		}
		st, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		ppid := 0
		if i := strings.LastIndexByte(string(st), ')'); i > 0 {
			if f := strings.Fields(string(st)[i+1:]); len(f) > 1 {
				ppid, _ = strconv.Atoi(f[1])
			}
		}
		return strings.ReplaceAll(string(b), "\x00", " "), ppid, true
	default: // macOS 等
		out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=,args=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return "", 0, false
		}
		f := strings.Fields(string(out))
		if len(f) < 2 {
			return "", 0, false
		}
		ppid, _ := strconv.Atoi(f[0])
		return strings.Join(f[1:], " "), ppid, true
	}
}

// Kind 判断拉起本进程的是哪种 Claude Code。
// 桌面 App 会设置 CLAUDE_CODE_ENTRYPOINT=claude-desktop 和 CLAUDE_CODE_DESKTOP_APP_VERSION。
func Kind() string {
	ep := os.Getenv("CLAUDE_CODE_ENTRYPOINT")
	switch {
	case ep == "claude-desktop" || os.Getenv("CLAUDE_CODE_DESKTOP_APP_VERSION") != "":
		return "desktop"
	case ep == "cli":
		return "cli"
	case strings.HasPrefix(ep, "sdk"):
		return "sdk"
	case ep == "":
		return "unknown"
	}
	return ep
}
