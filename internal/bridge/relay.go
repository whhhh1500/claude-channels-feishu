package bridge

// Channel 自带的权限转发（只在带 channel 参数、且插件 hook 没在工作时用）：审批卡片和 y/n 文字回复。

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/whhhh1500/claude-channels-feishu/internal/feishu"
	"github.com/whhhh1500/claude-channels-feishu/internal/mcp"
)

type permEntry struct {
	req     mcp.PermissionRequest
	cards   []string // 已发出的审批卡片消息 ID
	decided string   // "", "allow", "deny"
	at      time.Time
}

func (b *Bridge) OnPermissionRequest(req mcp.PermissionRequest) {
	// 开关关着就不往飞书发审批（按钮回调也收不到）。
	if b.conn == nil || !b.conn.Active() {
		return
	}
	// 插件的 PermissionRequest hook 在工作时由它发更完整的审批卡片，这里不重复发。
	b.mu.Lock()
	hookSeen := b.hookSeen
	b.mu.Unlock()
	if hookSeen {
		return
	}
	acc, err := b.access.Load()
	if err != nil || len(acc.AllowFrom) == 0 {
		return
	}
	entry := &permEntry{req: req, at: time.Now()}
	b.mu.Lock()
	b.perms[req.RequestID] = entry
	// 清理一小时前的旧记录
	for id, p := range b.perms {
		if time.Since(p.at) > time.Hour {
			delete(b.perms, id)
		}
	}
	b.mu.Unlock()

	card := permissionCard(req, "")
	ctx := context.Background()
	for _, openID := range acc.AllowFrom {
		id, err := b.client().SendCardToUser(ctx, openID, card)
		if err != nil {
			b.log.Warn("send permission card", "open_id", openID, "err", err)
			continue
		}
		b.own(id)
		b.mu.Lock()
		entry.cards = append(entry.cards, id)
		b.mu.Unlock()
	}
}

// resolvePermission 回传审批结果并把所有审批卡片改成"已处理"。返回 false 表示已处理过或不存在。
func (b *Bridge) resolvePermission(id string, allow bool, by string) (ok bool, known bool) {
	b.mu.Lock()
	p, exists := b.perms[id]
	if !exists {
		b.mu.Unlock()
		// 重启后丢了记录：照样把结果交给 Claude Code，它会校验 ID。
		b.mcp.SendVerdict(id, allow)
		return true, false
	}
	if p.decided != "" {
		b.mu.Unlock()
		return false, true
	}
	p.decided = "deny"
	if allow {
		p.decided = "allow"
	}
	cards := slices.Clone(p.cards)
	req, decided := p.req, p.decided
	b.mu.Unlock()

	b.mcp.SendVerdict(id, allow)
	b.log.Info("permission verdict", "request_id", id, "allow", allow, "by", by)

	// 在卡片回调返回之后再改卡片，对所有人生效。
	go func() {
		time.Sleep(500 * time.Millisecond)
		card := permissionCard(req, decided)
		for _, mid := range cards {
			if err := b.client().PatchCard(context.Background(), mid, card); err != nil {
				b.log.Debug("patch permission card", "err", err)
			}
		}
	}()
	return true, true
}

// OnCardAction 处理审批卡片按钮。必须 3 秒内返回。
// 多个会话在线时它只在负责接收的会话（hub）上触发：选会话卡片由 hub 处理，审批卡片转给发卡片的会话。
func (b *Bridge) OnCardAction(_ context.Context, ev *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if ev == nil || ev.Event == nil || ev.Event.Action == nil || ev.Event.Operator == nil {
		return toast("error", "无效操作"), nil
	}
	eventID := ""
	if ev.EventV2Base != nil && ev.EventV2Base.Header != nil {
		eventID = ev.EventV2Base.Header.EventID
	}
	if eventID != "" && !b.markSeen("card:"+eventID) {
		return toast("info", "处理中"), nil
	}
	acc, err := b.access.Load()
	if err != nil || !slices.Contains(acc.AllowFrom, ev.Event.Operator.OpenID) {
		return toast("error", "你没有审批权限"), nil
	}
	if kind, _ := ev.Event.Action.Value["kind"].(string); kind == "route" {
		return b.onRouteCardAction(ev), nil
	}
	if resp := b.forwardCardAction(ev); resp != nil {
		return resp, nil
	}
	return b.localCardAction(ev), nil
}

