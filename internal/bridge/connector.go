package bridge

// 飞书开关。默认关，用户在会话里说"连接飞书"时 Claude 调用 connect 工具才打开。
// 多个会话可以同时打开。飞书长连接每条事件只推给一个连接，所以用本机端口当锁：
// 拿到锁的会话建立长连接、接收事件，再按引用关系转给对应会话（见 router.go）；
// 它下线后，别的在线会话会自动拿到锁接手。

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/whhhh1500/claude-channels-feishu/internal/config"
	"github.com/whhhh1500/claude-channels-feishu/internal/feishu"
)

type Connector struct {
	log *slog.Logger
	b   *Bridge

	mu        sync.Mutex
	lifecycle sync.Mutex // 串行化开关操作，等待旧连接退出期间不能启动新连接。
	cancel    context.CancelFunc
	done      chan struct{}
	connected atomic.Bool // 本会话负责接收，且长连接已就绪
	hub       atomic.Bool // 本会话拿到了锁，负责接收飞书事件并转给各会话
	since     time.Time
}

func NewConnector(log *slog.Logger, b *Bridge) *Connector {
	return &Connector{log: log, b: b}
}

func lockAddr() string {
	if a := os.Getenv("FEISHU_LOCK_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:47831"
}

// 没拿到锁的在线会话隔多久试一次接手（负责接收的会话断开或退出后）。
const lockRetry = 2 * time.Second

// Active 表示开关处于打开状态（本会话在线，可能还在连接中）。
func (c *Connector) Active() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancel != nil
}

// Hub 表示本会话正负责接收飞书事件。
func (c *Connector) Hub() bool { return c.hub.Load() }

// Ready 表示能收到消息：自己负责接收时看长连接；否则由别的在线会话接收后转过来。
func (c *Connector) Ready() bool {
	if c.hub.Load() {
		return c.connected.Load()
	}
	return c.Active()
}

func (c *Connector) Since() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.since
}

// Connect 打开开关。拿到锁就由本会话建立长连接（最多等 15 秒连上）；
// 锁已被别的在线会话拿着时不抢，本会话只是上线，消息由它按引用关系转过来，它下线后本会话自动接手。
func (c *Connector) Connect(ctx context.Context) (hub bool, err error) {
	c.lifecycle.Lock()
	c.mu.Lock()
	if c.cancel != nil {
		c.mu.Unlock()
		c.lifecycle.Unlock()
		return c.hub.Load(), nil
	}
	// 每次连接都重新读凭据，面板里改过的立即生效。
	cfg, fs, err := c.b.reloadConfig()
	if err != nil {
		c.mu.Unlock()
		c.lifecycle.Unlock()
		return false, err
	}
	if fs.BotOpenID() == "" {
		if err := fs.LoadBotInfo(ctx); err != nil {
			c.mu.Unlock()
			c.lifecycle.Unlock()
			return false, err
		}
	}
	lock, _ := net.Listen("tcp", lockAddr())
	runCtx, cancel := context.WithCancel(context.Background())
	c.cancel, c.done, c.since = cancel, make(chan struct{}), time.Now()
	ready := make(chan struct{}, 1)
	go c.run(runCtx, cfg, lock, ready, c.done)
	c.mu.Unlock()
	c.lifecycle.Unlock()

	if lock == nil {
		c.log.Info("另一个会话正负责接收飞书消息，本会话上线等待转发", "lock", lockAddr())
		return false, nil
	}
	select {
	case <-ready:
		return true, nil
	case <-time.After(15 * time.Second):
		return true, errors.New("15 秒内没连上飞书，后台会继续重试；请检查网络和开放平台的长连接配置")
	case <-ctx.Done():
		return true, ctx.Err()
	}
}

// Disconnect 关闭开关：本会话下线；如果是它在负责接收，就断开长连接并释放锁，让别的在线会话接手。
func (c *Connector) Disconnect() bool {
	return c.disconnect("manual")
}

// DisconnectFor 同 Disconnect，reason 记进连接历史：manual / exit。
func (c *Connector) DisconnectFor(reason string) bool {
	return c.disconnect(reason)
}

func (c *Connector) disconnect(reason string) bool {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel = nil
	c.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	c.connected.Store(false)
	c.log.Info("飞书开关已关闭", "reason", reason)
	c.b.recordDisconnect(reason)
	return true
}

// run 开关打开期间一直运行：没拿到锁就定期重试，拿到锁就负责接收，直到开关关闭。
func (c *Connector) run(ctx context.Context, cfg *config.Config, lock net.Listener, ready chan struct{}, done chan struct{}) {
	defer close(done)
	for lock == nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(lockRetry):
		}
		if l, err := net.Listen("tcp", lockAddr()); err == nil {
			lock = l
			c.log.Info("接手负责接收飞书消息")
		}
	}
	defer lock.Close()
	go serveLock(lock)
	c.hub.Store(true)
	defer c.hub.Store(false)
	c.receive(ctx, cfg, ready, done)
}

func (c *Connector) receive(ctx context.Context, cfg *config.Config, ready chan struct{}, done chan struct{}) {
	setReady := func(value bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.done == done && ctx.Err() == nil {
			c.connected.Store(value)
		}
	}
	d := dispatcher.NewEventDispatcher("", "")
	d.InitConfig(larkevent.WithLogger(feishu.LogAdapter{L: c.log.With("component", "lark-event")}), larkevent.WithLogLevel(sdkLogLevel(larkcore.LogLevelWarn)))
	d.OnP2MessageReceiveV1(c.b.OnMessage)
	d.OnP2CardActionTrigger(c.b.OnCardAction)
	// 用户打开和机器人的私聊窗口时飞书会推这个事件；注册空处理，免得 SDK 报 not found handler。
	d.OnP2ChatAccessEventBotP2pChatEnteredV1(func(context.Context, *larkim.P2ChatAccessEventBotP2pChatEnteredV1) error { return nil })

	var wasReady atomic.Bool
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		ws := larkws.NewClient(cfg.AppID, cfg.AppSecret,
			larkws.WithEventHandler(d),
			larkws.WithDomain(feishu.DomainURL(cfg.Domain)),
			larkws.WithLogger(feishu.LogAdapter{L: c.log.With("component", "lark-ws")}),
			larkws.WithLogLevel(sdkLogLevel(larkcore.LogLevelInfo)),
			larkws.WithAutoReconnect(true),
			larkws.WithOnReady(func() {
				wasReady.Store(true)
				setReady(true)
				select {
				case ready <- struct{}{}:
				default:
				}
				c.log.Info("飞书长连接已建立")
			}),
			larkws.WithOnReconnecting(func() { setReady(false) }),
			larkws.WithOnReconnected(func() { setReady(true) }),
		)
		err := ws.Start(ctx)
		setReady(false)
		if ctx.Err() != nil {
			return
		}
		if wasReady.Swap(false) {
			backoff = 5 * time.Second
		}
		c.log.Error("飞书长连接断开，稍后重建", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

// serveLock 持锁期间把连到锁端口的连接直接关掉（旧版本会发 "release" 请求接管，现在不再让出）。
func serveLock(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}
}

// sdkLogLevel 设了 FEISHU_DEBUG 时把飞书 SDK 的日志开到 debug（能看到每个收到的帧）。
func sdkLogLevel(def larkcore.LogLevel) larkcore.LogLevel {
	if os.Getenv("FEISHU_DEBUG") != "" {
		return larkcore.LogLevelDebug
	}
	return def
}
