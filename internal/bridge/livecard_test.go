package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/whhhh1500/claude-channels-feishu/internal/config"
	"github.com/whhhh1500/claude-channels-feishu/internal/feishu"
)

// 真实发卡片验证格式：FEISHU_LIVE_OPEN_ID=ou_xxx go test -run TestLiveCards
func TestLiveCards(t *testing.T) {
	openID := os.Getenv("FEISHU_LIVE_OPEN_ID")
	if openID == "" {
		t.Skip("set FEISHU_LIVE_OPEN_ID to run")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	fs := feishu.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reqs := []*hookRequest{
		{id: "testaa", kind: "permission", in: hookInput{ToolName: "Bash",
			ToolInput:          json.RawMessage(`{"command":"echo 测试审批卡片","description":"【测试】演示三选项审批卡片"}`),
			PermissionSuggests: json.RawMessage(`[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"echo"}],"behavior":"allow","destination":"localSettings"}]`)}},
		{id: "testbb", kind: "question", in: hookInput{ToolName: "AskUserQuestion", ToolInput: json.RawMessage(`{"questions":[
			{"question":"【测试】用哪个框架？","header":"框架","multiSelect":false,"options":[{"label":"React","description":"组件库"},{"label":"Vue","description":"渐进式框架"}]},
			{"question":"【测试】需要哪些部分？","header":"部分","multiSelect":true,"options":[{"label":"登录"},{"label":"支付"},{"label":"报表"}]}]}`)}},
		{id: "testcc", kind: "plan", in: hookInput{ToolName: "ExitPlanMode", ToolInput: json.RawMessage(`{"plan":"# 【测试】计划\n1. 读代码\n2. 改 bug\n3. 跑测试"}`)}},
	}
	for _, r := range reqs {
		card, err := hookCard(r)
		if err != nil {
			t.Fatal(err)
		}
		id, err := fs.SendCardToUser(context.Background(), openID, card)
		if err != nil {
			t.Errorf("%s card rejected: %v", r.kind, err)
			continue
		}
		t.Logf("%s card sent: %s", r.kind, id)
		if err := fs.PatchCard(context.Background(), id, resolvedCard(r, "✅ 测试：已处理状态")); err != nil {
			t.Errorf("%s resolved patch rejected: %v", r.kind, err)
		}
	}
}
