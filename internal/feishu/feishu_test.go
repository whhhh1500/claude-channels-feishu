package feishu

import (
	"strings"
	"testing"
)

func TestSplitTextKeepsFences(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("intro\n```go\n")
	for i := 0; i < 400; i++ {
		sb.WriteString("fmt.Println(\"hello world\")\n")
	}
	sb.WriteString("```\noutro")
	parts := SplitText(sb.String(), 4000)
	if len(parts) < 2 {
		t.Fatalf("expected split, got %d", len(parts))
	}
	for i, p := range parts {
		if strings.Count(p, "```")%2 != 0 {
			t.Errorf("part %d has unbalanced fences", i)
		}
		if n := len([]rune(p)); n > 4000+10 {
			t.Errorf("part %d too long: %d", i, n)
		}
	}
	if SplitText("short", 10)[0] != "short" {
		t.Error("short text should be unchanged")
	}
}

func TestRenderPost(t *testing.T) {
	flat := `{"title":"标题","content":[[{"tag":"text","text":"加粗","style":["bold"]},{"tag":"a","text":"链接","href":"https://x.y"}],[{"tag":"at","user_id":"@_user_1"},{"tag":"img","image_key":"img_1"}],[{"tag":"code_block","language":"GO","text":"x := 1\n"}]]}`
	text, atts := RenderPost(flat)
	for _, want := range []string{"**标题**", "**加粗**", "[链接](https://x.y)", "@_user_1", "```go\nx := 1\n```"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
	if len(atts) != 1 || atts[0].Key != "img_1" {
		t.Errorf("attachments = %+v", atts)
	}
	wrapped := `{"zh_cn":{"title":"","content":[[{"tag":"text","text":"hi"}]]}}`
	if text, _ := RenderPost(wrapped); text != "hi" {
		t.Errorf("wrapped post = %q", text)
	}
}

func TestOptimizeMarkdown(t *testing.T) {
	in := "# A\n## B\n```\n# not heading\n```\n#hashtag"
	want := "#### A\n##### B\n```\n# not heading\n```\n#hashtag"
	if got := OptimizeMarkdown(in); got != want {
		t.Errorf("got %q", got)
	}
}
