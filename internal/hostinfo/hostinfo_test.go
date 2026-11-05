package hostinfo

import (
	"strings"
	"testing"
)

func TestClassifyHost(t *testing.T) {
	t.Setenv("CLAUDE_CODE_DESKTOP_APP_VERSION", "")
	cases := map[string]string{
		`c:\users\me\appdata\roaming\claude\claude-code\2.1.309\abc\claude.exe --output-format stream-json --verbose --input-format stream-json --permission-prompt-tool=stdio`: "desktop",
		`"d:\nodejs\node_modules\@anthropic-ai\claude-code\bin\claude.exe" --dangerously-load-development-channels plugin:feishu@claude-channels`:                               "cli",
		`claude --resume abc`:                 "cli",
		`claude -p "hi" --output-format json`: "sdk",
		`/usr/local/bin/claude --input-format stream-json --output-format stream-json`: "sdk",
	}
	for cmd, want := range cases {
		if got := Classify(strings.ToLower(cmd)); got != want {
			t.Errorf("%s => %s, want %s", cmd, got, want)
		}
	}
}
