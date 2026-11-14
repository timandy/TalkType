# clawbot-ilink-relay

自研的微信 **iLink / ClawBot（小龙虾）** 客户端，用于替代 OpenClaw 的微信渠道插件。

程序通过官方 iLink Bot API 扫码登录，长轮询接收文本消息，再在 Windows 上用
`SendInput` 的 Unicode 键事件把内容直接键入当前光标处并回车，**不占用剪贴板**。

> 依赖 Windows Win32 API（`user32.dll` / `kernel32.dll`），只能在 Windows 上运行。
> Linux / 云端环境只能编译与静态检查，无法验证真实键盘输入。

## 特性

- **扫码登录**：二维码获取、状态轮询、过期自动刷新，凭证自动落盘
- **长轮询收消息**：`getupdates` 挂起约 35 秒，无需公网 IP 或 Webhook
- **请求签名**：HMAC-SHA256（`v1` 方案），可开关
- **可靠 ACK**：键入成功才推进并持久化游标，失败保留游标重放
- **消息去重**：按 `message_id` 记录，避免重放导致重复输入
- **凭证持久化**：`token.json` 原子写入，重启免扫码
- **断线重连**：指数退避 + 抖动；会话过期（`-14`）自动重新登录
- **Unicode 键入**：中文、Emoji 直接输入，无需剪贴板和输入法
- **发送者白名单**：只处理指定微信用户的消息

## 工作流程

```
手机微信 ──► iLink 服务端 ──getupdates（长轮询）──► clawbot-ilink-relay
                                                      │
                                    签名 → 去重 → 键入 → ACK
                                                      │
                                                      ▼
                                     当前前台窗口光标处 + 回车
```

1. 首次运行：`get_bot_qrcode` 获取二维码 → 微信扫码 → `get_qrcode_status` 确认
2. 登录成功：保存 `bot_token`、`baseurl`、`ilink_bot_id` 到 `token.json`
3. 常驻循环：`getupdates` 收消息 → 过滤文本 → Unicode 键入 → 持久化游标
4. 异常恢复：网络错误退避重连；`-14` 清空凭证重新扫码

## 快速开始

1. 准备 `clawbot-ilink-relay.exe` 与 `config.json`，放在同一目录
2. 运行程序：

   ```bat
   clawbot-ilink-relay.exe
   ```

3. 首次运行会打印登录链接并自动打开浏览器，用手机微信扫码确认
4. 把目标窗口切到前台，并点击到输入框（确保光标可见）
5. 在手机上给 **ClawBot** 发送文本，程序会自动键入并回车

程序常驻运行，登录成功后无需再次扫码。强制重新登录：

```bat
clawbot-ilink-relay.exe -relogin
```

指定配置文件：

```bat
clawbot-ilink-relay.exe -config D:\clawbot\config.json
```

## 编译

需要 Go 1.22+，无第三方依赖。

在 Windows 上直接编译：

```bat
go build -trimpath -ldflags "-s -w" -o clawbot-ilink-relay.exe ./cmd/clawbot-ilink-relay
```

在 Linux / macOS 上交叉编译：

```bash
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o clawbot-ilink-relay.exe ./cmd/clawbot-ilink-relay
```

静态检查（云端可用）：

```bash
go build ./...
go vet ./...
GOOS=windows GOARCH=amd64 go build -o /tmp/clawbot-ilink-relay.exe ./cmd/clawbot-ilink-relay
GOOS=windows GOARCH=amd64 go vet ./...
```

## 配置说明

配置文件为 `config.json`，未填写的字段使用默认值。

### 基础

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `base_url` | `https://ilinkai.weixin.qq.com` | iLink Bot API 地址 |
| `bot_type` | `"3"` | 扫码登录的 bot 类型 |
| `channel_version` | `"2.1.6"` | `base_info.channel_version`，随官方插件升级 |
| `token_file` | `"token.json"` | 凭证文件，相对路径以配置文件目录为基准 |
| `token` | `""` | 可直接填 `bot_id:secret` 免扫码，通常留空 |

### 登录与轮询

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `login.auto_open_qr` | `true` | 自动用浏览器打开二维码链接 |
| `login.poll_interval_ms` | `2000` | 扫码状态轮询间隔 |
| `poll.timeout_seconds` | `35` | `getupdates` 长轮询超时 |
| `poll.http_timeout_seconds` | `45` | 单次 HTTP 超时，需大于长轮询超时 |

### 重连

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `reconnect.min_delay_ms` | `1000` | 初始退避时间 |
| `reconnect.max_delay_ms` | `60000` | 最大退避时间，每次失败翻倍 |

### 文本输入

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `inject.type_delay_ms` | `60` | 文字键入完成后、发送回车前的等待 |
| `inject.enter_delay_ms` | `120` | 回车前的额外等待 |
| `inject.press_enter` | `true` | 键入后是否发送回车 |
| `inject.max_retries` | `5` | 单条消息键入失败的重试次数 |

