// Package access 管理谁能和 Claude 对话：私聊白名单、群策略、配对码。
package access

// 访问控制：只有 allowFrom 里的飞书用户（open_id）能把消息推进会话、能审批权限。
// 配置放在 <状态目录>/access.json，每次收到消息都会重新读取，
// 所以用 `feishu-channel access ...` 改完立即生效，不用重启。

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

type GroupPolicy struct {
	// 群里是否必须 @机器人 才处理（默认 true）。
	RequireMention *bool `json:"requireMention,omitempty"`
	// 群内额外允许的人；为空表示沿用全局 allowFrom。
	AllowFrom []string `json:"allowFrom,omitempty"`
}

type PendingPair struct {
	OpenID    string    `json:"openId"`
	ChatID    string    `json:"chatId"`
	Name      string    `json:"name,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Access struct {
	// allowlist（默认）：陌生人直接忽略，配对靠会话里生成的配对码；
	// pairing：陌生人私聊会拿到配对码，在终端用 access pair 批准；disabled：全部忽略。
	DMPolicy  string                  `json:"dmPolicy"`
	AllowFrom []string                `json:"allowFrom"`
	Groups    map[string]*GroupPolicy `json:"groups,omitempty"`
	Pending   map[string]*PendingPair `json:"pending,omitempty"`
	// 收到消息后给原消息加的表情（飞书 emoji_type，比如 Typing / OnIt / OK），空字符串关闭。
	AckReaction *string `json:"ackReaction,omitempty"`
	// 回复后是否撤掉 ack 表情。
	ClearAckOnReply *bool `json:"clearAckOnReply,omitempty"`
}

func (a *Access) Ack() string {
	if a.AckReaction == nil {
		return "Typing"
	}
	return *a.AckReaction
}

func (a *Access) ClearAck() bool {
	return a.ClearAckOnReply == nil || *a.ClearAckOnReply
}

type Store struct {
	path string
	mu   *sync.Mutex
}

var storeLocks sync.Map // 同一进程的 Bridge 和面板必须共用文件锁。

func NewStore(stateDir string) *Store {
	path := filepath.Join(stateDir, "access.json")
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	key := path
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	mu, _ := storeLocks.LoadOrStore(key, &sync.Mutex{})
	return &Store{path: path, mu: mu.(*sync.Mutex)}
}

func (s *Store) Load() (*Access, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (*Access, error) {
	a := &Access{DMPolicy: "allowlist"}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, a); err != nil {
		return nil, fmt.Errorf("access.json 格式错误: %w", err)
	}
	if a.DMPolicy == "" {
		a.DMPolicy = "allowlist"
	}
	for id, g := range a.Groups {
		if g == nil {
			return nil, fmt.Errorf("access.json: groups[%q] 不能为 null", id)
		}
	}
	return a, nil
}

func (s *Store) saveLocked(a *Access) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), "access-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Update 读-改-写，保证并发安全。
func (s *Store) Update(fn func(a *Access) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.loadLocked()
	if err != nil {
		return err
	}
	if err := fn(a); err != nil {
		return err
	}
	return s.saveLocked(a)
}

type Result int

const (
	Drop Result = iota
	Allow
	Pair
)

// Gate 判断一条消息能不能进会话。只看发送人 open_id，不看群。
func (a *Access) Gate(senderOpenID, chatID, chatType string, mentionedBot bool) Result {
	if chatType == "p2p" {
		if a.DMPolicy == "disabled" {
			return Drop
		}
		if slices.Contains(a.AllowFrom, senderOpenID) {
			return Allow
		}
		if a.DMPolicy == "pairing" {
			return Pair
		}
		return Drop
	}
	// 群聊：群必须在 groups 里登记过，发送人必须在名单里。
	g, ok := a.Groups[chatID]
	if !ok || g == nil || chatType != "group" {
		return Drop
	}
	requireMention := g.RequireMention == nil || *g.RequireMention
	if requireMention && !mentionedBot {
		return Drop
	}
	allow := g.AllowFrom
	if len(allow) == 0 {
		allow = a.AllowFrom
	}
	if slices.Contains(allow, senderOpenID) {
		return Allow
	}
	return Drop
}

const pairAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

func NewPairCode() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = pairAlphabet[int(b[i])%len(pairAlphabet)]
	}
	return string(b)
}

// StartPairing 为陌生人生成（或复用）配对码。返回码和是否为重发。
func (s *Store) StartPairing(openID, chatID, name string) (code string, resend bool, err error) {
	err = s.Update(func(a *Access) error {
		now := time.Now()
		if a.Pending == nil {
			a.Pending = map[string]*PendingPair{}
		}
		for c, p := range a.Pending {
			if p == nil || now.After(p.ExpiresAt) {
				delete(a.Pending, c)
				continue
			}
			if p.OpenID == openID {
				code, resend = c, true
			}
		}
		if code != "" {
			return nil
		}
		if len(a.Pending) >= 5 {
			return errors.New("待配对请求过多")
		}
		code = NewPairCode()
		a.Pending[code] = &PendingPair{OpenID: openID, ChatID: chatID, Name: name, ExpiresAt: now.Add(time.Hour)}
		return nil
	})
	return
}

// ApprovePairing 在终端里执行，把配对码对应的人加进白名单。
func (s *Store) ApprovePairing(code string) (*PendingPair, error) {
	var got *PendingPair
	err := s.Update(func(a *Access) error {
		code = strings.ToLower(strings.TrimSpace(code))
		p, ok := a.Pending[code]
		if !ok || p == nil || time.Now().After(p.ExpiresAt) {
			return fmt.Errorf("配对码 %q 不存在或已过期", code)
		}
		delete(a.Pending, code)
		if !slices.Contains(a.AllowFrom, p.OpenID) {
			a.AllowFrom = append(a.AllowFrom, p.OpenID)
		}
		got = p
		return nil
	})
	return got, err
}
