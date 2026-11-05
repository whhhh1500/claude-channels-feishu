---
name: disconnect
description: 关闭飞书开关，本会话不再收发飞书消息，审批只在终端进行。用户输入 /feishu:disconnect，或说"断开飞书""关闭IM"时使用（在终端或飞书里说都可以）。
---

# /feishu:disconnect — 断开飞书

调用飞书 MCP 服务的 `disconnect` 工具（不需要参数），用一句话告诉用户结果。

断开是安全操作，白名单里的用户在飞书里要求断开也可以执行：先用 `reply` 回一句"已断开，回到电脑前说'连接飞书'可以重新连上"，再调用 `disconnect`（断开后就发不出飞书消息了）。
