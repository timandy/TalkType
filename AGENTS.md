# AGENTS.md

WeType：通过微信官方 iLink Bot API、飞书机器人或企业微信智能机器人接收文本消息，用 Win32 `SendInput` 键入到当前光标处，或键入到用 Alt+. 锁定的窗口 / 终端。Go 1.22，无第三方依赖，模块名 `wetype`。

## 环境约束

- 运行时支持 Windows（`user32.dll` / `kernel32.dll`）和 macOS（`osascript` 驱动 System Events，不支持窗口锁定）。Linux / 云端只能编译和静态检查，无法验证真实键入。
- 带 `_windows.go` 后缀的文件只在 Windows 编译，`_darwin.go` 只在 macOS 编译，其余平台用 `*_other.go` 占位。新增 Win32 逻辑时各平台都要提供实现，macOS 不实现的功能（锁定、后台写终端）返回 `wintype.ErrUnsupported`。
- `wechat_token.json` 含 bot_token，`feishu_token.json` 含飞书 App Secret，`wecom_token.json` 含企业微信机器人 Secret，都已被 `.gitignore` 忽略。不要提交，不要打印到日志。

## 常用命令

```bash
go build ./...
go vet ./...
go test ./...
# 交叉检查 Windows、macOS 构建
GOOS=windows GOARCH=amd64 go build -o /tmp/wetype.exe ./cmd/wetype
GOOS=windows GOARCH=amd64 go vet ./...
GOOS=darwin GOARCH=arm64 go build -o /tmp/wetype ./cmd/wetype
GOOS=darwin GOARCH=arm64 go vet ./...
```

发布构建（Windows）：

```bat
go build -trimpath -ldflags "-s -w" -o wetype.exe ./cmd/wetype
```

消息来源由 `config.json` 的 `source`（`wechat` / `feishu` / `wecom` / `console`，不区分大小写）选择，命令行 `-source` 可临时覆盖。调试时可用 `wetype.exe -source console` 在控制台输入文字。

## 目录与分层

| 路径 | 职责 |
| --- | --- |
| `cmd/wetype/main.go` | 进程生命周期、参数、依赖装配。不放业务逻辑。 |
| `internal/config` | 共用配置的结构、默认值、校验。无内部依赖。 |
| `internal/source` | 来源接口 `Source`、消息 `Message`、`Handler`、`ErrDiscarded`，多来源组合 `Multi`。 |
| `internal/source/wechat` | 微信来源：签名、扫码、长轮询、ACK、重连、回执。只接受扫码人。 |
| `internal/source/feishu` | 飞书来源：扫码创建机器人、长连接（手写 protobuf 帧）、事件处理、回执。只处理机器人单聊，只接受扫码人。 |
| `internal/source/wecom` | 企业微信来源：扫码创建智能机器人、长连接（JSON 帧）、配对、心跳、回执。只处理机器人单聊，只接受配对的本人。 |
| `internal/source/console` | 控制台来源（调试用）。 |
| `internal/delivery` | 投递器 `Deliverer`、`Injector`、`CursorInjector`。 |
| `internal/delivery/winlock` | Alt+. / Alt+/ 锁定：状态、目标识别、热键、提示卡片。 |
| `internal/delivery/wintype` | 键入实现：SendInput、切前台输入、后台写终端、进程快照；macOS 用 osascript（`applescript.go`、`input_darwin.go`）。 |
| `internal/store` | 辅助：各来源的凭证与本人身份（`*_token.json`）和已处理消息 ID（`*_acked.ids`，一行一个，追加写入）。凭证和记录的读写只走这里。 |
| `internal/ws` | 辅助：最小 WebSocket 客户端（RFC 6455），飞书和企业微信共用；`wstest` 是测试用服务端。握手请求头需与官方 SDK 一致，见 `docs/wecom.md`。 |
| `docs/window-lock.md` | 锁定功能的原理、支持范围、已知限制。 |
| `docs/wechat.md` | 微信来源的请求头、签名、游标 ACK、会话过期和限制。 |
| `docs/feishu.md` | 飞书来源的接口、长连接协议、处理规则和未实测项。 |
| `docs/wecom.md` | 企业微信来源的接口、长连接协议、处理规则和未实测项。 |

