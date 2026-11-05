#!/usr/bin/env bash
# 编译飞书 channel，输出到插件目录 plugin/server/ 下（插件的 .mcp.json 和 hooks 从这里启动它）。
set -euo pipefail
cd "$(dirname "$0")/.."
if ! command -v go >/dev/null 2>&1; then
  echo "没找到 go 命令：先安装 Go 1.27 以上（https://go.dev/dl/，macOS 也可以 brew install go），再重新运行。" >&2
  exit 1
fi
echo "$(go env GOVERSION)，目标平台 $(go env GOOS)/$(go env GOARCH)"
out="plugin/server/feishu-channel$(go env GOEXE)"
# Windows 上正在运行的 exe 不能覆盖，但可以改名：先挪开旧文件，新会话就会用新版本
if [ -f "$out" ]; then
  mv -f "$out" "$out.old" 2>/dev/null || true
fi
go build -trimpath -ldflags "-s -w" -o "$out" ./cmd/feishu-channel
rm -f "$out.old" 2>/dev/null || true
echo "已编译: $out"
