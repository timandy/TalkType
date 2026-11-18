# 企业微信消息来源：协议与实现

`source` 设为 `wecom`（或命令行 `-source wecom`）时，通过企业微信“智能机器人”接收**单聊**里的文字消息。
代码在 `internal/wecom`，凭证读写在 `internal/store/wecom.go`，WebSocket 客户端在 `internal/ws`（与飞书共用）。不依赖企业微信 SDK。

## 1. 整体流程

```
首次运行：GET /ai/qc/generate 拿到 scode → 浏览器打开扫码页 → 用户用企业微信扫码 → 轮询 /ai/qc/query_result 拿到 botid / secret → 写入 wecom_token.json
          → 控制台显示 6 位配对码 → 用户把配对码发给机器人 → 发送者的 userid 记为本人，写入 wecom_token.json
常驻运行：连接 wss://openws.work.weixin.qq.com → 发认证帧 → 每 30 秒心跳 → 收消息回调 → 键入 → 可选回执
异常恢复：断线按 reconnect.* 退避重连；认证返回 853000 或连续 3 次认证失败时视为凭证无效，清除本地凭证后重新扫码；
         收到 disconnected_event（同一机器人在别处连上）时退出，不重连
```

凭证只能扫码获得：有 `wecom_token.json` 就用它，没有就扫码。不支持在 `config.json` 里手动填写。

## 2. 扫码创建智能机器人

接口取自企业微信官方命令行工具 `wecom-cli`（`auth init` 默认扫码，npm 包 `@wecom/cli-win32-x64` 里的可执行文件），
请求和响应格式取自扫码页面的脚本 `wwcdn.weixin.qq.com/.../aibotQuickCreate/gen$*.js`。都没有公开文档。

| 步骤 | 请求 | 响应（已实测前两项） |
|---|---|---|
| 生成 | `GET https://work.weixin.qq.com/ai/qc/generate?source=wecom_cli_external&plat=1` | `{"data":{"scode":"…","auth_url":"https://work.weixin.qq.com/ai/qc/c?s=…"}}` |
| 扫码页 | 浏览器打开 `https://work.weixin.qq.com/ai/qc/gen?source=wecom_cli_external&scode=…` | 页面标题“接入企业微信智能机器人”，显示二维码 |
| 轮询 | `GET https://work.weixin.qq.com/ai/qc/query_result?scode=…` | 未扫码 `{"data":{"status":"init"}}`；成功时 `status` 为 `success`，`bot_info` 里有 `botid`、`secret` |

- 页面每 3 秒轮询一次，二维码 5 分钟后过期；程序按同样的节奏，过期后重新生成。
- `source=wecom_cli_external` 是 `wecom-cli` 用的取值，程序沿用。
- 扫码结果里没有扫码人的身份，所以用配对码确认本人，见第 4 节。

## 3. 长连接

协议取自官方 SDK `@wecom/aibot-node-sdk`（`dist/index.cjs.js`）。WebSocket 上收发 JSON 文本帧，统一格式：

```json
{"cmd": "...", "headers": {"req_id": "..."}, "body": {...}}
```

认证、心跳和回执的响应没有 `cmd`，只有 `headers.req_id` 和 `errcode` / `errmsg`，靠 `req_id` 的前缀区分。
`req_id` 的格式与 SDK 一致：`{前缀}_{毫秒时间戳}_{随机串}`。

| 帧 | 方向 | 内容 |
|---|---|---|
| 认证 | 发 | `cmd=aibot_subscribe`，`body={"bot_id","secret"}`；响应 `errcode=0` 表示成功。凭证无效时实测返回 `errcode=853000`（`invalid bot_id or secret`），收到即判定凭证无效，不再重试 |
| 心跳 | 发 | `cmd=ping`，无 body；每 30 秒一次，连续 2 次没有响应判定连接已死 |
| 消息回调 | 收 | `cmd=aibot_msg_callback`，`body` 见第 4 节 |
| 事件回调 | 收 | `cmd=aibot_event_callback`，`body.event.eventtype`；`disconnected_event` 表示同一机器人在别处建立了新连接 |
| 回执 | 发 | `cmd=aibot_respond_msg`，`req_id` 沿用消息回调的，`body={"msgtype":"stream","stream":{"id","finish":true,"content"}}` |

