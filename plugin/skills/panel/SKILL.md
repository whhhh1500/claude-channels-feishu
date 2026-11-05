---
name: panel
description: 在浏览器打开飞书接入面板（填凭据、给各个会话开关飞书、生成配对码、管理白名单和群）。用户输入 /feishu:panel 或说"打开飞书面板"时使用。
---

# /feishu:panel — 飞书面板

调用飞书 MCP 服务的 `panel` 工具（不需要参数）。它会在浏览器打开面板并返回链接，把链接告诉用户，并说明链接里带口令，只在本机使用。

如果工具报错（比如飞书 MCP 服务没加载），改为让用户在终端运行：

```
"${CLAUDE_PLUGIN_ROOT}/server/feishu-channel" web
```
