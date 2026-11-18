# WeType

让微信消息直达 Windows 窗口与终端。

基于微信官方 iLink Bot API（ClawBot，小龙虾），扫码登录后长轮询接收文本消息，键入到当前光标处，
或键入到用快捷键锁定的窗口与终端。终端在后台直接写入，不抢焦点，也不占用剪贴板。
可替代 OpenClaw 的微信渠道插件。也支持飞书（`feishu`）和企业微信（`wecom`）的机器人单聊：扫码创建机器人后通过长连接收消息。

> 依赖 Windows Win32 API（`user32.dll` / `kernel32.dll`），只能在 Windows 上运行。
> Linux / 云端环境只能编译与静态检查，无法验证真实键盘输入。

## 特性

- **扫码登录**：二维码获取、状态轮询、过期自动刷新，凭证自动落盘
- **长轮询收消息**：`getupdates` 挂起约 35 秒，无需公网 IP 或 Webhook
- **请求签名**：HMAC-SHA256（`v1` 方案），可开关
- **可靠 ACK**：键入成功才推进并持久化游标，失败保留游标重放
- **消息去重**：已处理的 `message_id` 逐行追加到 `wechat_acked.ids`，避免重放导致重复输入
- **凭证持久化**：`wechat_token.json` 原子写入，重启免扫码
- **断线重连**：指数退避 + 抖动；会话过期（`-14`）自动重新登录
- **Unicode 键入**：中文、Emoji 直接输入，无需剪贴板和输入法
- **只接受本人**：扫码时自动记下本人身份（企业微信发一次配对码），只键入本人发来的消息
- **锁定窗口或终端**：Alt+. 锁定目标，Alt+/ 解除；终端（PowerShell、Windows Terminal、JetBrains / VS Code 终端）在后台直接写入，不切换窗口
- **消息来源可替换**：微信、飞书、企业微信（机器人单聊），或调试用的控制台输入

## 工作流程

```
手机微信 ──► iLink 服务端 ──getupdates（长轮询）──► WeType
                                                      │
                                    签名 → 去重 → 键入 → ACK
                                                      │
                                                      ▼
                                     当前前台窗口光标处 + 回车
```

1. 首次运行：`get_bot_qrcode` 获取二维码 → 微信扫码 → `get_qrcode_status` 确认
2. 登录成功：保存 `bot_token`、`baseurl`、`ilink_bot_id` 到 `wechat_token.json`
3. 常驻循环：`getupdates` 收消息 → 过滤文本 → Unicode 键入 → 持久化游标
4. 异常恢复：网络错误退避重连；`-14` 清空凭证重新扫码

## 快速开始

1. 准备 `wetype.exe` 与 `config.json`，放在同一目录
2. 运行程序：

   ```bat
   wetype.exe
   ```

3. 首次运行会打印登录链接并自动打开浏览器，用手机微信扫码确认
4. 把目标窗口切到前台，并点击到输入框（确保光标可见）
5. 在手机上给 **ClawBot** 发送文本，程序会自动键入并回车

程序常驻运行，登录成功后无需再次扫码。强制重新登录：

```bat
wetype.exe -relogin
```

指定配置文件：

```bat
wetype.exe -config D:\wetype\config.json
```

## 编译

需要 Go 1.22+，无第三方依赖。

在 Windows 上直接编译：

```bat
go build -trimpath -ldflags "-s -w" -o wetype.exe ./cmd/wetype
```

在 Linux / macOS 上交叉编译：

```bash
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o wetype.exe ./cmd/wetype
```

静态检查（云端可用）：

```bash
go build ./...
go vet ./...
GOOS=windows GOARCH=amd64 go build -o /tmp/wetype.exe ./cmd/wetype
GOOS=windows GOARCH=amd64 go vet ./...
```

## 配置说明

`config.json` 只放各来源共用的配置，可以不存在，此时全部使用默认值。不认识的字段会报错，避免拼错的字段被静默忽略。

不需要配置、全部自动处理的内容：

- **服务地址和协议参数**：微信的 iLink 地址、bot 类型、`channel_version`、签名、长轮询超时，企业微信的长连接地址，都内置在程序里
- **凭证**：扫码获得，存进配置文件所在目录的 `wechat_token.json` / `feishu_token.json` / `wecom_token.json`
- **本人身份**：微信、飞书取扫码人，企业微信发一次配对码；之后只接受本人的消息
- **飞书站点**：先连国内版飞书，扫码账号属于国际版 Lark 时自动切换，并记进 `feishu_token.json`

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `source` | `"wechat"` | `wechat`（微信）、`feishu`（飞书）、`wecom`（企业微信）或 `console`（调试用），不区分大小写。命令行 `-source` 可临时覆盖 |
| `login.auto_open_qr` | `true` | 扫码登录时自动用浏览器打开二维码链接 |
| `reconnect.min_delay_ms` | `1000` | 断线重连的初始退避时间 |
| `reconnect.max_delay_ms` | `60000` | 最大退避时间，每次失败翻倍 |
| `inject.type_delay_ms` | `60` | 文字键入完成后、发送回车前的等待 |
| `inject.enter_delay_ms` | `120` | 回车前的额外等待 |
| `inject.press_enter` | `true` | 键入后是否发送回车 |
| `inject.max_retries` | `5` | 单条消息键入失败的重试次数 |
| `ack.reply_text` | `""` | 非空时，键入成功后向发送者回一条文本作为回执 |
| `ack.max_acked_ids` | `500` | 保留的已处理消息 ID 数量，用于去重。记录文件超过两倍时压缩 |

