package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Bash / PowerShell 工具的 stdout 是文件，Monitor 的是管道：只有文件要拒绝。
func TestIsRegularFile(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "task.output"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !isRegularFile(f) {
		t.Error("普通文件应判为文件")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isRegularFile(w) {
		t.Error("管道不应判为文件")
	}
}

func TestCanShutdown(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0.2.0": false, "0.1.9": false, "0.3.0": true, "1.0.0": true} {
		if got := canShutdown(v); got != want {
			t.Errorf("canShutdown(%q) = %v", v, got)
		}
	}
}

// 插件进程比磁盘上的程序新（刚启动）：不提示，也不去连它
func TestOutdatedNoteFresh(t *testing.T) {
	if note := outdatedNote(&Info{Started: time.Now().Add(time.Hour), Port: 1}); note != "" {
		t.Fatalf("note = %q", note)
	}
}
