package bridge

// 本会话的控制端口（服务端）：网页面板、插件 hook 和监听进程通过它查询状态、开关飞书、
// 提交审批、订阅消息。登记文件和客户端在 session 包里。

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/whhhh1500/claude-channels-feishu/internal/hostinfo"
	"github.com/whhhh1500/claude-channels-feishu/internal/session"
	"github.com/whhhh1500/claude-channels-feishu/internal/version"
)

type Control struct {
	b      *Bridge
	log    *slog.Logger
	l      net.Listener
	info   session.Info
	file   string
	ctx    context.Context
	cancel context.CancelFunc
	conns  map[net.Conn]struct{}

	flagMu sync.Mutex
	flag   string
	host   string
	closed bool
}

func StartControl(b *Bridge, log *slog.Logger) (*Control, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	cwd, _ := os.Getwd()
	c := &Control{b: b, log: log, l: l, flag: "checking", info: session.Info{
		PID: os.Getpid(), Port: l.Addr().(*net.TCPAddr).Port, Token: session.RandomToken(),
		Cwd: cwd, Started: time.Now(), Version: version.Version,
		Host: hostinfo.Kind(), Entrypoint: os.Getenv("CLAUDE_CODE_ENTRYPOINT"), ClaudeSession: os.Getenv("CLAUDE_CODE_SESSION_ID"),
		ClaudePID: os.Getppid(),
	}}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.conns = make(map[net.Conn]struct{})
	dir := session.Dir(b.config().StateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		l.Close()
		return nil, err
	}
	c.file = filepath.Join(dir, strconv.Itoa(c.info.PID)+".json")
	raw, _ := json.MarshalIndent(c.info, "", "  ")
	if err := os.WriteFile(c.file, raw, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	go func() {
		f, host := hostinfo.Inspect()
		c.flagMu.Lock()
		defer c.flagMu.Unlock()
		if c.closed {
			return
		}
		c.flag = map[hostinfo.Flag]string{hostinfo.FlagPresent: "present", hostinfo.FlagMissing: "missing", hostinfo.FlagUnknown: "unknown"}[f]
		c.host = host
		info := c.info // 已发布的控制端口和口令保持只读。
		info.Host = host
		raw, _ := json.MarshalIndent(info, "", "  ")
		b.setFlag(f)
		b.setHost(host)
		_ = os.WriteFile(c.file, raw, 0o600)
		log.Info("host detected", "host", host, "channel_flag", f)
	}()
	go c.serve()
	return c, nil
}

func (c *Control) Close() {
	c.flagMu.Lock()
	defer c.flagMu.Unlock()
	c.closed = true
	c.cancel()
	c.l.Close()
	for conn := range c.conns {
		conn.Close()
	}
	os.Remove(c.file)
}

func (c *Control) status() *session.Status {
	c.flagMu.Lock()
	flag, host := c.flag, c.host
	if host == "" {
		host = c.info.Host
	}
	c.flagMu.Unlock()
	st := &session.Status{
		PID: c.info.PID, Cwd: c.info.Cwd, Started: c.info.Started,
		Active: c.b.conn.Active(), Ready: c.b.conn.Ready(), Hub: c.b.conn.Hub(), ChannelFlag: flag,
		BotName: c.b.client().BotName(),
		Host:    host, Entrypoint: c.info.Entrypoint, ClaudeSession: c.info.ClaudeSession,
	}
	st.Title, st.WorkDir = transcript.sessionInfo()
	st.Tag = sessionTag()
	if st.Active {
		st.Since = c.b.conn.Since()
	}
	return st
}

func (c *Control) serve() {
	for {
		conn, err := c.l.Accept()
		if err != nil {
			return
		}
		c.flagMu.Lock()
		if c.closed {
			c.flagMu.Unlock()
			conn.Close()
			return
		}
		c.conns[conn] = struct{}{}
		c.flagMu.Unlock()
		go c.handle(conn)
	}
}

func (c *Control) handle(conn net.Conn) {
	defer conn.Close()
	defer func() {
		c.flagMu.Lock()
		delete(c.conns, conn)
		c.flagMu.Unlock()
	}()
	_ = conn.SetDeadline(time.Now().Add(40 * time.Second))
	rd := bufio.NewReader(conn)
	// hook 客户端最多接收 8 MiB；避免未认证连接无限增长内存。
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 4096), 64<<20)
	if !sc.Scan() {
		return
	}
	line := sc.Bytes()
	var req session.Request
	resp := session.Response{}
	if json.Unmarshal(line, &req) != nil || c.info.Token == "" || req.Token != c.info.Token {
		resp.Error = "口令错误"
	} else {
		switch req.Cmd {
		case "subscribe":
			c.subscribe(conn, rd)
			return
		case "wait": // 空闲时的后台等待命令：有暂存消息才返回
			_ = conn.SetDeadline(time.Time{})
			done := make(chan struct{})
			go func() { // 对方退出（比如被 TaskStop 停掉）时不再等
				_, _ = rd.ReadString('\n')
				close(done)
			}()
			resp.OK = c.b.monitors.waitBacklog(done)
		case "hook":
			// 等用户在飞书上处理，最长约 9.5 分钟
			_ = conn.SetDeadline(time.Now().Add(session.HookWait + time.Minute))
			resp = session.Response{OK: true, Output: c.b.handleHook(c.ctx, req.Payload)}
		// 以下三个由负责接收的会话（hub）调用，见 router.go
		case "claim":
			var q claimQuery
			resp.OK = json.Unmarshal(req.Payload, &q) == nil && c.b.conn.Active() && c.b.claims(q)
		case "message":
			var data larkim.P2MessageReceiveV1Data
			if !c.b.conn.Active() {
				resp.Error = "这个会话没连接飞书"
			} else if err := json.Unmarshal(req.Payload, &data); err != nil {
				resp.Error = err.Error()
			} else {
				c.b.acceptForwarded(&data)
				resp.OK = true
			}
		case "card":
			resp = session.Response{OK: true, Output: c.b.handleForwardedCard(req.Payload)}
		default:
			resp = c.exec(req.Cmd)
		}
	}
	raw, _ := json.Marshal(resp)
	_, _ = conn.Write(append(raw, '\n'))
}