## 实现说明

### 请求头

`getupdates`、`sendmessage` 等业务请求会携带：

```
Content-Type: application/json
AuthorizationType: ilink_bot_token
Authorization: Bearer <bot_token>
iLink-App-Id: bot
X-WECHAT-UIN: base64(decimal(random_uint32))
```

`bot_token` 形如 `bot_id:secret`。登录成功后服务端可能下发独立的 `baseurl`，
后续请求优先使用该地址。

### 签名逻辑

官方 iLink 主要使用 Bearer 鉴权；本项目额外实现了 HMAC-SHA256 签名作为加固层，
未校验签名的服务端会忽略这些请求头。每个业务 POST 计算：

```
canonical = METHOD + "\n" + PATH + "\n" + timestamp + "\n" + nonce + "\n" + hex(SHA256(body))
signature = "v1=" + base64(HMAC-SHA256(secret, canonical))
```

写入请求头 `X-ILink-Timestamp`、`X-ILink-Nonce`、`X-ILink-Signature`。
签名默认开启，密钥取 bot_token 最后一个 `:` 之后的部分。

### 消息 ACK

iLink 没有单独的 ACK 接口，长轮询游标 `get_updates_buf` 就是 ACK：

1. `getupdates` 返回 `msgs` 与新的 `get_updates_buf`
2. 逐条处理消息，键入成功后将 `message_id` 追加到 `wechat_acked.ids`
3. 一批消息全部成功后才更新并持久化游标
4. 任意一条失败则不推进游标，退避后用旧游标重放
5. 重放时按 `message_id` 去重，已处理的不会再次键入

`wechat_acked.ids` 是纯文本，一行一个 ID，每处理一条消息追加一行；行数超过 `ack.max_acked_ids` 的两倍时整份重写，只留最近的；
进程中断导致最后一行没写完时，读取时丢弃该行并在下次写入前重写文件。删除该文件只会失去去重记录，不影响登录。

配置 `ack.reply_text` 后，处理成功还会调用 `sendmessage` 回一条用户可见的回执。

### 会话过期与重连

- `ret` / `errcode` 为 `-14`，或 HTTP 401/403：删除 `wechat_token.json`、清空内存状态并重新扫码
- 网络错误、HTTP 5xx、响应解析失败：按 `min_delay_ms` 起步翻倍，封顶 `max_delay_ms`，叠加随机抖动
- 成功收到一次 `getupdates` 响应即重置退避

### Windows 文本输入

1. 文本按 UTF-16 码元拆分为 `KEYEVENTF_UNICODE` 键事件（每个码元一次按下 + 一次抬起）
2. 换行、制表符转换为 `VK_RETURN`、`VK_TAB` 虚拟键
3. 每 512 个事件一批调用 `SendInput`，中文、Emoji 无需剪贴板或输入法
4. 可选等待后发送回车按下/抬起

`INPUT` 结构按 64 位（40 字节）/ 32 位（28 字节）分别构造。键击作用于**当前前台窗口**，
因此未锁定时需把目标窗口置前并点中输入框；锁定后的发送方式见 [docs/window-lock.md](docs/window-lock.md)。

## 锁定窗口或终端（Alt+. / Alt+/）

快捷键：

| 操作 | 效果 |
|---|---|
| 未锁定时按 Alt+. | 锁定当前窗口或终端 |
| 已锁定，在**别的**窗口或终端标签里按 Alt+. | 锁定切换到当前这个 |
| 已锁定，在**锁定的那个**里按 Alt+. | 解除锁定 |
| 在任意位置按 Alt+/ | 直接解除锁定 |

左右两个 Alt 键都可以。按下到提示卡片出现，实测 30～260 毫秒。

锁定后的发送方式：

