package bridge

// 插件 hook 转来的请求：授权（PermissionRequest）、选择题（AskUserQuestion）、计划确认（ExitPlanMode）。
// 发飞书卡片，等用户操作，再把结果翻译成 hook 的输出。

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/whhhh1500/claude-channels-feishu/internal/access"
	"github.com/whhhh1500/claude-channels-feishu/internal/feishu"
	"github.com/whhhh1500/claude-channels-feishu/internal/session"
)

type hookInput struct {
	HookEventName      string          `json:"hook_event_name"`
	ToolName           string          `json:"tool_name"`
	ToolInput          json.RawMessage `json:"tool_input"`
	PermissionSuggests json.RawMessage `json:"permission_suggestions"`
	SessionID          string          `json:"session_id"`
	Cwd                string          `json:"cwd"`
	PermissionMode     string          `json:"permission_mode"`
	TranscriptPath     string          `json:"transcript_path"`
	Prompt             string          `json:"prompt"`
}

type askQuestion struct {
	Question    string `json:"question"`
	Header      string `json:"header"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

type hookRequest struct {
	id     string
	kind   string // permission / question / plan
	in     hookInput
	cards  []string
	result chan string // hook 要打印的 stdout，空串表示不做决定
	done   bool
	label  string // 处理结果，用于改卡片
}

// HandleHook 由控制端口调用，阻塞到用户在飞书上处理完或超时。返回 hook 的 stdout（空串=不做决定）。
func (b *Bridge) HandleHook(raw json.RawMessage) string {
	return b.handleHook(context.Background(), raw)
}

func (b *Bridge) handleHook(ctx context.Context, raw json.RawMessage) string {
	ctx, cancel := context.WithTimeout(ctx, session.HookWait)
	defer cancel()
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return ""
	}
	if in.HookEventName == "Stop" {
		// 回合结束：把 r📨 段落发到飞书。后台发，不耽误 Claude。
		b.mu.Lock()
		b.stopHook = true
		b.mu.Unlock()
		go b.onTurnEnd(in.TranscriptPath)
		return ""
	}
	if in.HookEventName == "UserPromptSubmit" {
		// 电脑上打的字立刻同步到飞书（发送在后台进行）
		b.onPromptSubmit(in.Prompt)
		return ""
	}
	kind := ""
	switch {
	case in.HookEventName == "PermissionRequest":
		kind = "permission"
		b.mu.Lock()
		b.hookSeen = true
		b.mu.Unlock()
	case in.HookEventName == "PreToolUse" && in.ToolName == "AskUserQuestion":
		kind = "question"
	case in.HookEventName == "PreToolUse" && in.ToolName == "ExitPlanMode":
		kind = "plan"
	default:
		return ""
	}
	// 开关关着：照常在电脑上处理。
	if b.conn == nil || !b.conn.Active() {
		return ""
	}
	acc, err := b.access.Load()
	if err != nil || len(acc.AllowFrom) == 0 {
		return ""
	}

	req := &hookRequest{id: access.NewPairCode(), kind: kind, in: in, result: make(chan string, 1)}
	card, err := hookCard(req)
	if err != nil {
		b.log.Warn("build hook card", "err", err)
		return ""
	}
	b.mu.Lock()
	if b.hooks == nil {
		b.hooks = map[string]*hookRequest{}
	}
	b.hooks[req.id] = req
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.hooks, req.id)
		b.mu.Unlock()
	}()

	for _, openID := range acc.AllowFrom {
		if ctx.Err() != nil {
			break
		}
		id, err := b.client().SendCardToUser(ctx, openID, card)
		if err != nil {
			b.log.Warn("send hook card", "err", err)
			continue
		}
		b.own(id)
		b.mu.Lock()
		req.cards = append(req.cards, id)
		done, label := req.done, req.label
		b.mu.Unlock()
		// 用户可能在其余卡片还没发完时已处理第一张。
		if done {
			if err := b.client().PatchCard(context.Background(), id, resolvedCard(req, label)); err != nil {
				b.log.Debug("patch late hook card", "err", err)
			}
		}
	}
	if len(req.cards) == 0 {
		return ""
	}
	b.log.Info("hook card sent", "kind", kind, "tool", in.ToolName, "id", req.id)

	select {
	case out := <-req.result:
		return out
	case <-ctx.Done():
		b.finishHook(req, "⏱ 已超时，请回电脑上处理", "")
		return <-req.result
	}
}

// finishHook 标记完成并把所有卡片改成最终状态（在卡片回调返回之后异步执行）。返回 false 表示已经处理过。
func (b *Bridge) finishHook(req *hookRequest, label, out string) bool {
	b.mu.Lock()
	if req.done {
		b.mu.Unlock()
		return false
	}
	req.done, req.label = true, label
	req.result <- out // 完成状态和结果同时发布，避免超时分支丢掉已提交的决定。
	cards := slices.Clone(req.cards)
	b.mu.Unlock()
	card := resolvedCard(req, label)
	go func() {
		time.Sleep(500 * time.Millisecond)
		for _, mid := range cards {
			if err := b.client().PatchCard(context.Background(), mid, card); err != nil {
				b.log.Debug("patch hook card", "err", err)
			}
		}
	}()
	return true
}

// onHookCardAction 处理 hook 卡片上的按钮（3 秒内返回）。
func (b *Bridge) onHookCardAction(ev *callback.CardActionTriggerEvent) *callback.CardActionTriggerResponse {
	v := ev.Event.Action.Value
	id, _ := v["id"].(string)
	action, _ := v["action"].(string)
	b.mu.Lock()
	req := b.hooks[id]
	done := req != nil && req.done
	b.mu.Unlock()
	if req == nil {
		return toast("warning", "这个请求已失效（已超时、已在电脑上处理，或会话已重启）")
	}
	if done {
		return toast("warning", "这个请求已经处理过了")
	}
	form := ev.Event.Action.FormValue
	out, label, err := hookAnswer(req, action, form)
	if err != nil {
		return toast("error", err.Error())
	}
	if !b.finishHook(req, label, out) {
		return toast("warning", "这个请求已经处理过了")
	}
	b.log.Info("hook answered", "id", id, "action", action, "by", ev.Event.Operator.OpenID)

	var data map[string]any
	_ = json.Unmarshal([]byte(resolvedCard(req, label)), &data)
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "已提交"},
		Card:  &callback.Card{Type: "raw", Data: data},
	}
}

func formString(form map[string]any, key string) string {
	switch x := form[key].(type) {
	case string:
		return strings.TrimSpace(x)
	case []any:
		var parts []string
		for _, p := range x {
			if s, ok := p.(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

func hookJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// hookAnswer 把卡片操作翻译成 hook 的 stdout。
func hookAnswer(req *hookRequest, action string, form map[string]any) (out, label string, err error) {
	reason := formString(form, "reason")
	switch req.kind {
	case "permission":
		decision := map[string]any{}
		switch action {
		case "allow":
			decision["behavior"], label = "allow", "✅ 已允许"
		case "always":
			decision["behavior"], label = "allow", "✅ 已允许，以后不再询问"
			var sugg []any
			if json.Unmarshal(req.in.PermissionSuggests, &sugg) == nil && len(sugg) > 0 {
				decision["updatedPermissions"] = sugg
			}
		case "deny":
			decision["behavior"], label = "deny", "❌ 已拒绝"
			msg := "用户在飞书上拒绝了这次操作。"
			if reason != "" {
				msg += "用户的说明：" + reason
				label += "：" + feishu.Truncate(reason, 200)
			}
			decision["message"] = msg
		case "skip":
			return "", "↩ 已转回电脑上处理", nil
		default:
			return "", "", fmt.Errorf("无效操作")
		}
		return hookJSON(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest", "decision": decision}}), label, nil

	case "question":
		if action == "skip" {
			return "", "↩ 已转回电脑上回答", nil
		}
		if action != "submit" {
			return "", "", fmt.Errorf("无效操作")
		}
		var input map[string]any
		if err := json.Unmarshal(req.in.ToolInput, &input); err != nil || input == nil {
			return "", "", fmt.Errorf("无效问题输入")
		}
		qs := questionsOf(req.in.ToolInput)
		if len(qs) == 0 {
			return "", "", fmt.Errorf("没有问题")
		}
		answers := map[string]any{}
		var shown []string
		for i, q := range qs {
			ans := formString(form, fmt.Sprintf("other%d", i))
			if ans == "" {
				ans = formString(form, fmt.Sprintf("q%d", i))
			}
			if ans == "" {
				return "", "", fmt.Errorf("第 %d 题还没选", i+1)
			}
			answers[q.Question] = ans
			shown = append(shown, fmt.Sprintf("%s → %s", q.Header, ans))
		}
		input["answers"] = answers
		return hookJSON(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PreToolUse", "permissionDecision": "allow", "updatedInput": input}}),
			"✅ 已回答：" + feishu.Truncate(strings.Join(shown, "；"), 300), nil

	case "plan":
		var input map[string]any
		_ = json.Unmarshal(req.in.ToolInput, &input)
		switch action {
		case "approve":
			return hookJSON(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PreToolUse", "permissionDecision": "allow", "updatedInput": input}}), "✅ 已批准计划", nil
		case "revise":
			msg := "用户在飞书上要求继续修改计划，暂不执行。"
			if reason != "" {
				msg += "修改意见：" + reason
			}
			return hookJSON(map[string]any{"hookSpecificOutput": map[string]any{
					"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": msg}}),
				"✏️ 已要求修改" + map[bool]string{true: "：" + feishu.Truncate(reason, 200), false: ""}[reason != ""], nil
		case "skip":
			return "", "↩ 已转回电脑上处理", nil
		}
	}
	return "", "", fmt.Errorf("无效操作")
}

func questionsOf(raw json.RawMessage) []askQuestion {
	var in struct {
		Questions []askQuestion `json:"questions"`
	}
	_ = json.Unmarshal(raw, &in)
	return in.Questions
}

// ---------------------------------------------------------------- 卡片
