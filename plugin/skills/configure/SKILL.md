---
name: configure
description: 配置飞书 Channel：保存飞书应用的 App ID / App Secret、检查凭据、查看当前状态和下一步该做什么。用户贴出 cli_ 开头的 App ID 和 Secret、问"飞书怎么配置"、或想看飞书 channel 状态时使用。
argument-hint: "[app_id app_secret [--lark]]"
---

# /feishu:configure — 飞书 Channel 配置

程序路径：`${CLAUDE_PLUGIN_ROOT}/server/feishu-channel`（下文记作 `<fc>`，执行时用这个完整路径并加引号）。

参数：`$ARGUMENTS`

## 带参数：保存凭据

参数是 `<app_id> <app_secret>`（可选 `--lark` 表示国际版）时：

1. 执行 `"<fc>" configure <app_id> <app_secret>`（有 `--lark` 就带上）。
2. 再执行 `"<fc>" check`，把结果告诉用户：成功会打印机器人名字；失败说明凭据错误或权限没开。
3. 回复里不要重复完整的 App Secret。

## 不带参数：状态和下一步

1. 执行 `"<fc>" check`，看凭据是否可用。
2. 执行 `"<fc>" access list`，看白名单、群、私聊策略。
3. 用几句话告诉用户现状，并给出**一个**下一步：
   - 没有凭据 → 让用户到飞书开放平台建"企业自建应用"，然后运行 `/feishu:configure <app_id> <app_secret>`，或者用 `/feishu:panel` 在网页里填。
   - 凭据报错 → 多半是 App ID/Secret 填错，或应用版本没发布。
   - 凭据正常、白名单为空 → 在本会话运行 `/feishu:connect`，按提示用飞书私聊机器人发送配对码。
   - 都正常 → 运行 `/feishu:connect` 就能开始在飞书里聊。

## 开放平台要做的事（用户问起时再说）

- 添加"机器人"能力。
- 权限：`im:message.p2p_msg:readonly`、`im:message.group_at_msg:readonly`、`im:message:readonly`、`im:message:send_as_bot`、`im:message:update`、`im:message.reactions:write_only`、`im:resource`、`im:chat:readonly`；群里不 @ 也要收消息再加 `im:message.group_msg`。
- 事件与回调：订阅方式都选"长连接"；事件加 `im.message.receive_v1`，回调加 `card.action.trigger`。保存长连接时飞书要求有客户端在线，可以先 `/feishu:connect` 再去保存。
- 创建版本并发布，可用范围要包含用户自己。

## 提醒

这个会话必须用带 channel 参数的命令启动，飞书消息才进得来：

```
claude --dangerously-load-development-channels plugin:feishu@claude-channels
```
