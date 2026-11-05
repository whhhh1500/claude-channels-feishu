// feishu-channel：把飞书机器人接进 Claude Code 会话的 channel（MCP stdio 服务）。
//
// 用法：
//
//	feishu-channel                       作为 MCP 服务运行（由 Claude Code 拉起）
//	feishu-channel configure <id> <secret> [--lark]
//	feishu-channel check                 检查凭据和机器人信息
//	feishu-channel web                   打开本机网页面板
//	feishu-channel access ...            管理白名单，见 access help
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/whhhh1500/claude-channels-feishu/internal/access"
	"github.com/whhhh1500/claude-channels-feishu/internal/bridge"
	"github.com/whhhh1500/claude-channels-feishu/internal/config"
	"github.com/whhhh1500/claude-channels-feishu/internal/feishu"
	"github.com/whhhh1500/claude-channels-feishu/internal/hostinfo"
	"github.com/whhhh1500/claude-channels-feishu/internal/mcp"
	"github.com/whhhh1500/claude-channels-feishu/internal/panel"
	"github.com/whhhh1500/claude-channels-feishu/internal/session"
	"github.com/whhhh1500/claude-channels-feishu/internal/version"
)

func main() {
	if len(os.Args) > 1 {
		os.Exit(runCLI(os.Args[1:]))
	}
	if err := runServer(); err != nil {
		fmt.Fprintln(os.Stderr, "feishu-channel:", err)
		os.Exit(1)
	}
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("FEISHU_DEBUG") != "" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func runServer() error {
	// stdout 只留给 JSON-RPC。飞书 SDK 有些日志会直接写 os.Stdout，统一重定向到 stderr。
	realStdout := os.Stdout
	os.Stdout = os.Stderr

	log := newLogger()
	log.Info("feishu-channel starting", "version", version.Version, "host", hostinfo.Kind(),
		"entrypoint", os.Getenv("CLAUDE_CODE_ENTRYPOINT"), "claude_session", os.Getenv("CLAUDE_CODE_SESSION_ID"))
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		// 没配凭据也照常启动：用户可以在面板里填好后再说"连接飞书"。
		log.Warn(cfgErr.Error())
	}

	srv := &mcp.Server{Name: "feishu", Version: version.Version, Instructions: bridge.Instructions, Tools: bridge.Tools}
	// 控制端口和自动连接可能在 Serve 开始读 stdin 之前推送通知。
	srv.SetOutput(realStdout, log)
	br := bridge.New(cfg, log, feishu.New(cfg, log), srv)
	srv.CallTool = br.CallTool
	srv.OnPermissionRequest = br.OnPermissionRequest

	// 本会话的控制端口，网页面板、插件 hook 和监听进程通过它找到本会话。
	ctl, err := bridge.StartControl(br, log)
	if err != nil {
		log.Warn("控制端口启动失败，网页面板和 hook 无法操作本会话", "err", err)
	} else {
		defer ctl.Close()
	}

	if os.Getenv("FEISHU_AUTOCONNECT") != "" && cfgErr == nil {
		go func() {
			if err := br.AutoConnect(context.Background()); err != nil {
				log.Error("自动连接飞书失败", "err", err)
			}
		}()
	}

	err = srv.Serve(os.Stdin, realStdout, log)
	br.Shutdown()
	return err
}

// ---------------------------------------------------------------- CLI

func runCLI(args []string) int {
	switch args[0] {
	case "configure":
		rest := slices.DeleteFunc(slices.Clone(args[1:]), func(s string) bool { return s == "--lark" })
		if len(rest) != 2 {
			fmt.Println("用法: feishu-channel configure <app_id> <app_secret> [--lark]")
			return 2
		}
		domain := "feishu"
		if slices.Contains(args, "--lark") {
			domain = "lark"
		}
		p, err := config.SaveCredentials(rest[0], rest[1], domain)
		if err != nil {
			fmt.Println("保存失败:", err)
			return 1
		}
		fmt.Println("已保存到", p)
		return 0
	case "check":
		cfg, err := config.Load()
		if err != nil {
			fmt.Println(err)
			return 1
		}
		fs := feishu.New(cfg, newLogger())
		if err := fs.LoadBotInfo(context.Background()); err != nil {
			fmt.Println("凭据或权限有问题:", err)
			return 1
		}
		fmt.Printf("凭据正常。机器人: %s (open_id %s)\n状态目录: %s\n", fs.BotName(), fs.BotOpenID(), cfg.StateDir)
		return 0
	case "access":
		return runAccess(args[1:])
	case "listen": // Monitor 监听用；--wait 是空闲时的后台等待命令
		return session.RunListen(slices.Contains(args, "--wait"))
	case "hook": // 插件 hook 用
		return session.RunHook()
	case "web", "panel":
		if err := panel.Run(newLogger(), !slices.Contains(args, "--no-open")); err != nil {
			fmt.Println("面板启动失败:", err)
			return 1
		}
		return 0
	case "version", "--version", "-v":
		fmt.Println(version.Version)
		return 0
	default:
		fmt.Println(`feishu-channel ` + version.Version + `

  (无参数)                              作为 MCP channel 服务运行，由 Claude Code 拉起
  configure <app_id> <app_secret> [--lark]  保存飞书应用凭据
  check                                 检查凭据
  web [--no-open]                       打开本机网页面板（填凭据、开关会话、配对、白名单）
  access help                           管理谁能和 Claude 对话`)
		return 2
	}
}