// localCardAction 处理本会话发出的审批卡片按钮（hub 转来的也走这里）。
func (b *Bridge) localCardAction(ev *callback.CardActionTriggerEvent) *callback.CardActionTriggerResponse {
	acc, err := b.access.Load()
	if err != nil || !slices.Contains(acc.AllowFrom, ev.Event.Operator.OpenID) {
		return toast("error", "你没有审批权限")
	}
	if kind, _ := ev.Event.Action.Value["kind"].(string); kind == "hook" {
		return b.onHookCardAction(ev)
	}
	action, _ := ev.Event.Action.Value["perm"].(string)
	id, _ := ev.Event.Action.Value["id"].(string)
	if (action != "allow" && action != "deny") || !regexp.MustCompile(`^[a-km-z]{5}$`).MatchString(id) {
		return toast("error", "无效操作")
	}
	allow := action == "allow"
	ok, known := b.resolvePermission(id, allow, ev.Event.Operator.OpenID)
	if !ok {
		return toast("warning", "这个请求已经处理过了")
	}
	msg := "已允许"
	if !allow {
		msg = "已拒绝"
	}
	if !known {
		msg += "（如果终端已处理，此操作会被忽略）"
		return toast("success", msg)
	}
	b.mu.Lock()
	p := b.perms[id]
	if p == nil {
		b.mu.Unlock()
		return toast("success", msg)
	}
	req := p.req
	b.mu.Unlock()
	var data map[string]any
	_ = json.Unmarshal([]byte(permissionCard(req, action)), &data)
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: msg},
		Card:  &callback.Card{Type: "raw", Data: data},
	}
}

func permissionCard(req mcp.PermissionRequest, decided string) string {
	preview := req.InputPreview
	var pretty any
	if json.Unmarshal([]byte(preview), &pretty) == nil {
		if bb, err := json.MarshalIndent(pretty, "", "  "); err == nil {
			preview = string(bb)
		}
	}
	preview = strings.ReplaceAll(feishu.Truncate(preview, 2500), "```", "ˋˋˋ")
	md := fmt.Sprintf("**工具**：%s\n**说明**：%s\n```json\n%s\n```",
		feishu.MDEscape(req.ToolName), feishu.MDEscape(req.Description), preview)

	elements := []any{map[string]any{"tag": "markdown", "content": md}}
	template, title := "orange", "🔐 Claude 请求权限"
	switch decided {
	case "allow":
		template, title = "green", "✅ 已允许"
	case "deny":
		template, title = "red", "❌ 已拒绝"
	default:
		btn := func(label, typ, action string) map[string]any {
			return map[string]any{
				"tag": "column", "width": "auto",
				"elements": []any{map[string]any{
					"tag": "button", "type": typ,
					"text":      map[string]any{"tag": "plain_text", "content": label},
					"behaviors": []any{map[string]any{"type": "callback", "value": map[string]any{"perm": action, "id": req.RequestID, "pid": selfPID}}},
				}},
			}
		}
		elements = append(elements,
			map[string]any{"tag": "column_set", "columns": []any{btn("允许", "primary", "allow"), btn("拒绝", "danger", "deny")}},
			map[string]any{"tag": "markdown", "content": fmt.Sprintf("<font color='grey'>也可以回复 y %s 或 n %s</font>", req.RequestID, req.RequestID), "text_size": "notation"},
		)
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true, "width_mode": "fill"},
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": title}, "template": template},
		"body":   map[string]any{"elements": elements},
	}
	bb, _ := json.Marshal(card)
	return string(bb)
}
