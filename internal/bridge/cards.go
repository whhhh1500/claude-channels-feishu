package bridge

// 飞书卡片（JSON 2.0）的拼装：hook 用的授权、选择题、计划卡片，以及处理后的状态卡片。

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/whhhh1500/claude-channels-feishu/internal/feishu"
)

func plain(s string) map[string]any { return map[string]any{"tag": "plain_text", "content": s} }

func md(s string) map[string]any { return map[string]any{"tag": "markdown", "content": s} }

func formButton(name, label, typ, id, action string) map[string]any {
	return map[string]any{
		"tag": "button", "name": name, "type": typ, "text": plain(label),
		"form_action_type": "submit",
		"behaviors":        []any{map[string]any{"type": "callback", "value": map[string]any{"kind": "hook", "id": id, "action": action, "pid": selfPID}}},
	}
}

func buttonRow(btns ...map[string]any) map[string]any {
	var cols []any
	for _, b := range btns {
		cols = append(cols, map[string]any{"tag": "column", "width": "auto", "elements": []any{b}})
	}
	return map[string]any{"tag": "column_set", "columns": cols}
}

func cardJSON(title, template string, elements []any) string {
	b, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true, "width_mode": "fill"},
		"header": map[string]any{"title": plain(title), "template": template},
		"body":   map[string]any{"elements": elements},
	})
	return string(b)
}

func codeBlock(s string, limit int) string {
	return "```\n" + strings.ReplaceAll(feishu.Truncate(s, limit), "```", "ˋˋˋ") + "\n```"
}

func prettyJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) == nil {
		if b, err := json.MarshalIndent(v, "", "  "); err == nil {
			return string(b)
		}
	}
	return string(raw)
}

// hookSummary 是卡片上方的说明部分，处理前后都显示。
func hookSummary(req *hookRequest) (title string, elements []any) {
	defer func() { title = titleWithTag(title) }()
	switch req.kind {
	case "permission":
		var ti map[string]any
		_ = json.Unmarshal(req.in.ToolInput, &ti)
		desc, _ := ti["description"].(string)
		body := fmt.Sprintf("**工具**：%s", feishu.MDEscape(req.in.ToolName))
		if desc != "" {
			body += "\n**说明**：" + feishu.MDEscape(desc)
		}
		if cmd, ok := ti["command"].(string); ok {
			body += "\n" + codeBlock(cmd, 2500)
		} else {
			body += "\n" + codeBlock(prettyJSON(req.in.ToolInput), 2500)
		}
		return "🔐 Claude 请求权限", []any{md(body)}
	case "question":
		var els []any
		for i, q := range questionsOf(req.in.ToolInput) {
			mode := "单选"
			if q.MultiSelect {
				mode = "多选"
			}
			var opts []string
			for _, o := range q.Options {
				line := "· **" + feishu.MDEscape(o.Label) + "**"
				if o.Description != "" {
					line += "：" + feishu.MDEscape(o.Description)
				}
				opts = append(opts, line)
			}
			els = append(els, md(fmt.Sprintf("**%d. %s**（%s）\n%s", i+1, feishu.MDEscape(q.Question), mode, strings.Join(opts, "\n"))))
		}
		return "❓ Claude 想问你", els
	case "plan":
		var ti map[string]any
		_ = json.Unmarshal(req.in.ToolInput, &ti)
		plan, _ := ti["plan"].(string)
		if plan == "" {
			plan = "（计划内容为空）"
		}
		return "📋 Claude 的计划，等你确认", []any{md(feishu.OptimizeMarkdown(feishu.Truncate(plan, 3500)))}
	}
	return "Claude", nil
}

func hookCard(req *hookRequest) (string, error) {
	title, els := hookSummary(req)
	var form []any
	switch req.kind {
	case "permission":
		form = append(form,
			map[string]any{"tag": "input", "name": "reason", "max_length": 500,
				"placeholder": plain("拒绝时可以写原因，Claude 会照着改（可选）")},
		)
		btns := []map[string]any{formButton("allow", "允许", "primary", req.id, "allow")}
		var sugg []any
		if json.Unmarshal(req.in.PermissionSuggests, &sugg) == nil && len(sugg) > 0 {
			btns = append(btns, formButton("always", "总是允许", "default", req.id, "always"))
		}
		btns = append(btns, formButton("deny", "拒绝", "danger", req.id, "deny"), formButton("skip", "回电脑处理", "laser", req.id, "skip"))
		form = append(form, buttonRow(btns...))
	case "question":
		qs := questionsOf(req.in.ToolInput)
		if len(qs) == 0 {
			return "", fmt.Errorf("没有问题")
		}
		for i, q := range qs {
			var opts []any
			for _, o := range q.Options {
				opts = append(opts, map[string]any{"text": plain(o.Label), "value": o.Label})
			}
			tag := "select_static"
			if q.MultiSelect {
				tag = "multi_select_static"
			}
			form = append(form,
				md(fmt.Sprintf("**%d. %s**", i+1, feishu.MDEscape(q.Header))),
				map[string]any{"tag": tag, "name": fmt.Sprintf("q%d", i), "placeholder": plain("选择"), "options": opts, "width": "fill"},
				map[string]any{"tag": "input", "name": fmt.Sprintf("other%d", i), "max_length": 500,
					"placeholder": plain("其他（填了就以这里为准）")},
			)
		}
		form = append(form, buttonRow(
			formButton("submit", "提交", "primary", req.id, "submit"),
			formButton("skip", "回电脑回答", "laser", req.id, "skip")))
	case "plan":
		form = append(form,
			map[string]any{"tag": "input", "name": "reason", "max_length": 1000,
				"placeholder": plain("要修改的话写下意见（可选）")},
			buttonRow(
				formButton("approve", "批准", "primary", req.id, "approve"),
				formButton("revise", "继续修改", "default", req.id, "revise"),
				formButton("skip", "回电脑处理", "laser", req.id, "skip")),
		)
	}
	els = append(els,
		map[string]any{"tag": "form", "name": "hook_form", "elements": form},
		map[string]any{"tag": "markdown", "content": "<font color='grey'>超过 9 分钟没处理会转回电脑上。</font>", "text_size": "notation"},
	)
	return cardJSON(title, "orange", els), nil
}

func resolvedCard(req *hookRequest, label string) string {
	title, els := hookSummary(req)
	template := "green"
	switch {
	case strings.HasPrefix(label, "❌"):
		template = "red"
	case strings.HasPrefix(label, "↩"), strings.HasPrefix(label, "⏱"):
		template = "grey"
	case strings.HasPrefix(label, "✏️"):
		template = "yellow"
	}
	els = append(els, map[string]any{"tag": "hr"}, md("**"+feishu.MDEscape(label)+"**"))
	return cardJSON(title, template, els)
}
