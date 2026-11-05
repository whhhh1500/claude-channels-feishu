// Package mcp 是一个最小的 MCP stdio 服务端，支持 Claude Code 的 channel 扩展通知。
package mcp

// 最小化的 MCP stdio 服务端：按行读写 JSON-RPC 2.0。
// 只实现 channel 需要的部分：initialize / ping / tools/list / tools/call，
// 以及 Claude Code 的两个扩展通知（channel 推送、权限请求/裁决）。
// stdout 只能写 JSON-RPC，所有日志一律走 stderr。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// maxProtocolVersion 是协商时最多答应的 MCP 协议版本：
// 按 Claude Code 文档，协商到 2026-07-28 及以后的版本时 Channel 推送不生效。
const maxProtocolVersion = "2025-11-25"

// negotiateVersion 回应客户端要的协议版本，但不超过 maxProtocolVersion（版本号是日期，可以按字符串比较）。
func negotiateVersion(want string) string {
	switch {
	case want == "":
		return "2025-06-18"
	case want > maxProtocolVersion:
		return maxProtocolVersion
	}
	return want
}

const (
	methodChannel           = "notifications/claude/channel"
	methodPermissionRequest = "notifications/claude/channel/permission_request"
	methodPermissionVerdict = "notifications/claude/channel/permission"
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type Result struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func Text(format string, a ...any) Result {
	return Result{Content: []Content{{Type: "text", Text: fmt.Sprintf(format, a...)}}}
}

func Error(err error) Result {
	return Result{Content: []Content{{Type: "text", Text: err.Error()}}, IsError: true}
}

type PermissionRequest struct {
	RequestID    string `json:"request_id"`
	ToolName     string `json:"tool_name"`
	Description  string `json:"description"`
	InputPreview string `json:"input_preview"`
}

type Server struct {
	Name         string
	Version      string
	Instructions string
	Tools        []Tool
	// CallTool 在独立 goroutine 中执行，可以阻塞。
	CallTool func(name string, args json.RawMessage) Result
	// OnPermissionRequest 收到 Claude Code 的权限请求时调用。
	OnPermissionRequest func(PermissionRequest)

	out   io.Writer
	outMu sync.Mutex
	log   *slog.Logger
}

func (s *Server) logger() *slog.Logger {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}

func (s *Server) write(msg rpcMessage) {
	msg.JSONRPC = "2.0"
	b, err := json.Marshal(msg)
	if err != nil {
		s.logger().Error("marshal rpc", "err", err)
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	b = append(b, '\n')
	if s.out == nil {
		return
	}
	if _, err := s.out.Write(b); err != nil {
		s.log.Error("write stdout", "err", err)
	}
}

// Notify 发送一条服务端 → 客户端的通知。
func (s *Server) Notify(method string, params any) {
	raw, err := json.Marshal(params)
	if err != nil {
		s.logger().Error("marshal notify params", "err", err)
		return
	}
	s.write(rpcMessage{Method: method, Params: raw})
}

// PushChannel 把一条外部消息推进 Claude Code 会话。meta 的 key 只能是字母数字下划线。
func (s *Server) PushChannel(content string, meta map[string]string) {
	s.Notify(methodChannel, map[string]any{"content": content, "meta": meta})
}

// SendVerdict 把远程审批结果回传给 Claude Code。
func (s *Server) SendVerdict(requestID string, allow bool) {
	behavior := "deny"
	if allow {
		behavior = "allow"
	}
	s.Notify(methodPermissionVerdict, map[string]string{"request_id": requestID, "behavior": behavior})
}

// SetOutput 指定 JSON-RPC 输出和日志（Serve 会自动调用；测试里单独用它来接管输出）。
func (s *Server) SetOutput(out io.Writer, log *slog.Logger) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if log == nil {
		log = slog.Default()
	}
	s.out = out
	s.log = log
}

// Serve 阻塞读取 stdin，直到 EOF（Claude Code 退出）。
func (s *Server) Serve(in io.Reader, out io.Writer, log *slog.Logger) error {
	s.SetOutput(out, log)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			s.logger().Warn("bad json-rpc line", "err", err)
			continue
		}
		s.dispatch(msg)
	}
	return sc.Err()
}

func (s *Server) dispatch(msg rpcMessage) {
	isRequest := len(msg.ID) > 0 && msg.Method != ""
	switch msg.Method {
	case "initialize", "ping", "tools/list", "tools/call":
		if !isRequest {
			return // JSON-RPC 通知不产生响应，也不执行需要请求 ID 的工具调用。
		}
	}
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		s.write(rpcMessage{ID: msg.ID, Result: map[string]any{
			"protocolVersion": negotiateVersion(p.ProtocolVersion),
			"capabilities": map[string]any{
				"experimental": map[string]any{
					"claude/channel":            map[string]any{},
					"claude/channel/permission": map[string]any{},
				},
				"tools": map[string]any{},
			},
			"serverInfo":   map[string]string{"name": s.Name, "version": s.Version},
			"instructions": s.Instructions,
		}})
	case "ping":
		s.write(rpcMessage{ID: msg.ID, Result: map[string]any{}})
	case "tools/list":
		s.write(rpcMessage{ID: msg.ID, Result: map[string]any{"tools": s.Tools}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			s.write(rpcMessage{ID: msg.ID, Error: &rpcError{Code: -32602, Message: err.Error()}})
			return
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					s.logger().Error("tool panic", "tool", p.Name, "panic", r)
					s.write(rpcMessage{ID: msg.ID, Result: Error(fmt.Errorf("internal error: %v", r))})
				}
			}()
			s.write(rpcMessage{ID: msg.ID, Result: s.CallTool(p.Name, p.Arguments)})
		}()
	case methodPermissionRequest:
		var p PermissionRequest
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			s.logger().Warn("bad permission_request", "err", err)
			return
		}
		if s.OnPermissionRequest != nil {
			go s.OnPermissionRequest(p)
		}
	case "notifications/initialized", "notifications/cancelled":
		// 不需要处理
	default:
		if isRequest {
			s.write(rpcMessage{ID: msg.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + msg.Method}})
		}
	}
}
