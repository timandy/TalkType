# AGENTS.md

WeType：通过微信官方 iLink Bot API 或飞书机器人接收文本消息，用 Win32 `SendInput` 键入到当前光标处，或键入到用 Alt+. 锁定的窗口 / 终端。Go 1.22，无第三方依赖，模块名 `wetype`。

## 环境约束

- 运行时只支持 Windows（`user32.dll` / `kernel32.dll`）。Linux / 云端只能编译和静态检查，无法验证真实键入。
- 带 `_windows.go` 后缀的文件只在 Windows 编译，其余平台用 `*_other.go` 占位。新增 Win32 逻辑时两边都要提供实现。
- `wechat_token.json` 含 bot_token，`feishu_token.json` 含飞书 App Secret，都已被 `.gitignore` 忽略。不要提交，不要打印到日志。

## 常用命令

```bash
go build ./...
go vet ./...
go test ./...
# 交叉检查 Windows 构建
GOOS=windows GOARCH=amd64 go build -o /tmp/wetype.exe ./cmd/wetype
GOOS=windows GOARCH=amd64 go vet ./...
```

发布构建（Windows）：

```bat
go build -trimpath -ldflags "-s -w" -o wetype.exe ./cmd/wetype
```

消息来源由 `config.json` 的 `source`（`wechat` / `feishu` / `console`，不区分大小写）选择，命令行 `-source` 可临时覆盖。调试时可用 `wetype.exe -source console` 在控制台输入文字。

## 目录与分层

| 路径 | 职责 |
| --- | --- |
| `cmd/wetype/main.go` | 进程生命周期、参数、依赖装配。不放业务逻辑。 |
| `internal/config` | 配置结构、默认值、校验。无内部依赖，可被任意层引用。 |
| `internal/store` | 微信的 `bot_token`、游标、`context_token`，飞书的 App ID / Secret，以及已处理消息 ID 记录（`*_acked.ids`，一行一个，追加写入）。凭证和记录的读写只走这里。 |
| `internal/relay` | 消息来源接口 `Source` 与投递器 `Deliverer`。 |
| `internal/ilink` | 微信来源：签名、登录、长轮询、ACK、重连、sendmessage。 |
| `internal/feishu` | 飞书来源：扫码创建机器人、长连接（手写 WebSocket 和 protobuf 帧）、事件处理、回执。只处理机器人单聊。 |
| `internal/console` | 控制台来源（调试用）。 |
| `internal/winlock` | Alt+. / Alt+/ 锁定：状态、目标识别、热键、提示卡片。 |
| `internal/wintype` | 键入实现：SendInput、切前台输入、后台写终端、进程快照。 |
| `docs/window-lock.md` | 锁定功能的原理、支持范围、已知限制。 |
| `docs/feishu.md` | 飞书来源的接口、长连接协议、处理规则和未实测项。 |

分层规则：

- `internal/ilink`、`internal/feishu` 不直接读写文件，也不直接调用键盘输入。投递统一经过 `internal/relay`。
- Win32 调用只放在 `internal/wintype` 和 `internal/winlock` 的 `*_windows.go` 中。其他包不要引入 `syscall` / `unsafe`。
- `INPUT` 结构体 64 位（40 字节）和 32 位（28 字节）分别构造，改动时保持两种布局。

## 行为约定（改动时不要破坏）

- **ACK**：iLink 没有独立 ACK 接口，游标 `get_updates_buf` 即 ACK。一批消息全部键入成功后才推进并持久化游标，任一失败则保留旧游标重放，重放靠 `message_id` 去重。
- **会话过期**：`ret` / `errcode` 为 `-14` 或 HTTP 401/403 时，删除 `wechat_token.json` 并重新扫码。网络错误、5xx、解析失败走指数退避加抖动，收到一次成功响应即重置。
- **非文本消息**：只在控制台打印提示，不键入、不回复。
- **飞书**：只处理机器人单聊；键入失败回 500 且不记为已处理，成功或按规则忽略回 200。不引入飞书 SDK，协议细节见 `docs/feishu.md`。
- **签名**：HMAC-SHA256 `v1` 方案，由 `wechat.signature.enabled` 控制。canonical 串格式见 README「签名逻辑」，修改前先读。
- **锁定**：终端后台直接写入，不切换窗口。其他窗口发送时短暂切换再切回。锁定目标关闭后丢弃消息并弹提示。

## 代码风格

- 提交前运行 `gofmt`，保证 `go vet` 通过。
- 日志输出到控制台，前缀 `[wetype]`。
- 注释、日志、提交信息、文档使用中文，与现有代码保持一致。
- 新增配置项时同步更新三处：`internal/config` 的结构与默认值、根目录 `config.json`、README「配置说明」表格。各来源共用的放顶层，微信、飞书独有的放 `wechat`、`feishu` 对象里。测试会检查根目录 `config.json` 与默认值一致，且不允许未知字段。
- 测试与被测文件同目录，命名 `xxx_test.go`。Windows 专属测试用 `_windows_test.go` 后缀。

## 修改文档

- 用户可见行为变化（快捷键、参数、配置、限制）要同步更新 `README.md`。锁定相关的原理与限制同步更新 `docs/window-lock.md`。
- README 末尾的「项目结构」需与实际文件保持一致。

## 许可证

Apache License 2.0，见 `LICENSE`。