- **终端**（PowerShell、cmd、Windows Terminal、IntelliJ IDEA / GoLand 等 JetBrains 终端、VS Code 终端）：
  在后台直接写入该终端，**不切换窗口、不闪烁、不打断你正在别处打字**。
  - Windows Terminal 有多个标签时，按窗口标题确认当前标签。
  - JetBrains / VS Code 的终端：按下 Alt+. 后，程序把窗口右边和底边临时内移再恢复（窗口右边和底部会轻微闪一下）。先内移 24 像素，没有反应再逐级加到 64、120 像素，
    终端停靠在 IDE 主窗口里时也能识别。终端单独成窗口时约 0.1 秒。
    IDE 只会调整正在显示的终端的尺寸，由此确定锁定哪个标签。Reworked 2025 和 Classic 引擎、Window / Float / 停靠等显示模式都适用。
    通知里会显示锁定标签的提示符，方便确认。
  - 也可以在目标终端里运行 `wetype.exe -lock` 精确锁定它；`-unlock` 解除。
- **其他窗口**（浏览器、IDE 的代码编辑器、终端被隐藏的 IDE 窗口等）：发送时短暂切到该窗口输入，再切回原来的窗口。
  凡是探测不到正在显示的终端，都按这种方式处理。
- 锁定的窗口或终端关闭后，消息不再发送（该消息被丢弃），并弹出提示。
- 每次状态变化都会在屏幕右下角弹出彩色提示卡片：新卡片把旧的往上顶，越旧越透明，几秒后淡出消失；不抢焦点，也不挡鼠标。
- 如果 Alt+. 已被其他程序占用，启动时会打印警告，锁定功能不可用；只有 Alt+/ 被占用时，可在已锁定的目标里按 Alt+. 解除。
- 以管理员身份运行的终端无法后台写入，除非本程序也以管理员身份运行。

原理、支持的窗口类别和已知限制详见 [docs/window-lock.md](docs/window-lock.md)。

## 消息来源

用 `config.json` 的 `source` 选择，也可以用命令行 `-source` 临时覆盖：

- `wechat`（默认）：微信。
- `feishu`：飞书机器人单聊，用法见下文。
- `wecom`：企业微信智能机器人单聊，用法见下文。
- `console`：调试用，不连接微信，在本程序窗口里输入文字测试发送。

控制台来源的用法：

```bat
wetype.exe -source console
```

每输入一行并回车，就当作收到一条消息。先切到目标终端按 Alt+. 锁定，再回到本窗口输入即可。
未锁定时，回车后有 3 秒时间切到目标窗口。

### 飞书

```bat
wetype.exe -source feishu
```

也可以把 `config.json` 的 `source` 改为 `feishu`，之后直接运行 `wetype.exe`。

首次运行会打印一个飞书授权链接，并在 `login.auto_open_qr` 开启时自动用浏览器打开。
用飞书 App 扫描该链接的二维码并确认，飞书会自动创建一个机器人应用，App ID、App Secret 和扫码人的 `open_id`
保存到 `feishu_token.json`，之后重启免扫码。然后在飞书里给这个机器人发单聊消息即可。

- 收消息走飞书长连接（WebSocket），程序主动连出，不需要公网 IP；WebSocket 和帧编解码都是手写的，不依赖飞书 SDK
- 只处理机器人单聊里的文字消息；群聊消息、图片等只在控制台提示，不输入、不回复
- 只接受扫码人本人的消息（扫码时自动获得本人的 `open_id`）
- 键入失败时向飞书返回 500、不记为已处理，期望飞书重推；键入成功或按规则忽略时返回 200
- `-relogin` 会清除 `feishu_token.json` 并重新扫码

扫码用的应用注册接口取自飞书官方安装工具 `@larksuite/openclaw-lark-tools`，开放平台没有公开文档，接口变化时扫码可能失效。企业组织账号扫码创建的应用需要管理员审核后才能使用。
调研记录和协议细节见 [docs/feishu.md](docs/feishu.md)。

### 企业微信

```bat
wetype.exe -source wecom
```

首次运行会打印企业微信的扫码页面地址，并在 `login.auto_open_qr` 开启时自动用浏览器打开。用企业微信扫描页面上的二维码，
会创建一个智能机器人并接入本程序，Bot ID 和 Secret 保存到 `wecom_token.json`，之后重启免扫码。再按控制台提示给机器人发一次配对码，之后给它发单聊消息即可。

- 收消息走企业微信长连接（`wss://openws.work.weixin.qq.com`），程序主动连出，不需要公网 IP；不依赖企业微信 SDK
- 只处理机器人单聊里的文字消息；群聊、图片、语音、文件等只在控制台提示，不输入、不回复
- 扫码结果里没有扫码人的身份，所以要配对：控制台会显示一个 6 位配对码，在企业微信里把它发给机器人，程序就把你的 `userid` 记为本人并回复“配对成功”。配对前不键入任何消息，配对后只接受本人的消息。配对记录保存在 `wecom_token.json`，换机器人或 `-relogin` 后需要重新配对。
- 同一个机器人只能有一个长连接。在别处又连上时，本程序会收到通知并退出，避免两边互相挤下线
- `-relogin` 会清除 `wecom_token.json` 并重新扫码

