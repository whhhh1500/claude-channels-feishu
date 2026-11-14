package session

import (
	"os"
	"path/filepath"
	"testing"
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