认证帧发出后 15 秒内没有响应，断开重连。

### 握手请求必须和官方 SDK 一致

实测企业微信长连接服务端对 Go `net/http` 默认发出的握手请求返回 **HTTP 404**，同样的地址用手写的请求
（`Sec-WebSocket-Key` 原样大小写、不带 `User-Agent` 和 `Accept-Encoding`）返回 101。Go 默认的请求有三处不同：
头名被规范成 `Sec-Websocket-Key` / `Sec-Websocket-Version`，带 `User-Agent: Go-http-client/1.1`，带 `Accept-Encoding: gzip`。
`internal/ws` 现在按原样大小写设置头名，并去掉这两个头，`TestHandshakeRequestBytes` 检查发出的原始字节。

另外，短时间内反复握手失败后，服务端会**临时屏蔽该 IP**：之后几分钟里，任何握手请求（包括原本能成功的）都返回 404。
调试时不要用很小的 `reconnect.min_delay_ms` 反复重连。

## 4. 消息处理规则

消息回调的 `body`：`msgid`、`aibotid`、`chattype`（`single` 单聊 / `group` 群聊）、`from.userid`、`msgtype`，
文本在 `text.content`。依次判断：

| 情况 | 处理 |
|---|---|
| `msgid` 已处理过 | 忽略 |
| 不是单聊 | 控制台提示“只支持机器人单聊” |
| 尚未配对 | 内容是配对码：把发送者记为本人，回复“配对成功”，这条消息不键入；否则控制台提示并忽略 |
| 发送者不是配对的本人 | 控制台提示 |
| 不是 `text`（图片、图文混排、语音、文件、视频） | 控制台提示“暂只支持文字消息” |
| 文字为空 | 控制台提示 |
| 键入成功，或锁定目标已关闭（消息丢弃） | 记为已处理；配置了 `ack.reply_text` 时回执 |
| 键入失败 | 不记为已处理，只打印日志 |

企业微信不需要逐条确认，也不会因为键入失败而重推，所以键入失败的消息不会自动重来。
已处理的 `msgid` 逐行追加到 `wecom_acked.ids`（与凭证文件同目录）。

配对码是每次启动时随机生成的 6 位数字，只在本机控制台显示，只认单聊里的文字消息。配对记录（`owner_userid`）和机器人凭证一起存在 `wecom_token.json`；
换了机器人（重新扫码得到不同的 `bot_id`）或 `-relogin` 时清除，需要重新配对。

## 5. 测试

- 单元测试：扫码（等待、成功、生成失败）、消息处理规则、键入失败与丢弃、配对（错误配对码、群聊里的配对码、配对成功与持久化、换机器人后需重新配对、配对回执）；
  用本地假服务器跑通“连接 → 认证 → 收消息 → 键入 → 回执 → 被挤下线”，以及认证失败、心跳无响应判定断线、
  凭证失效（853000 立即、其他错误连续 3 次）后重新扫码。
- 真实服务器：`generate`、`query_result`（未扫码）已调通；扫码页能打开；修正握手请求后，Go 客户端一次握手成功，
  用假凭证认证得到 `{"headers":{"req_id":"aibot_subscribe_…"},"errcode":853000,"errmsg":"invalid bot_id or secret, …"}`，帧格式与代码一致。

## 6. 尚未实测

1. 扫码成功后的完整流程：创建的机器人能否直接收到单聊消息，以及普通成员扫码是否需要管理员审批。
2. 回执（`aibot_respond_msg` 的 stream 消息）在单聊里的显示效果。
3. 公司网络是否放行 `work.weixin.qq.com` 和 `openws.work.weixin.qq.com`。
