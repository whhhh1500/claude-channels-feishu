// Package panel 是本机网页面板：填凭据、给各个会话开关飞书、生成配对码、管理白名单和群。
package panel

// 本机网页面板：填凭据、测试、给各个会话开关飞书连接、生成配对码、管理白名单和群。
// 只监听 127.0.0.1；接口要求 X-Panel-Token 头（口令在状态目录的 panel.token 里），
// 并校验 Host，防止别的网页跨站调用或 DNS 重绑定。

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/whhhh1500/claude-channels-feishu/internal/access"
	"github.com/whhhh1500/claude-channels-feishu/internal/config"
	"github.com/whhhh1500/claude-channels-feishu/internal/feishu"
	"github.com/whhhh1500/claude-channels-feishu/internal/session"
	"github.com/whhhh1500/claude-channels-feishu/internal/version"
)

//go:embed panel.html
var panelHTML []byte

func panelAddr() string {
	if a := os.Getenv("FEISHU_PANEL_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:47830"
}

func panelToken(stateDir string) (string, error) {
	p := filepath.Join(stateDir, "panel.token")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	// 多个会话可能同时首次启动；只能由一个进程创建，不能覆盖正在使用的口令。
	for i := 0; i < 50; i++ {
		b, err := os.ReadFile(p)
		if err == nil {
			if t := strings.TrimSpace(string(b)); len(t) >= 16 {
				return t, nil
			}
			// 创建者可能刚打开文件、尚未写完。
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		t := session.RandomToken()
		_, writeErr := f.WriteString(t)
		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			os.Remove(p)
			return "", err
		}
		return t, nil
	}
	return "", errors.New("panel.token 为空或无效")
}

func panelURL(addr, token string) string { return fmt.Sprintf("http://%s/?t=%s", addr, token) }

var (
	inProcPanelOnce sync.Once
	inProcPanelErr  error
)

// Ensure 在本进程里启动面板；端口已被占（别的会话或 web 命令开着）就直接复用那个。
func Ensure(log *slog.Logger) (string, error) {
	dir := config.StateDir()
	token, err := panelToken(dir)
	if err != nil {
		return "", err
	}
	addr := panelAddr()
	inProcPanelOnce.Do(func() {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			// 多半是已经有面板在跑，检查一下
			c, derr := net.DialTimeout("tcp", addr, time.Second)
			if derr != nil {
				inProcPanelErr = fmt.Errorf("面板端口 %s 不可用: %w", addr, err)
				return
			}
			c.Close()
			return
		}
		srv := &http.Server{Handler: newHandler(addr, token, log), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(l)
	})
	if inProcPanelErr != nil {
		return "", inProcPanelErr
	}
	return panelURL(addr, token), nil
}

// Run 是 `feishu-channel web` 命令：前台运行面板并打开浏览器。
func Run(log *slog.Logger, openBrowser bool) error {
	dir := config.StateDir()
	token, err := panelToken(dir)
	if err != nil {
		return err
	}
	addr := panelAddr()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		url := panelURL(addr, token)
		fmt.Println("面板已经在运行：", url)
		if openBrowser {
			OpenURL(url)
		}
		return nil
	}
	url := panelURL(addr, token)
	fmt.Println("飞书面板：", url)
	fmt.Println("按 Ctrl+C 退出")
	if openBrowser {
		OpenURL(url)
	}
	srv := &http.Server{Handler: newHandler(addr, token, log), ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(l)
}

func OpenURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}
}

type panelState struct {
	Version  string           `json:"version"`
	StateDir string           `json:"stateDir"`
	Exe      string           `json:"exe"`
	Config   panelConfig      `json:"config"`
	Sessions []session.Status `json:"sessions"`
	Access   panelAccess      `json:"access"`
}

type panelConfig struct {
	AppID     string `json:"appId"`
	Domain    string `json:"domain"`
	HasSecret bool   `json:"hasSecret"`
}

type panelGroup struct {
	ChatID         string   `json:"chatId"`
	RequireMention bool     `json:"requireMention"`
	AllowFrom      []string `json:"allowFrom"`
}

type panelAccess struct {
	DMPolicy  string       `json:"dmPolicy"`
	AllowFrom []string     `json:"allowFrom"`
	Groups    []panelGroup `json:"groups"`
	Ack       string       `json:"ack"`
}