func runAccess(args []string) int {
	store := access.NewStore(config.StateDir())
	if len(args) == 0 {
		args = []string{"list"}
	}
	fail := func(err error) int {
		fmt.Println("失败:", err)
		return 1
	}
	need := func(n int, usage string) bool {
		if len(args) < n {
			fmt.Println("用法: feishu-channel access " + usage)
			return false
		}
		return true
	}
	switch args[0] {
	case "list":
		a, err := store.Load()
		if err != nil {
			return fail(err)
		}
		fmt.Println("私聊策略:", a.DMPolicy)
		fmt.Println("白名单:")
		for _, id := range a.AllowFrom {
			fmt.Println("  ", id)
		}
		if len(a.Groups) > 0 {
			fmt.Println("群:")
			for id, g := range a.Groups {
				mention := g.RequireMention == nil || *g.RequireMention
				fmt.Printf("   %s  需要@: %v  额外允许: %v\n", id, mention, g.AllowFrom)
			}
		}
		if len(a.Pending) > 0 {
			fmt.Println("待配对:")
			for code, p := range a.Pending {
				fmt.Printf("   %s  %s  到期 %s\n", code, p.OpenID, p.ExpiresAt.Format("15:04"))
			}
		}
		return 0
	case "pair":
		if !need(2, "pair <配对码>") {
			return 2
		}
		p, err := store.ApprovePairing(args[1])
		if err != nil {
			return fail(err)
		}
		fmt.Println("已加入白名单:", p.OpenID)
		// 顺手告诉对方配对成功
		if cfg, err := config.Load(); err == nil {
			fs := feishu.New(cfg, newLogger())
			_, _ = fs.SendText(context.Background(), p.ChatID, "", "配对成功，现在可以直接和 Claude 对话了。")
		}
		return 0
	case "allow":
		if !need(2, "allow <open_id>") {
			return 2
		}
		err := store.Update(func(a *access.Access) error {
			if !slices.Contains(a.AllowFrom, args[1]) {
				a.AllowFrom = append(a.AllowFrom, args[1])
			}
			return nil
		})
		if err != nil {
			return fail(err)
		}
		fmt.Println("ok")
		return 0
	case "remove":
		if !need(2, "remove <open_id>") {
			return 2
		}
		err := store.Update(func(a *access.Access) error {
			a.AllowFrom = slices.DeleteFunc(a.AllowFrom, func(s string) bool { return s == args[1] })
			return nil
		})
		if err != nil {
			return fail(err)
		}
		fmt.Println("ok")
		return 0
	case "policy":
		if !need(2, "policy <pairing|allowlist|disabled>") {
			return 2
		}
		p := args[1]
		if p != "pairing" && p != "allowlist" && p != "disabled" {
			return fail(fmt.Errorf("未知策略 %q", p))
		}
		if err := store.Update(func(a *access.Access) error { a.DMPolicy = p; return nil }); err != nil {
			return fail(err)
		}
		fmt.Println("ok")
		return 0
	case "group":
		if !need(3, "group add <chat_id> [--no-mention] | group remove <chat_id>") {
			return 2
		}
		chatID := args[2]
		err := store.Update(func(a *access.Access) error {
			switch args[1] {
			case "add":
				if a.Groups == nil {
					a.Groups = map[string]*access.GroupPolicy{}
				}
				g := &access.GroupPolicy{}
				if slices.Contains(args, "--no-mention") {
					f := false
					g.RequireMention = &f
				}
				a.Groups[chatID] = g
			case "remove":
				delete(a.Groups, chatID)
			default:
				return fmt.Errorf("未知子命令 %q", args[1])
			}
			return nil
		})
		if err != nil {
			return fail(err)
		}
		fmt.Println("ok")
		return 0
	case "ack":
		if !need(2, "ack <emoji_type|off>") {
			return 2
		}
		v := args[1]
		if strings.EqualFold(v, "off") {
			v = ""
		}
		if err := store.Update(func(a *access.Access) error { a.AckReaction = &v; return nil }); err != nil {
			return fail(err)
		}
		fmt.Println("ok")
		return 0
	default:
		fmt.Println(`feishu-channel access 子命令：
  list                         查看白名单、群、待配对
  pair <配对码>                批准配对（陌生人私聊机器人会收到配对码）
  allow <open_id>              直接加白名单
  remove <open_id>             移出白名单
  policy pairing|allowlist|disabled   私聊策略
  group add <chat_id> [--no-mention]  允许某个群（默认需要 @机器人）
  group remove <chat_id>
  ack <emoji_type|off>         收到消息时加的表情，默认 Typing`)
		return 2
	}
}
