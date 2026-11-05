# feishu-channel

把飞书机器人接进**正在运行的** Claude Code 会话：你在飞书上发消息，Claude 在电脑上干活，回复发回飞书；Claude 需要授权、向你提问或确认计划时，卡片发到你的飞书上点。离开电脑也能继续用 Claude Code。

- **开关式**：默认不连飞书。在会话里说"连接飞书"才开始收发，说"断开飞书"就关。
- **配对码**：第一次连接时电脑上显示 6 位配对码，用飞书私聊机器人发这个码就完成绑定。
- **远程审批**：授权、选择题、计划确认都变成飞书卡片，电脑上的弹窗同时有效，哪边先答用哪边。
- **电脑和飞书同步**：飞书来的消息和 Claude 的回复在电脑上都看得到；你在电脑上打的字和 Claude 的回答也会同步到飞书。
- **多会话**：几个会话可以同时连飞书，引用回复哪个会话的消息就发给哪个。
- **网页面板**：在本机网页上填凭据、开关各个会话、生成配对码、管理白名单。
- 飞书用长连接收消息，不需要公网 IP、不需要服务器。

## 快速开始

需要：Go 1.27 以上（建议 1.27.3 或更新）、Claude Code 命令行、一个能建企业自建应用的飞书账号。Windows、macOS、Linux 都能用。

### 1. 下载并编译

```bash
git clone https://github.com/whhhh1500/claude-channels-feishu.git
```

```bash
cd claude-channels-feishu
```

```bash
bash scripts/build.sh
```

程序会编译到 `plugin/server/feishu-channel`（Windows 上是 `feishu-channel.exe`）。仓库里不带编译好的程序，必须先编译。

### 2. 建飞书应用

