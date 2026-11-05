# 飞书插件命令行测试：普通 claude 启动（不带 channel 参数），默认审批模式
$prompt = "现在调用飞书的 connect 工具连接飞书，并告诉我结果。连接成功后，用 Write 工具创建 scripts/manual-test/test.txt，内容写 hello。然后等待飞书消息并回复。"
claude $prompt --permission-mode default --allowedTools "mcp__plugin_feishu_feishu__connect,mcp__plugin_feishu_feishu__reply,mcp__plugin_feishu_feishu__status"