func newHandler(addr, token string, log *slog.Logger) http.Handler {
	_, port, _ := net.SplitHostPort(addr)
	allowedHosts := []string{"127.0.0.1:" + port, "localhost:" + port}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		_, _ = w.Write(panelHTML)
	})

	api := func(pattern string, h func(r *http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			if !slices.Contains(allowedHosts, r.Host) || token == "" || r.Header.Get("X-Panel-Token") != token {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "口令无效，请用终端打印的完整链接打开面板"})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			v, err := h(r)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(v)
		})
	}

	api("GET /api/state", func(r *http.Request) (any, error) {
		dir := config.StateDir()
		cfg, _ := config.Load()
		exe, _ := os.Executable()
		st := panelState{Version: version.Version, StateDir: dir, Exe: filepath.ToSlash(exe),
			Config:   panelConfig{AppID: cfg.AppID, Domain: cfg.Domain, HasSecret: cfg.AppSecret != ""},
			Sessions: session.List(dir),
		}
		slices.SortFunc(st.Sessions, func(a, b session.Status) int { return b.Started.Compare(a.Started) })
		acc, err := access.NewStore(dir).Load()
		if err != nil {
			return nil, err
		}
		st.Access = panelAccess{DMPolicy: acc.DMPolicy, AllowFrom: acc.AllowFrom, Ack: acc.Ack()}
		for id, g := range acc.Groups {
			st.Access.Groups = append(st.Access.Groups, panelGroup{ChatID: id, RequireMention: g.RequireMention == nil || *g.RequireMention, AllowFrom: g.AllowFrom})
		}
		slices.SortFunc(st.Access.Groups, func(a, b panelGroup) int { return strings.Compare(a.ChatID, b.ChatID) })
		if st.Access.AllowFrom == nil {
			st.Access.AllowFrom = []string{}
		}
		return st, nil
	})

	api("GET /api/history", func(r *http.Request) (any, error) {
		return buildHistory(config.StateDir())
	})

	api("POST /api/config", func(r *http.Request) (any, error) {
		var in struct{ AppID, AppSecret, Domain string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		in.AppID, in.AppSecret = strings.TrimSpace(in.AppID), strings.TrimSpace(in.AppSecret)
		if in.AppSecret == "" { // 留空表示不改
			cfg, _ := config.Load()
			in.AppSecret = cfg.AppSecret
		}
		if in.AppID == "" || in.AppSecret == "" {
			return nil, errors.New("App ID 和 App Secret 都要填")
		}
		if in.Domain != "lark" {
			in.Domain = "feishu"
		}
		if _, err := config.SaveCredentials(in.AppID, in.AppSecret, in.Domain); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	})

	api("POST /api/check", func(r *http.Request) (any, error) {
		cfg, err := config.Load()
		if err != nil {
			return nil, err
		}
		fs := feishu.New(cfg, log)
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if err := fs.LoadBotInfo(ctx); err != nil {
			return nil, err
		}
		return map[string]string{"botName": fs.BotName(), "openId": fs.BotOpenID()}, nil
	})

	api("POST /api/session", func(r *http.Request) (any, error) {
		var in struct {
			PID int    `json:"pid"`
			Cmd string `json:"cmd"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		if !slices.Contains([]string{"connect", "disconnect", "pair", "status"}, in.Cmd) {
			return nil, errors.New("未知操作")
		}
		resp, err := session.Command(config.StateDir(), in.PID, in.Cmd)
		if err != nil {
			return nil, err
		}
		if !resp.OK && resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		return resp, nil
	})

	api("POST /api/access", func(r *http.Request) (any, error) {
		var in struct {
			Op        string `json:"op"`
			Value     string `json:"value"`
			NoMention bool   `json:"noMention"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		v := strings.TrimSpace(in.Value)
		err := access.NewStore(config.StateDir()).Update(func(a *access.Access) error {
			switch in.Op {
			case "allow":
				if !strings.HasPrefix(v, "ou_") {
					return errors.New("open_id 应以 ou_ 开头")
				}
				if !slices.Contains(a.AllowFrom, v) {
					a.AllowFrom = append(a.AllowFrom, v)
				}
			case "remove":
				a.AllowFrom = slices.DeleteFunc(a.AllowFrom, func(s string) bool { return s == v })
			case "policy":
				if v != "allowlist" && v != "pairing" && v != "disabled" {
					return errors.New("未知策略")
				}
				a.DMPolicy = v
			case "ack":
				a.AckReaction = &v
			case "groupAdd":
				if !strings.HasPrefix(v, "oc_") {
					return errors.New("群 chat_id 应以 oc_ 开头")
				}
				if a.Groups == nil {
					a.Groups = map[string]*access.GroupPolicy{}
				}
				g := &access.GroupPolicy{}
				if in.NoMention {
					f := false
					g.RequireMention = &f
				}
				a.Groups[v] = g
			case "groupRemove":
				delete(a.Groups, v)
			default:
				return errors.New("未知操作")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	})

	return mux
}