在 [飞书开放平台](https://open.feishu.cn/app) 创建**企业自建应用**，加上机器人能力、开通权限、订阅长连接事件，然后发布。完整步骤见后面的[飞书应用配置](#飞书应用配置)。记下 App ID 和 App Secret。

### 3. 填凭据

```bash
./plugin/server/feishu-channel configure <App ID> <App Secret>
```

```bash
./plugin/server/feishu-channel check
```

显示"凭据正常。机器人: xxx"就对了。也可以用网页面板填：`./plugin/server/feishu-channel web`。

### 4. 安装插件

把仓库目录登记成插件市场（写完整路径），再装上里面的 feishu 插件：

```bash
claude plugin marketplace add /path/to/claude-channels-feishu
```

Windows 上写成 `claude plugin marketplace add D:\code\claude-channels-feishu` 这样的完整路径。

```bash
claude plugin install feishu@claude-channels
```

插件直接使用仓库里编译出的程序。以后更新只要 `git pull` 再 `bash scripts/build.sh`，然后新开会话，不用重装插件。

### 5. 启动、连接、配对

**推荐用这条命令启动**（原生 Channel 模式，体验最好，见下一节对比）：

```bash
claude --dangerously-load-development-channels plugin:feishu@claude-channels
```

启动时会出现一个全屏警告，说明正在加载开发中的 Channel，选 **I am using this for local development** 继续。这个参数名里有 dangerously，是因为自建插件不在 Anthropic 的 Channel 白名单里，官方要求用它来加载；`--channels` 只能加载官方插件。

进入会话后说"连接飞书"（或运行 `/feishu:connect`）。第一次会显示：

```
已连接飞书机器人「xxx」。
还没有配对的飞书账号。配对码：k7m2qx
请用要绑定的飞书账号私聊机器人「xxx」，只发送这 6 个字符。
```

用飞书私聊机器人发 `k7m2qx`，机器人回"配对成功"，之后直接在飞书上聊。配对码 10 分钟有效，只能用一次，只在电脑上显示。

**想一启动就自动连上**，加环境变量 `FEISHU_AUTOCONNECT=1`（只在原生 Channel 模式下生效）：

```bash
FEISHU_AUTOCONNECT=1 claude --dangerously-load-development-channels plugin:feishu@claude-channels
```

PowerShell 写法：

```powershell
$env:FEISHU_AUTOCONNECT = "1"; claude --dangerously-load-development-channels plugin:feishu@claude-channels
```

常用的话可以做成别名，比如在 `~/.bashrc` 或 `~/.zshrc` 里加：

```bash
alias claude-feishu='FEISHU_AUTOCONNECT=1 claude --dangerously-load-development-channels plugin:feishu@claude-channels'
```

之后运行 `claude-feishu` 就行，其他参数照常加在后面，比如 `claude-feishu --resume`。

## 三种启动方式

不带参数直接运行 `claude`、或者用桌面 App 也能用，只是体验差一点：

| | 原生 Channel（推荐） | 普通命令行 `claude` | 桌面 App |
|---|---|---|---|
| 启动方式 | `claude --dangerously-load-development-channels plugin:feishu@claude-channels` | `claude` | 正常新建会话 |
| 飞书消息怎么进会话 | Claude Code 直接推进会话 | 连接时 Claude 启动一个后台监听（Monitor，最长 30 分钟）；到期时有过消息就重启，空闲就换成没有时间限制的后台等待命令，来消息再切回来 | 同普通命令行 |
| 监听维护的额外 token | 无 | 有消息时每 30 分钟 2 次调用；空闲后切换一次，之后不再消耗；空闲后来飞书消息时多 2 次（见 [token 消耗](#token-消耗)） | 同普通命令行 |
| 启动就自动连接 | 可以（`FEISHU_AUTOCONNECT=1`） | 不行，要在会话里说"连接飞书" | 同普通命令行 |
| 启动时 | 每次弹一次开发警告 | 无 | 无 |
| 审批、提问、计划卡片 | ✅ | ✅ | ✅ |
| 回复发到飞书、电脑打字同步 | ✅ | ✅ | ✅ |

程序会自动判断当前是哪种方式，连接时选对应的收消息方式，不用手动设置。

不支持的情况：
- `claude -p` 和 Agent SDK 这类非交互模式收不到消息，连接会被拒绝。
- Claude Code 用 Amazon Bedrock、Google Cloud、Microsoft Foundry 接入时不能用 Channel，只能用普通命令行或桌面 App 的方式。
- claude.ai Team / Enterprise 账号要管理员先在"组织设置 > Claude Code > Channels"里打开 Channels，原生 Channel 模式才能用。

## 日常使用

### 在会话里

| 说（或命令） | 效果 |
|---|---|
| 连接飞书 / `/feishu:connect` | 打开开关 |
| 断开飞书 / `/feishu:disconnect` | 关闭开关（在飞书里说也行） |
| 飞书状态 | 是否连接、绑定了几个账号 |
| 生成飞书配对码 | 再绑定一个飞书账号 |
| 打开飞书面板 / `/feishu:panel` | 在浏览器打开网页面板 |
| `/feishu:configure [app_id app_secret]` | 保存凭据；不带参数时查看状态和下一步 |
| `/feishu:access ...` | 管理白名单、群、私聊策略 |

连接飞书、改白名单、生成配对码只接受你在电脑上提的要求，飞书消息里提的一律拒绝，防止别人通过飞书提权。

### 在飞书上

- 直接发消息，像和 Claude 聊天一样。机器人先给你的消息加一个"敲键盘"表情表示收到，Claude 回复后撤掉。
- 回复支持 markdown，太长会自动分成几条。
- 发图片，Claude 会直接看；发文件，Claude 需要时会下载。
- 引用某条消息再提问，被引用的内容会一起带给 Claude。
- 需要你决定时会收到卡片，9 分钟没处理自动转回电脑：

  | 情况 | 卡片上能做的 |
  |---|---|
  | Claude 要执行命令、改文件 | 允许 / 总是允许 / 拒绝（可写原因） / 回电脑处理 |
  | Claude 向你提问 | 每题一个下拉框，加"其他"输入框；提交 / 回电脑回答 |
  | 计划模式结束 | 批准 / 继续修改（写修改意见） / 回电脑处理 |

### 电脑和飞书同步

- 飞书来的消息，电脑上显示成一行"📨 消息正文"；Claude 给你的回复写成一段"r📨: ……"，回合结束时原样发到飞书。电脑上看到的就是飞书收到的。
- 你在电脑上直接打字，一提交就同步一条"💻 你打的字"到你的飞书私聊，Claude 答完后再引用它发出最终回答。不想同步设 `FEISHU_MIRROR=off`。
- 每条发到飞书的消息开头带会话标识，例如 `【report-bot#1a2b3c4d#...acme\shop\api】`（会话名#会话 ID 前 8 位#当前目录最后三级），多个会话同时在线时分得清是谁在说话。设 `FEISHU_SESSION_TAG=off` 关掉。
- 同步和远程卡片靠插件的 hook，要按上面的方式装成插件才有。

### 多个会话同时连飞书

- 只有一个会话在线：消息直接给它。
- 多个会话在线：在飞书里**引用回复**某个会话发的消息，就送到那个会话；没引用时机器人会弹卡片让你选。
- 审批卡片各自记着是哪个会话的，点按钮会回到那个会话。

### 网页面板

```bash
./plugin/server/feishu-channel web
```

浏览器会打开 `http://127.0.0.1:47830/?t=<口令>`，只在本机访问。可以填凭据、测试连接、给每个会话开关飞书、生成配对码、管理白名单和群，还能看连接历史：已退出的会话可以复制 `claude --resume <会话ID>` 恢复。

## 命令行

`feishu-channel` 程序除了被插件拉起，也可以直接在终端用：

```bash
./plugin/server/feishu-channel configure <app_id> <app_secret> [--lark]  # 保存凭据（--lark 用于国际版 Lark）
./plugin/server/feishu-channel check                             # 测试凭据
./plugin/server/feishu-channel web [--no-open]                   # 网页面板
./plugin/server/feishu-channel access list                       # 查看白名单、群、待配对
./plugin/server/feishu-channel access pair <配对码>              # 批准配对
./plugin/server/feishu-channel access allow <open_id>            # 直接加白名单
./plugin/server/feishu-channel access remove <open_id>           # 移出白名单
./plugin/server/feishu-channel access policy pairing|allowlist|disabled   # 私聊策略
./plugin/server/feishu-channel access group add <chat_id> [--no-mention]  # 允许某个群（默认要 @机器人）
./plugin/server/feishu-channel access group remove <chat_id>
./plugin/server/feishu-channel access ack <emoji_type|off>       # 收到消息时加的表情
./plugin/server/feishu-channel version
```

`listen` 和 `hook` 两个子命令是给插件内部用的，不用手动运行。

## 环境变量

在启动 `claude` 的终端里设置，插件会继承。

| 变量 | 作用 |
|---|---|
| `FEISHU_AUTOCONNECT` | 设任意值：会话启动就自动连接（只在原生 Channel 模式下生效） |
| `FEISHU_MIRROR` | 设为 `off`：电脑上打的字和回答不同步到飞书 |
| `FEISHU_SESSION_TAG` | 设为 `off`：发到飞书的消息不带会话标识 |
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | 覆盖保存的凭据 |
| `FEISHU_DOMAIN` | `feishu`（默认）或 `lark` |
| `FEISHU_STATE_DIR` | 状态目录，默认 `~/.claude/channels/feishu` |
| `FEISHU_PANEL_ADDR` | 面板地址，默认 `127.0.0.1:47830` |
| `FEISHU_LOCK_ADDR` | 多会话共用的接收锁端口，默认 `127.0.0.1:47831` |
| `FEISHU_SKIP_FLAG_CHECK` | 启动参数检测误判时跳过检查 |
| `FEISHU_DEBUG` | 打开调试日志 |

## token 消耗

同步到飞书、审批卡片、网页面板都由插件自己完成，不经过模型，不消耗 token。额外消耗来自下面几处。

先说一个前提：模型本身不记事，Claude Code **每调用一次模型，都要把整个对话（系统提示 + 全部历史）重新发一遍**。所以一次调用的花费约等于当时的上下文大小，长会话里一次就是几十万 token。重复的部分有缓存：缓存有效时按约 1/10 的价格计费；缓存过期后，下一次要按约 1.25 倍价格重新写入。下文用"1 份上下文"表示一次全价读完整个对话的花费。

### 固定和按消息的消耗

| 项目 | 大约多少 | 说明 |
|---|---|---|
| 每个会话固定 | 1,600～2,500 token | 插件说明、命令描述、工具列表。插件装在用户级，不连飞书的会话也有；之后每轮按缓存价计费 |
| 每条飞书消息 | 约 300 token | 消息附带的会话 ID、消息 ID、回复格式提示，加上 Claude 写的"📨"回显行 |
| 电脑上打字同步、`r📨` 回复、审批卡片 | 0 | 插件自己发，不经过模型 |

### 监听维护（只有普通命令行和桌面 App）

原生 Channel 模式由 Claude Code 直接推消息，没有这一块。

普通命令行和桌面 App 靠 Claude 用 Monitor 工具启动的监听收消息。Monitor 最长运行 30 分钟，到期后只有 Claude 能重新启动它，每次要调用 2 次模型。插件的做法：

- **这 30 分钟里收到过消息**：重启监听，2 次调用，按缓存价约 0.2 份上下文。
- **一条都没收到**：Claude 改用后台等待命令 `feishu-channel listen --wait`。它没有时间上限，等待期间不调用模型。这次切换同样是 2 次调用。
- **等待期间来了飞书消息**：等待命令结束，Claude 重启监听，暂存的消息马上补发。多 2 次调用，回复晚几秒到十几秒；飞书上的"敲键盘"表情会立刻加上。

举例：一个上下文约 34 万 token 的会话，聊天活跃时每 30 分钟维护一次，约 68 万 token（按缓存价计）；空闲下来切换一次后，就不再消耗。

### 长时间没对话之后

| 情况 | 额外消耗 |
|---|---|
| 在电脑上打字 | 插件没有额外调用。等待命令继续挂着，不用重启监听 |
| 从飞书发来消息 | 多 2 次调用（叫醒、重启监听），约 1.35 份上下文：缓存已过期，第一次要重新写入（1.25 份），第二次按缓存价（0.1 份） |

缓存过期和插件无关：隔很久再对话，第一次调用本来就要重写缓存。

### 怎么省

- 长时间离开电脑、用飞书遥控时，用原生 Channel 模式：没有监听维护，消息直接进会话。
- 会话很长时先 `/compact` 压缩，或开新会话。每次调用的花费和上下文大小成正比。
- 不用飞书的时候说"断开飞书"：不再收消息，也就没有任何维护调用。

## 注意事项

- **飞书上的每条消息都能让 Claude 操作你的电脑**，白名单要管好，审批权限也跟着白名单走。
- 群聊默认只处理白名单成员 @机器人 的消息，群要先在面板或 `access group add` 里登记。
- Claude 正在处理时，新来的消息会排队，等这一轮结束后一起处理。
- 断线重连后飞书会补发旧消息，超过 30 分钟的直接丢弃。
- 插件依赖 Claude Code 的 Channels（研究预览）和对话记录格式，Claude Code 升级后可能需要跟着改。
- 日志写到 stderr。排查问题时用 `claude --debug` 启动（原生 Channel 模式再加上面的参数），再看 `~/.claude/debug/<会话ID>.txt`。
- 以前用 `claude mcp add` 手动注册过 feishu 的，先 `claude mcp remove feishu -s user` 删掉，否则会有两套一样的工具。

## 飞书应用配置

1. 打开 [飞书开放平台](https://open.feishu.cn/app)，创建**企业自建应用**，记下 App ID 和 App Secret。
2. **添加应用能力** → 机器人。
3. **权限管理**，开通以下权限：

   | 权限 | 用途 |
   |---|---|
   | `im:message.p2p_msg:readonly` | 收私聊消息 |
   | `im:message.group_at_msg:readonly` | 收群里 @机器人 的消息 |
   | `im:message:readonly` | 读取被引用的消息 |
   | `im:message:send_as_bot` | 发消息 |
   | `im:message:update` | 编辑消息、更新审批卡片 |
   | `im:message.reactions:write_only` | 加/撤"处理中"表情 |
   | `im:resource` | 收发图片和文件 |
   | `im:message.group_msg`（可选，敏感权限） | 群里不 @ 也能收 |

4. **事件与回调**：
   - 事件配置 → 订阅方式选**使用长连接接收事件**，添加事件 `im.message.receive_v1`。
   - 回调配置 → 订阅方式选**使用长连接接收回调**，添加回调 `card.action.trigger`。没有这个，卡片上的按钮点了没反应。
   - 保存长连接设置时飞书要求有客户端在线：先完成快速开始的第 3、4 步，在会话里说"连接飞书"（这时连不上没关系），再回来保存。
5. **版本管理与发布** → 创建版本并发布。之后每次改权限都要重新发布。

## 开发

```bash
go test ./...
```

```bash
bash scripts/build.sh
```

改完代码重新编译，再新开会话就会用上新版本。真实发飞书卡片的测试默认跳过，要跑时设置 `FEISHU_LIVE_OPEN_ID=<你的 open_id>`。

```
.claude-plugin/marketplace.json   本仓库作为插件市场的声明
plugin/                           插件本体（Claude Code 安装的就是这个目录）
  .claude-plugin/plugin.json      插件声明
  .mcp.json                       启动 server/feishu-channel 作为 MCP 服务
  hooks/hooks.json                审批卡片、回合结束同步、打字同步
  skills/                         /feishu:configure、connect、disconnect、panel、access
  server/                         编译产物（不进 git）
cmd/feishu-channel/               程序入口：MCP 服务和命令行子命令
internal/
  bridge/                         核心：收发消息、hook 卡片、消息投递、连接开关、控制端口、MCP 工具
  feishu/                         飞书 API 封装，消息内容和 markdown 转换
  access/                         白名单、群策略、配对码
  session/                        会话登记，hook 和监听进程连回会话的客户端
  hostinfo/                       识别桌面端 / 命令行 / SDK 和 channel 启动参数
  mcp/                            最小的 MCP stdio 服务端
  panel/                          本机网页面板
  config/                         凭据和状态目录
scripts/build.sh                  编译到 plugin/server/
```

实现细节：

- 收消息：带 channel 参数启动时，程序通过 MCP 的 Channel 通知把消息推进会话；不带参数时，连接时让 Claude 用 Monitor 工具运行 `feishu-channel listen`，每条飞书消息是一行输出；监听到期时如果这段时间没有消息，Claude 改用后台 Bash 运行 `feishu-channel listen --wait`，它挂在控制端口上等到有暂存消息才退出（退出码 0；飞书断开时是 3），Claude 再启动 Monitor，暂存的消息随即补发。程序看拉起它的 Claude 进程的命令行来判断是哪种情况，以及是桌面端、命令行还是 SDK。
- 多会话：飞书长连接每条事件只推给一个连接，所以第一个连上的会话拿到本机锁（`127.0.0.1:47831`）负责接收，再通过各会话的本机控制端口问"这条引用的是不是你的消息"并转过去。它退出后其他在线会话 2 秒内接手。
- 同步：Stop hook 在回合结束时读这一回合的对话记录，发出 r📨 段落或最终回答；UserPromptSubmit hook 在你提交时把打的字发到飞书私聊。

## 许可证

MIT，见 [LICENSE](LICENSE)。