### ACK 与过滤

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `ack.reply_text` | `""` | 非空时，处理成功向发送者回一条文本作为回执 |
| `ack.max_acked_ids` | `500` | 本地保留的已 ACK 消息 ID 数量 |
| `filter.allowed_senders` | `[]` | 发送者白名单，空表示全部允许 |

### 签名

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `signature.enabled` | `true` | 是否为业务 POST 附加签名头 |
| `signature.secret` | `""` | 签名密钥，留空时取 token 中最后一个 `:` 之后的部分 |

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
不需要时设置 `signature.enabled = false`。

### 消息 ACK

iLink 没有单独的 ACK 接口，长轮询游标 `get_updates_buf` 就是 ACK：

1. `getupdates` 返回 `msgs` 与新的 `get_updates_buf`
2. 逐条处理消息，键入成功后将 `message_id` 写入本地 ACK 列表
3. 一批消息全部成功后才更新并持久化游标
4. 任意一条失败则不推进游标，退避后用旧游标重放
5. 重放时按 `message_id` 去重，已处理的不会再次键入

配置 `ack.reply_text` 后，处理成功还会调用 `sendmessage` 回一条用户可见的回执。

### 会话过期与重连

- `ret` / `errcode` 为 `-14`，或 HTTP 401/403：删除 `token.json`、清空内存状态并重新扫码
- 网络错误、HTTP 5xx、响应解析失败：按 `min_delay_ms` 起步翻倍，封顶 `max_delay_ms`，叠加随机抖动
- 成功收到一次 `getupdates` 响应即重置退避

### Windows 文本输入

1. 文本按 UTF-16 码元拆分为 `KEYEVENTF_UNICODE` 键事件（每个码元一次按下 + 一次抬起）
2. 换行、制表符转换为 `VK_RETURN`、`VK_TAB` 虚拟键
3. 每 512 个事件一批调用 `SendInput`，中文、Emoji 无需剪贴板或输入法
4. 可选等待后发送回车按下/抬起

`INPUT` 结构按 64 位（40 字节）/ 32 位（28 字节）分别构造。键击作用于**当前前台窗口**，
因此运行前需把目标窗口置前并点中输入框。

## 常见问题

| 现象 | 排查方向 |
| --- | --- |
| 一直停在等待扫码 | 检查网络是否可达 `ilinkai.weixin.qq.com`；二维码过期会自动刷新 |
| 收到消息但没有键入 | 确认目标窗口在前台、光标在输入框；尝试以与目标程序相同或更高权限运行 |
| 中文变成乱码或丢失 | 目标控件需支持 `WM_CHAR` / Unicode 输入；部分游戏、远程桌面或安全控件会拦截合成键击 |
| 偶尔重复输入 | 服务端未返回 `message_id` 时无法去重，属于预期情况 |
| 频繁出现重新扫码 | 微信侧主动失效或重复绑定顶号，重新扫码即可 |
| 输入到了别的窗口 | 程序不锁定窗口，键入跟随当前前台焦点，请勿在消息到达时切换窗口 |

日志直接输出到控制台，前缀为 `[clawbot]`，可据此判断登录、收消息、ACK 与重连状态。

## 安全与限制

- `token.json` 等同于该 ClawBot 的收发权限，请勿提交到仓库或发到群聊；程序以 `0600` 写入
- iLink 目前仅支持私聊，不支持群聊
- ClawBot 名额通常只有一个，重复扫码可能顶掉之前的绑定
- 仅处理文本消息，图片、语音、文件会被忽略
- 仅支持 Windows；UAC 安全桌面、更高完整性级别的窗口可能拒绝合成键击
- 协议字段可能随微信客户端版本变化，升级时请同步 `channel_version` 并复核接口

## 项目结构

```
clawbot-ilink-relay/
├── cmd/
│   └── clawbot-ilink-relay/
│       └── main.go          程序入口：参数、配置与凭证初始化、启动
├── internal/
│   ├── config/
│   │   └── config.go        配置结构、默认值、校验
│   ├── store/
│   │   └── store.go         bot_token、游标、context_token、ACK 持久化
│   ├── ilink/
│   │   ├── protocol.go      iLink JSON 协议结构
│   │   ├── client.go        签名、登录、长轮询、ACK、重连、sendmessage
│   │   └── util.go          随机数、退避、休眠、打开链接
│   └── wintype/
│       ├── options.go       输入参数
│       ├── input_windows.go SendInput Unicode 键入（//go:build windows）
│       └── input_other.go   非 Windows 占位实现
├── config.json              运行配置
├── token.json               运行后生成：凭证与游标
├── go.mod
└── README.md
```

分层约定：

- `cmd` 只负责进程生命周期与依赖装配
- `internal/ilink` 不直接操作文件，凭证读写全部通过 `internal/store`
- `internal/wintype` 隔离全部 Win32 调用，其他包不依赖 `syscall` / `unsafe`
- `internal/config` 无内部依赖，可被任意层引用

## 许可证

本项目基于 [MIT License](LICENSE) 开源，Copyright (c) 2026 TimAndy。
