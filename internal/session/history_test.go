package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistorySummarize(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 10, 28, 20, 0, 0, 0, time.UTC)
	evs := []HistoryEvent{
		{Time: t0, Event: "connect", ClaudeSession: "aaa", Cwd: `D:\p1`, Host: "cli", Mode: "monitor"},
		{Time: t0.Add(time.Minute), Event: "disconnect", Reason: "takeover", ClaudeSession: "aaa", Messages: 3},
		{Time: t0.Add(2 * time.Minute), Event: "connect", ClaudeSession: "bbb", Cwd: `D:\p2`, Host: "desktop"},
		{Time: t0.Add(3 * time.Minute), Event: "connect", ClaudeSession: "aaa", Cwd: `D:\p1`, Host: "cli"},
		{Time: t0.Add(4 * time.Minute), Event: "disconnect", Reason: "exit", ClaudeSession: "aaa", Messages: 2},
		{Time: t0.Add(5 * time.Minute), Event: "disconnect", Reason: "manual", PID: 42}, // 只有断开、没有连接的不算
	}
	for _, ev := range evs {
		if err := AppendHistory(dir, ev); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadHistory(dir)
	if err != nil || len(got) != len(evs) {
		t.Fatalf("read %d events, err %v", len(got), err)
	}
	sum := SummarizeHistory(got)
	if len(sum) != 2 {
		t.Fatalf("entries = %+v", sum)
	}
	a := sum[0] // aaa 最近一次连接最晚，排第一
	if a.ClaudeSession != "aaa" || a.Connects != 2 || a.Messages != 5 || a.LastReason != "exit" || a.Cwd != `D:\p1` || !a.FirstConnected.Equal(t0) {
		t.Errorf("aaa = %+v", a)
	}
	if sum[1].ClaudeSession != "bbb" || sum[1].Host != "desktop" {
		t.Errorf("bbb = %+v", sum[1])
	}

	merged := MergeHistory(sum, []HistoryEntry{
		{ClaudeSession: "aaa", Cwd: "ignored", FirstConnected: t0.Add(-time.Hour), Connects: 1, Source: "transcript"},
		{ClaudeSession: "ccc", Cwd: `D:\old`, LastConnected: t0.Add(-2 * time.Hour), Connects: 1, Source: "transcript"},
	})
	if len(merged) != 3 || merged[0].Cwd != `D:\p1` || !merged[0].FirstConnected.Equal(t0.Add(-time.Hour)) || merged[2].ClaudeSession != "ccc" {
		t.Errorf("merged = %+v", merged)
	}
}

func TestReadHistoryMissing(t *testing.T) {
	evs, err := ReadHistory(t.TempDir())
	if err != nil || len(evs) != 0 {
		t.Errorf("got %v %v", evs, err)
	}
}

func TestScanTranscripts(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "D--proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, lines ...string) {
		if err := os.WriteFile(filepath.Join(proj, name), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 真正调用了 connect
	write("s1.jsonl",
		`{"type":"user","cwd":"D:\\proj","sessionId":"s1","entrypoint":"cli","timestamp":"2026-10-28T12:00:00Z","message":{"content":"连接飞书"}}`,
		`{"type":"assistant","timestamp":"2026-10-28T12:00:05Z","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__plugin_feishu_feishu__connect","input":{}}]}}`,
	)
	// 只在工具清单里出现过名字（不算），以及调用了别的工具
	write("s2.jsonl",
		`{"type":"attachment","cwd":"D:\\proj","entrypoint":"claude-desktop","attachment":{"type":"deferred_tools_record","entries":[{"name":"mcp__plugin_feishu_feishu__connect"}]}}`,
		`{"type":"assistant","timestamp":"2026-10-28T12:00:05Z","message":{"content":[{"type":"tool_use","id":"t2","name":"mcp__plugin_feishu_feishu__status","input":{}}]}}`,
	)
	got := ScanTranscripts(dir, time.Now().Add(-time.Hour))
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	e := got[0]
	if e.ClaudeSession != "s1" || e.Cwd != `D:\proj` || e.Host != "cli" || e.Connects != 1 || e.Source != "transcript" ||
		!e.LastConnected.Equal(time.Date(2026, 10, 28, 12, 0, 5, 0, time.UTC)) {
		t.Errorf("entry = %+v", e)
	}
	// 太旧的文件不读
	if got := ScanTranscripts(dir, time.Now().Add(time.Hour)); len(got) != 0 {
		t.Errorf("since filter: %+v", got)
	}

	// 文件追加后只读新增部分：写了一半的行先不算，写完再算
	connect := `{"type":"assistant","timestamp":"2026-10-28T13:00:00Z","message":{"content":[{"type":"tool_use","id":"t3","name":"mcp__plugin_feishu_feishu__connect","input":{}}]}}`
	f, err := os.OpenFile(filepath.Join(proj, "s1.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	half := len(connect) / 2
	if _, err := f.WriteString(connect[:half]); err != nil {
		t.Fatal(err)
	}
	if got := ScanTranscripts(dir, time.Now().Add(-time.Hour)); got[0].Connects != 1 {
		t.Fatalf("half line counted: %+v", got[0])
	}
	if _, err := f.WriteString(connect[half:] + "\n"); err != nil {
		t.Fatal(err)
	}
	got = ScanTranscripts(dir, time.Now().Add(-time.Hour))
	if got[0].Connects != 2 || !got[0].LastConnected.Equal(time.Date(2026, 10, 28, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("appended connect missed: %+v", got[0])
	}
}
