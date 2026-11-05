package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// 用管道驱动 MCP 服务端，验证握手、工具列表、工具调用和自定义通知。
func TestMCPProtocol(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	got := make(chan PermissionRequest, 1)
	s := &Server{
		Name: "feishu", Version: "test", Instructions: "hi", Tools: testTools,
		CallTool:            func(name string, args json.RawMessage) Result { return Text("called %s", name) },
		OnPermissionRequest: func(p PermissionRequest) { got <- p },
	}
	go s.Serve(inR, outW, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rd := bufio.NewReader(outR)
	send := func(v string) { io.WriteString(inW, v+"\n") }
	recv := func() map[string]any {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad json %q", line)
		}
		return m
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	init := recv()
	caps := init["result"].(map[string]any)["capabilities"].(map[string]any)
	exp := caps["experimental"].(map[string]any)
	if _, ok := exp["claude/channel"]; !ok {
		t.Error("missing claude/channel capability")
	}
	if _, ok := exp["claude/channel/permission"]; !ok {
		t.Error("missing permission capability")
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := recv()
	if n := len(list["result"].(map[string]any)["tools"].([]any)); n != len(testTools) {
		t.Errorf("tools = %d", n)
	}
	send(`{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"reply","arguments":{"chat_id":"c","text":"t"}}}`)
	call := recv()
	if call["id"] != "x" || !strings.Contains(call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "called reply") {
		t.Errorf("call = %v", call)
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"bogus"}`)
	if e := recv()["error"]; e == nil {
		t.Error("expected method-not-found")
	}
	send(`{"jsonrpc":"2.0","method":"notifications/claude/channel/permission_request","params":{"request_id":"abcde","tool_name":"Bash","description":"d","input_preview":"{}"}}`)
	select {
	case p := <-got:
		if p.RequestID != "abcde" || p.ToolName != "Bash" {
			t.Errorf("perm = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("permission request not delivered")
	}

	go s.PushChannel("hello", map[string]string{"chat_id": "oc_1"})
	n := recv()
	if n["method"] != "notifications/claude/channel" || n["params"].(map[string]any)["content"] != "hello" {
		t.Errorf("push = %v", n)
	}
	go s.SendVerdict("abcde", true)
	v := recv()
	if v["method"] != "notifications/claude/channel/permission" || v["params"].(map[string]any)["behavior"] != "allow" {
		t.Errorf("verdict = %v", v)
	}
	inW.Close()
}

var testTools = []Tool{
	{Name: "reply", Description: "测试", InputSchema: map[string]any{"type": "object"}},
	{Name: "connect", Description: "测试", InputSchema: map[string]any{"type": "object"}},
}

func TestNegotiateVersion(t *testing.T) {
	cases := map[string]string{
		"":           "2025-06-18",
		"2025-06-18": "2025-06-18",
		"2025-11-25": "2025-11-25",
		"2026-07-28": maxProtocolVersion, // 新协议下 Channel 推送不生效，不答应
	}
	for in, want := range cases {
		if got := negotiateVersion(in); got != want {
			t.Errorf("negotiateVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