扫码接口取自企业微信官方命令行工具 `wecom-cli`，长连接协议取自官方 SDK `@wecom/aibot-node-sdk`，都没有公开文档。
协议细节见 [docs/wecom.md](docs/wecom.md)。

## 常见问题

| 现象 | 排查方向 |
| --- | --- |
| 一直停在等待扫码 | 检查网络是否可达 `ilinkai.weixin.qq.com`；二维码过期会自动刷新 |
| 收到消息但没有键入 | 确认目标窗口在前台、光标在输入框；尝试以与目标程序相同或更高权限运行 |
| 中文变成乱码或丢失 | 目标控件需支持 `WM_CHAR` / Unicode 输入；部分游戏、远程桌面或安全控件会拦截合成键击 |
| 偶尔重复输入 | 服务端未返回 `message_id` 时无法去重，属于预期情况 |
| 频繁出现重新扫码 | 微信侧主动失效或重复绑定顶号，重新扫码即可 |
| 输入到了别的窗口 | 未锁定时键入跟随当前前台焦点；按 Alt+. 锁定目标窗口或终端，见上文和 [docs/window-lock.md](docs/window-lock.md) |

日志直接输出到控制台，前缀为 `[wetype]`，可据此判断登录、收消息、ACK 与重连状态。

## 安全与限制

- `wechat_token.json` 等同于该 ClawBot 的收发权限，请勿提交到仓库或发到群聊；程序以 `0600` 写入
- `feishu_token.json` 含飞书机器人的 App Secret，同样请勿提交或外传；程序以 `0600` 写入
- `wecom_token.json` 含企业微信机器人的 Secret，同样请勿提交或外传；程序以 `0600` 写入
- iLink 目前仅支持私聊，不支持群聊
- ClawBot 名额通常只有一个，重复扫码可能顶掉之前的绑定
- 仅处理文本消息；图片、语音、文件等只在控制台打印“忽略…消息：暂只支持文字消息”，不输入、不回复
- 仅支持 Windows；UAC 安全桌面、更高完整性级别的窗口可能拒绝合成键击
- 协议字段可能随微信客户端版本变化，内置的 `channel_version` 需随官方插件升级并复核接口

## 项目结构

```
wetype/
├── cmd/
│   └── wetype/
│       └── main.go              程序入口：参数、选择消息来源、装配投递与锁定
├── internal/
│   ├── config/                  配置：共用配置的结构、默认值、校验
│   ├── source/                  消息来源
│   │   ├── source.go            来源接口 Source、消息 Message、Handler、ErrDiscarded
│   │   ├── wechat/              微信（iLink Bot API）：签名、扫码、长轮询、ACK、重连、回执
│   │   ├── feishu/              飞书：扫码创建机器人、长连接、pbbp2 帧编解码、事件处理、回执
│   │   ├── wecom/               企业微信：扫码创建智能机器人、长连接、配对、回执
│   │   └── console/             控制台（调试用）
│   ├── delivery/                投递
│   │   ├── delivery.go          投递器 Deliverer、Injector、CursorInjector
│   │   ├── winlock/             快捷键锁定：状态与语义、目标识别、热键、提示卡片
│   │   └── wintype/             键入：SendInput、切前台输入、后台写终端、进程快照；*_other.go 为非 Windows 占位
│   ├── store/                   辅助：凭证（*_token.json）和已处理消息 ID（*_acked.ids）的持久化
│   └── ws/                      辅助：最小 WebSocket 客户端（RFC 6455），飞书和企业微信共用
│       └── wstest/              测试用的 WebSocket 服务端
├── docs/
│   ├── window-lock.md           锁定功能的原理与支持范围
│   ├── feishu.md                飞书来源的协议细节与调研记录
│   └── wecom.md                 企业微信来源的协议细节与调研记录
├── config.json                  运行配置（可省略）
├── *_token.json                 运行后生成：各来源的凭证与本人身份
├── *_acked.ids                  运行后生成：已处理消息 ID，一行一个
├── go.mod
└── README.md
```

分层约定：

- `cmd` 只负责进程生命周期与依赖装配
- `internal/source/*` 只是消息来源，只依赖 `internal/source`、`internal/config`、`internal/store`、`internal/ws`，不调用键盘输入
- `internal/delivery` 负责投递，Win32 调用只在 `delivery/wintype` 和 `delivery/winlock` 的 `*_windows.go` 里，其他包不依赖 `syscall` / `unsafe`
- 凭证和消息记录的读写全部通过 `internal/store`
- `internal/config`、`internal/store`、`internal/ws` 没有内部依赖（`ws/wstest` 只依赖 `ws`），可被任意层引用

## 许可证

本项目基于 [Apache License 2.0](LICENSE) 开源，Copyright 2026 TimAndy。
