package bridge

import (
	"os"
	"strings"
	"testing"
	"time"
)

// setTranscriptID 测试里临时换会话 ID（加锁：New 起的后台协程会读它）
func setTranscriptID(t *testing.T, id string) {
	transcript.mu.Lock()
	old := transcript.id
	transcript.id = id
	transcript.mu.Unlock()
	t.Cleanup(func() {
		transcript.mu.Lock()
		transcript.id = old
		transcript.mu.Unlock()
	})
}

// 关闭插件：连着飞书时留下记录并退出；新进程读到记录，沿用原来的监听地址
func TestShutdownAndResume(t *testing.T) {
	setTranscriptID(t, "test-session")

	b, _ := testBridge(t)
	if r := b.toolShutdown(); !r.IsError {
		t.Fatal("shutdown without exit func should fail")
	}
	ctl, err := StartControl(b, b.log)
	if err != nil {
		t.Fatal(err)
	}
	url := b.listenURL()
	exited := make(chan struct{})
	b.SetOnExit(func() { ctl.Close(); close(exited) })
	setOnline(b)
	b.setMode("monitor")

	r := b.toolShutdown()
	if r.IsError || !strings.Contains(r.Content[0].Text, "status") || !strings.Contains(r.Content[0].Text, "原来的参数") {
		t.Fatalf("shutdown result: %+v", r)
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("did not exit")
	}

	// 同一个会话里重新拉起的新进程
	b2 := New(b.cfg, b.log, b.fs, b.mcp)
	b2.LoadRestart()
	if b2.restart == nil || b2.restart.Mode != "monitor" {
		t.Fatalf("restart mark = %+v", b2.restart)
	}
	if _, err := os.Stat(restartMarkPath(b.cfg.StateDir, "test-session")); !os.IsNotExist(err) {
		t.Error("restart mark not removed")
	}
	ctl2, err := StartControl(b2, b2.log)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl2.Close()
	if got := b2.listenURL(); got != url {
		t.Errorf("listen url changed: %s → %s", url, got)
	}

	// 没连飞书时关闭：不留记录
	b3 := New(b.cfg, b.log, b.fs, b.mcp)
	b3.SetOnExit(func() {})
	b3.toolShutdown()
	if b3.takeRestartMark() != nil {
		t.Error("mark written while offline")
	}
}

// 过期的记录不算
func TestRestartMarkExpired(t *testing.T) {
	setTranscriptID(t, "test-session")
	b, _ := testBridge(t)
	stale := `{"mode":"monitor","listen":"","at":"` + time.Now().Add(-time.Hour).Format(time.RFC3339) + `"}`
	if err := os.WriteFile(restartMarkPath(b.cfg.StateDir, "test-session"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := b.takeRestartMark(); m != nil {
		t.Fatalf("stale mark used: %+v", m)
	}
}
