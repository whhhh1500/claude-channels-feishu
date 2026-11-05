// Package session 管理会话登记文件，并提供 hook 和监听进程连回同一会话飞书服务的客户端。
package session

// 会话登记：每个飞书服务在 <状态目录>/sessions/<pid>.json 写下控制端口和口令，
// 网页面板据此列出所有会话并下发命令。

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Info struct {
	PID     int       `json:"pid"`
	Port    int       `json:"port"`
	Token   string    `json:"token"`
	Cwd     string    `json:"cwd"`
	Started time.Time `json:"started"`
	Version string    `json:"version"`
	// 由拉起本进程的 Claude Code 通过环境变量告诉我们
	Host          string `json:"host"`          // desktop / cli / sdk / 其他
	Entrypoint    string `json:"entrypoint"`    // CLAUDE_CODE_ENTRYPOINT 原值
	ClaudeSession string `json:"claudeSession"` // CLAUDE_CODE_SESSION_ID
	// 拉起本进程的 Claude Code 进程号。插件的 hook 和 Monitor 用它找到同一个会话里的飞书服务。
	ClaudePID int `json:"claudePid"`
}

type Status struct {
	PID           int       `json:"pid"`
	Cwd           string    `json:"cwd"`
	Started       time.Time `json:"started"`
	Active        bool      `json:"active"`
	Ready         bool      `json:"ready"`
	Hub           bool      `json:"hub"` // 正负责接收飞书事件（多个会话在线时只有一个）
	Since         time.Time `json:"since,omitempty"`
	BotName       string    `json:"botName,omitempty"`
	ChannelFlag   string    `json:"channelFlag"` // present / missing / unknown / checking
	Host          string    `json:"host"`
	Entrypoint    string    `json:"entrypoint"`
	ClaudeSession string    `json:"claudeSession"`
	Title         string    `json:"title,omitempty"`   // 用户给会话起的名字
	WorkDir       string    `json:"workDir,omitempty"` // Claude 当前所在目录（可能和启动目录不同）
	Tag           string    `json:"tag,omitempty"`     // 飞书回复开头的会话标识
}

type Request struct {
	Token   string          `json:"token"`
	Cmd     string          `json:"cmd"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Response struct {
	OK       bool     `json:"ok"`
	Error    string   `json:"error,omitempty"`
	Status   *Status  `json:"status,omitempty"`
	PairCode string   `json:"pairCode,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	Output   string   `json:"output,omitempty"` // hook 要打印的内容
}

func Dir(stateDir string) string { return filepath.Join(stateDir, "sessions") }

func RandomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Peer 是一个还活着的会话：登记信息（用来调用它）和当前状态。
type Peer struct {
	Info   *Info
	Status Status
}

// List 读取所有登记的会话并逐个询问状态；联系不上的视为已退出，顺手清理。
func List(stateDir string) []Status {
	peers := Peers(stateDir)
	out := make([]Status, 0, len(peers))
	for _, p := range peers {
		out = append(out, p.Status)
	}
	return out
}

// Peers 同 List，另外带上登记信息。
func Peers(stateDir string) []Peer {
	entries, _ := os.ReadDir(Dir(stateDir))
	var out []Peer
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(Dir(stateDir), e.Name())
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := ReadInfo(path)
			if err != nil {
				return
			}
			resp, err := Call(info, "status", 2*time.Second)
			if err != nil {
				os.Remove(path)
				return
			}
			if resp.Status != nil {
				mu.Lock()
				out = append(out, Peer{Info: info, Status: *resp.Status})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

func ReadInfo(path string) (*Info, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var info Info
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func Command(stateDir string, pid int, cmd string) (*Response, error) {
	info, err := ReadInfo(filepath.Join(Dir(stateDir), strconv.Itoa(pid)+".json"))
	if err != nil {
		return nil, errors.New("找不到这个会话，可能已经退出")
	}
	return Call(info, cmd, 40*time.Second)
}

func Call(info *Info, cmd string, timeout time.Duration) (*Response, error) {
	return CallWith(info, cmd, nil, timeout)
}

// CallWith 同 Call，附带 payload（会话之间转发飞书事件用）。
func CallWith(info *Info, cmd string, payload any, timeout time.Duration) (*Response, error) {
	var body json.RawMessage
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = raw
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", info.Port), 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	raw, _ := json.Marshal(Request{Token: info.Token, Cmd: cmd, Payload: body})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