分层规则：

- `internal/source/*` 不直接读写文件（走 `internal/store`），也不调用键盘输入；消息交给 `source.Handler`，由 `internal/delivery` 投递。
- Win32 调用只放在 `internal/delivery/wintype` 和 `internal/delivery/winlock` 的 `*_windows.go` 中。其他包不要引入 `syscall` / `unsafe`。
- `INPUT` 结构体 64 位（40 字节）和 32 位（28 字节）分别构造，改动时保持两种布局。

## 行为约定（改动时不要破坏）

- **启动前积压**：消息创建时间早于客户端创建时间（本次启动）的，只确认（推进游标、记入已处理）、不键入，用 `source.IsStale` 判断。来源没有提供创建时间时不做判断。
- **ACK**：iLink 没有独立 ACK 接口，游标 `get_updates_buf` 即 ACK。一批消息全部键入成功后才推进并持久化游标，任一失败则保留旧游标重放，重放靠 `message_id` 去重。
- **会话过期**：`ret` / `errcode` 为 `-14` 或 HTTP 401/403 时，删除 `wechat_token.json` 并重新扫码。网络错误、5xx、解析失败走指数退避加抖动，收到一次成功响应即重置。
- **非文本消息**：只在控制台打印提示，不键入、不回复。
- **飞书**：只处理机器人单聊；键入失败回 500 且不记为已处理，成功或按规则忽略回 200。不引入飞书 SDK，协议细节见 `docs/feishu.md`。
- **企业微信**：只处理机器人单聊；必须先发配对码确认本人，配对前不键入任何消息；不需要逐条确认，键入失败不会重推。同一机器人在别处连上（`disconnected_event`）时退出，不重连。协议细节见 `docs/wecom.md`。
- **签名**：HMAC-SHA256 `v1` 方案，默认开启，密钥取自 bot_token。canonical 串格式见 `docs/wechat.md`「签名」，修改前先读。
- **锁定**：终端后台直接写入，不切换窗口。其他窗口发送时短暂切换再切回。锁定目标关闭后丢弃消息并弹提示。

## 代码风格

- 提交前运行 `gofmt`，保证 `go vet` 通过。
- 日志输出到控制台，格式为 时间 + 前缀 + 正文（`log.Lmsgprefix`），前缀 `[wetype]`；各来源的日志前缀为 `[wetype-渠道名]`（如 `[wetype-wechat]`），由 `cmd/wetype` 创建后传给来源。
- 注释、日志、提交信息、文档使用中文，与现有代码保持一致。
- `config.json` 只放各来源共用的配置（`source`、`login`、`reconnect`、`inject`、`ack`）。各来源的地址和协议参数内置在各自的包里；凭证、本人身份、飞书站点由扫码自动获得，存进 `*_token.json`。不要为这些加配置项。新增共用配置项时同步更新 `internal/config` 的结构与默认值、根目录 `config.json`、README「配置说明」表格；测试会检查根目录 `config.json` 与默认值一致，且不允许未知字段。
- 测试与被测文件同目录，命名 `xxx_test.go`。Windows 专属测试用 `_windows_test.go` 后缀。

## 修改文档

- 用户可见行为变化（快捷键、参数、配置、限制）要同步更新 `README.md`。锁定相关的原理与限制同步更新 `docs/window-lock.md`；各来源的协议细节分别在 `docs/wechat.md`、`docs/feishu.md`、`docs/wecom.md`。
- README 末尾的「项目结构」需与实际文件保持一致。

## 许可证

Apache License 2.0，见 `LICENSE`。
