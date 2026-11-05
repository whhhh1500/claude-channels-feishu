package feishu

// 文本处理：长消息分段（不切断代码块）、标题降级、@占位符清理、标签属性转义。

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// SplitText 把长文本按字符数切块，优先在空行、换行处断开，避免切断代码块。
func SplitText(s string, limit int) []string {
	if utf8.RuneCountInString(s) <= limit {
		return []string{s}
	}
	var out []string
	rest := []rune(s)
	inFence := false
	for len(rest) > limit {
		cut := lastBreak(rest[:limit])
		if cut <= limit/2 {
			cut = limit
		}
		chunk := string(rest[:cut])
		rest = rest[cut:]
		// 代码块被切开时，在本块末尾补上 ``` 并在下一块开头重新打开。
		fences := strings.Count(chunk, "```")
		open := inFence
		if fences%2 == 1 {
			inFence = !inFence
		}
		if open {
			chunk = "```\n" + chunk
		}
		if inFence {
			chunk += "\n```"
		}
		out = append(out, strings.TrimRight(chunk, "\n"))
	}
	last := string(rest)
	if inFence {
		last = "```\n" + last
	}
	if strings.TrimSpace(last) != "" {
		out = append(out, last)
	}
	return out
}

func lastBreak(r []rune) int {
	s := string(r)
	for _, sep := range []string{"\n\n", "\n", "。", ". ", " "} {
		if i := strings.LastIndex(s, sep); i > 0 {
			return utf8.RuneCountInString(s[:i+len(sep)])
		}
	}
	return len(r)
}

// OptimizeMarkdown 把 markdown 调整成飞书好看的样子：
// 飞书 md 的一二级标题字号很大，统一降成四、五级（代码块里的不动）。
func OptimizeMarkdown(md string) string {
	lines := strings.Split(md, "\n")
	inFence := false
	for i, l := range lines {
		t := strings.TrimLeft(l, " ")
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		if inFence || !strings.HasPrefix(t, "#") {
			continue
		}
		level := len(t) - len(strings.TrimLeft(t, "#"))
		if level > 6 || len(t) <= level || t[level] != ' ' {
			continue
		}
		newLevel := 5
		if level == 1 {
			newLevel = 4
		}
		lines[i] = strings.Repeat("#", newLevel) + t[level:]
	}
	return strings.Join(lines, "\n")
}

var mentionPlaceholder = regexp.MustCompile(`@_user_\d+\s*`)

// StripMentions 去掉飞书文本里的 @_user_N 占位符。
func StripMentions(s string) string {
	return strings.TrimSpace(mentionPlaceholder.ReplaceAllString(s, ""))
}

// SafeAttr 清理会放进 <channel> 标签属性的外部字符串，防止伪造标签。
func SafeAttr(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', '"', '\r', '\n', '[', ']', ';':
			return '_'
		}
		return r
	}, s)
}

func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func MDEscape(s string) string {
	return strings.NewReplacer("*", `\*`, "_", `\_`, "`", "ˋ", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ---------------------------------------------------------------- 工具
