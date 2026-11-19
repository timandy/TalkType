# 飞书消息来源：协议与实现

`source` 设为 `feishu`（或命令行 `-source feishu`）时，通过飞书机器人接收**单聊**里的文字消息。本文记录用到的接口、长连接协议、实现取舍和尚未实测的部分。
代码在 `internal/source/feishu`，凭证读写在 `internal/store/feishu.go`。不依赖飞书 SDK。

## 1. 整体流程

```
首次运行：init → begin（拿到授权链接）→ 用户用飞书扫码确认 → poll 拿到 App ID / App Secret / open_id → 写入 feishu_token.json
常驻运行：POST /callback/ws/endpoint 取长连接地址 → WebSocket 连接 → 收事件帧 → 键入 → 回确认帧
异常恢复：网络错误按 reconnect.* 退避重连；凭证无效时清除本地凭证并重新扫码
```

凭证只能扫码获得：有 `feishu_token.json` 就用它，没有就扫码。不支持在 `config.json` 里手动填写。

## 2. 扫码创建机器人

接口和参数以飞书官方安装工具 `@larksuite/openclaw-lark-tools`（`dist/utils/feishu-auth.js`、`install-prompts.js`）为准，
飞书开放平台没有公开文档。第三方 OpenClaw 插件 `@openclaw/feishu` 用的是同一组接口，但多带了 `tp` 参数（见下文）
（[OpenClaw 飞书接入](https://docs.openclaw.ai/channels/feishu/setup)）。

地址 `https://accounts.feishu.cn/oauth/v1/app/registration`（Lark 为 `accounts.larksuite.com`），
请求体 `application/x-www-form-urlencoded`，是标准的 OAuth 设备码流程：

| 步骤 | 参数 | 返回（已实测） |
|---|---|---|
| init | `action=init` | `supported_auth_methods` 含 `client_secret` 才能继续 |
| begin | `action=begin&archetype=PersonalAgent&auth_method=client_secret&request_user_info=open_id` | `device_code`、`verification_uri_complete`（`https://open.feishu.cn/page/launcher?user_code=XXXX-XXXX`）、`expires_in`（实测 3599）、`interval`（实测 5） |
| poll | `action=poll&device_code=...` | 未扫码时是非 2xx 状态码加 `{"error":"authorization_pending","code":20094}`；成功时有 `client_id`、`client_secret`、`user_info.open_id` |

- 授权链接上只加 `from=onboard`，与官方工具一致。**不要带 `tp=ob_cli_app`**：第三方插件在链接和 poll 上都带了它，实测这样创建的是名为“…的飞书 CLI”的应用，`GET /open-apis/bot/v3/info` 返回 `activate_status: 0`（未激活），机器人收不到任何消息。
- poll 的其他错误：`slow_down` 轮询间隔加 5 秒；`access_denied`、`expired_token` 重新获取链接。
- `user_info.tenant_brand == "lark"` 时改用 Lark 地址继续轮询，凭证记为 `lark` 站点。
- 插件读的是 `expire_in` 字段，实际返回的是 `expires_in`，所以插件一直用默认 600 秒；本程序按 `expires_in` 读。
- 扫码人的 `open_id` 存为 `owner_open_id`，只接受此人的消息。

## 3. 长连接

协议取自官方 Node SDK `@larksuiteoapi/node-sdk` 的 `WSClient`。

### 3.1 取连接地址

`POST https://open.feishu.cn/callback/ws/endpoint`，JSON `{"AppID","AppSecret"}`，请求头 `locale: zh`。
返回 `data.URL`（wss 地址，查询参数带 `service_id`）和 `data.ClientConfig.PingInterval`（秒）。

| code | 含义 | 处理 |
|---|---|---|
| 0 | 成功 | |
| 1000040346 | app_id 无效（已实测） | 凭证无效：清除本地凭证后重新扫码 |
| 514 | 鉴权失败（含义不明确，未实测） | 不当作凭证无效：保留凭证，退避重连 |
| 其他（1 系统繁忙、1000040343 内部错误、1000040350 超出连接数等） | | 退避后重试 |

### 3.2 WebSocket

`internal/ws`（与企业微信共用）是一个只实现所需部分的 RFC 6455 客户端：握手、二进制消息、分片重组、
ping/pong、close，不支持扩展。握手借用 `net/http`：服务端回 101 后，响应的 `Body` 就是可读写的底层连接，
所以 TLS 和系统代理（`HTTPS_PROXY`）与普通请求一致；Transport 关闭了 HTTP/2，WebSocket 握手不能走 HTTP/2。握手请求头按原样大小写发送，不带 `User-Agent` 和 `Accept-Encoding`，原因见 [wecom.md](wecom.md) 第 3 节。
单条消息上限 16 MB。

### 3.3 帧格式 `pbbp2.Frame`

| 字段号 | 名称 | 类型 |
|---|---|---|
| 1 | SeqID | uint64 |
| 2 | LogID | uint64 |
| 3 | service | int32 |
| 4 | method | int32：0 控制帧，1 数据帧 |
| 5 | headers | repeated Header { 1 key, 2 value } |
| 6 | payloadEncoding | string |
| 7 | payloadType | string |
| 8 | payload | bytes |
| 9 | LogIDNew | string |

header key：`type`（`event` / `card` / `ping` / `pong`）、`message_id`、`sum`、`seq`、`trace_id`、`biz_rt`。
编解码在 `frame.go` 里手写，未知字段跳过。

### 3.4 心跳、事件和确认

- 每 `PingInterval` 秒（默认 120）发控制帧 `type=ping`，`service` 填 `service_id`。pong 的 payload 可能带新的
  `PingInterval`，收到后更新。超过“心跳间隔 + 60 秒”没有收到任何帧，判定连接已死并重连。
- 数据帧 `type=event`：`sum > 1` 时按 `message_id` 收齐 `seq` 0～sum-1 再拼接，1 分钟未收齐丢弃。
- 处理完原样回一个帧：headers 追加 `biz_rt`（耗时毫秒），payload 换成 `{"code":200}` 或 `{"code":500}`。
- 重连用 `reconnect.min_delay_ms` / `max_delay_ms` 退避加抖动，每次重连都重新取地址。

## 4. 消息处理规则

只处理 `im.message.receive_v1`，依次判断：

| 情况 | 处理 | 返回 |
|---|---|---|
| 发送者不是用户（`sender_type != user`） | 忽略 | 200 |
| `message_id` 已处理过 | 忽略（重推去重） | 200 |
| 不是单聊（`chat_type != p2p`） | 控制台提示“只支持机器人单聊” | 200 |
| 发送者不是扫码人 | 控制台提示 | 200 |
| 不是 `text`（图片、富文本 `post` 等） | 控制台提示“暂只支持文字消息” | 200 |
| 文字为空（去掉 `@_user_1` 这类提及占位符后） | 控制台提示 | 200 |
| 键入成功，或锁定目标已关闭（消息丢弃） | 记为已处理；配置了 `ack.reply_text` 时回执 | 200 |
| 键入失败 | 不记为已处理，期望飞书重推 | 500 |

已处理的 `message_id` 逐行追加到 `feishu_acked.ids`（与凭证文件同目录），保留最近 `ack.max_acked_ids` 条，文件超过两倍时压缩。

回执：先用 App ID / Secret 换 `tenant_access_token`（`/open-apis/auth/v3/tenant_access_token/internal`，缓存到过期前 5 分钟），
再调 `POST /open-apis/im/v1/messages?receive_id_type=open_id` 发文本。

## 5. 与微信来源的差异

| 方面 | 微信 iLink | 飞书 |
|---|---|---|
| 收消息 | HTTP 长轮询 | WebSocket 长连接 |
| ACK | 游标，键入成功才推进 | 每条事件回确认帧，没有游标 |
| 离线期间的消息 | 游标保证上线后补收 | 取决于飞书是否重推，未实测 |
| 会话 | 私聊 | 只处理机器人单聊 |
| 只接受谁 | 扫码人（`ilink_user_id`） | 扫码人（`open_id`） |

飞书没有 HTTP 长轮询接口；事件推送只有 Webhook（需要公网地址）和长连接两种，因此选长连接。

## 6. 测试

- 单元测试：帧编解码、WebSocket 客户端（本地测试服务器，含分片、ping、close、握手失败）、扫码流程
  （等待、拒绝、过期、切换 Lark、不支持扫码）、事件处理规则、分片重组、长连接地址错误分类、回执、
  凭证无效时的处理，以及用本地假服务器跑通“取地址 → 连接 → 收分片事件 → 键入 → 回确认帧”的端到端测试。
- 真实服务器：init、begin、poll（未扫码）三个接口已调通；长连接地址接口用无效 App ID 调用，返回格式与 SDK 一致；
  程序以 `-source feishu` 运行，能拿到真实授权链接并进入等待扫码。

## 7. 实测结论与尚未实测的部分

已实测：

- **企业组织账号**：扫码能创建应用并拿到凭证，长连接也能建立；但应用要等企业管理员审核，审核前在飞书里找不到这个机器人，无法发消息。
  企业不放行时只能请管理员审批。
- **`tp=ob_cli_app` 参数**：会创建未激活的“飞书 CLI”类应用，收不到消息（见第 2 节），已改为官方工具的参数。

尚未实测：

1. **改用官方参数后，个人账号扫码创建的机器人能否直接收到单聊消息并键入。** 需要扫码后查看 `bot/v3/info` 的 `activate_status`，
   并在日志里看到“收到文本消息”。
2. **授权链接在电脑浏览器里的表现。** 程序只打印链接并用浏览器打开，不在终端画二维码。
3. **键入失败回 500 是否会触发重推**，以及程序离线期间的消息是否补发。
4. **回执**：扫码创建的应用是否有发消息权限。