// subscribe 是监听进程（feishu-channel listen）的长连接：每条飞书消息一行。对方断开就退订。
func (c *Control) subscribe(conn net.Conn, rd *bufio.Reader) {
	_ = conn.SetDeadline(time.Time{})
	ch := c.b.monitors.add()
	defer c.b.monitors.remove(ch)
	c.log.Info("monitor subscribed")
	closed := make(chan struct{})
	go func() { // 读到 EOF 说明 Monitor 退出了
		_, _ = rd.ReadString('\n')
		close(closed)
	}()
	if _, err := conn.Write([]byte("{\"ok\":true}\n")); err != nil {
		return
	}
	for {
		select {
		case line := <-ch:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := conn.Write([]byte(line + "\n")); err != nil {
				return
			}
		case <-closed:
			c.log.Info("monitor unsubscribed")
			return
		}
	}
}

func (c *Control) exec(cmd string) session.Response {
	switch cmd {
	case "status":
		return session.Response{OK: true, Status: c.status()}
	case "connect":
		r, err := c.b.doConnect(context.Background())
		if err != nil {
			return session.Response{Error: err.Error(), Status: c.status()}
		}
		c.b.notify("用户在飞书面板里打开了飞书连接，之后飞书消息会进入本会话。不需要回复这条通知。",
			map[string]string{"event": "connected"})
		if r.NeedMonitor {
			// 面板连接时 Claude 不在场：消息先暂存，等会话里启动监听后补发
			r.Notes = append(r.Notes, "这个会话还没有飞书监听（监听要由会话里的 Claude 启动）。消息会先暂存：去这个会话里说一句\"连接飞书\"，Claude 会启动监听并补收消息。")
		}
		return session.Response{OK: true, Status: c.status(), PairCode: r.PairCode, Notes: r.Notes}
	case "disconnect":
		if c.b.conn.Disconnect() {
			c.b.notify("用户在飞书面板里断开了飞书连接，本会话不再收发飞书消息。如果用 Monitor 工具启动过飞书监听，可以用 TaskStop 停掉。不需要回复这条通知。",
				map[string]string{"event": "disconnected"})
			c.b.setMode("")
		}
		return session.Response{OK: true, Status: c.status()}
	case "pair":
		if !c.b.conn.Active() {
			return session.Response{Error: "这个会话没连接飞书，先连接", Status: c.status()}
		}
		return session.Response{OK: true, Status: c.status(), PairCode: c.b.NewPairCode()}
	}
	return session.Response{Error: "未知命令 " + cmd}
}

// ---------------------------------------------------------------- 面板侧：调用会话
