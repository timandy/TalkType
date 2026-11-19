# 微信消息来源：协议与实现

`source` 为 `wechat`（默认）时，通过微信官方 iLink Bot API（ClawBot，小龙虾）接收私聊里的文字消息。
代码在 `internal/source/wechat`，凭证读写在 `internal/store/store.go`。

## 1. 整体流程

```
手机微信 ──► iLink 服务端 ──getupdates（长轮询）──► WeType
                                                      │
                                    签名 → 去重 → 键入 → ACK
                                                      │
                                                      ▼
                                     当前光标处，或锁定的窗口 / 终端
```

1. 首次运行：`get_bot_qrcode` 获取二维码 → 微信扫码 → `get_qrcode_status` 确认
2. 登录成功：保存 `bot_token`、`baseurl`、`ilink_bot_id`、扫码人的 `ilink_user_id` 到 `wechat_token.json`
3. 常驻循环：`getupdates` 收消息 → 过滤文本 → Unicode 键入 → 持久化游标
4. 异常恢复：网络错误退避重连；`-14` 清空凭证重新扫码

iLink 地址（`https://ilinkai.weixin.qq.com`）、bot 类型、`channel_version`、签名、长轮询超时（35 秒）都内置在程序里。
协议字段可能随微信客户端版本变化，`channel_version` 需随官方插件升级并复核接口。

## 2. 请求头

`getupdates`、`sendmessage` 等业务请求会携带：

```
Content-Type: application/json
AuthorizationType: ilink_bot_token
Authorization: Bearer <bot_token>
iLink-App-Id: bot
X-WECHAT-UIN: base64(decimal(random_uint32))
```

`bot_token` 形如 `bot_id:secret`。登录成功后服务端可能下发独立的 `baseurl`，后续请求优先使用该地址。

## 3. 签名

官方 iLink 主要使用 Bearer 鉴权；本项目额外实现了 HMAC-SHA256 签名作为加固层，未校验签名的服务端会忽略这些请求头。
每个业务 POST 计算：

```
canonical = METHOD + "\n" + PATH + "\n" + timestamp + "\n" + nonce + "\n" + hex(SHA256(body))
signature = "v1=" + base64(HMAC-SHA256(secret, canonical))
```

写入请求头 `X-ILink-Timestamp`、`X-ILink-Nonce`、`X-ILink-Signature`。签名总是开启，密钥取 bot_token 最后一个 `:` 之后的部分。

## 4. 消息 ACK

iLink 没有单独的 ACK 接口，长轮询游标 `get_updates_buf` 就是 ACK：

1. `getupdates` 返回 `msgs` 与新的 `get_updates_buf`
2. 逐条处理消息，键入成功后将 `message_id` 追加到 `wechat_acked.ids`
3. 一批消息全部成功后才更新并持久化游标
4. 任意一条失败则不推进游标，退避后用旧游标重放
5. 重放时按 `message_id` 去重，已处理的不会再次键入
6. 消息的 `create_time_ms` 早于本次启动时间的是离线期间积压的历史消息：只确认（推进游标）、不键入，一批结束后打印忽略的条数。依据的是本机时钟，本机时钟比服务端快时，启动后头几秒内发的消息可能被当作历史消息

服务端没有返回 `message_id` 时无法去重，重放时可能重复键入。配置 `ack.reply_text` 后，处理成功还会调用 `sendmessage` 回一条用户可见的回执。

## 5. 只接受本人

与微信官方插件 `@tencent-weixin/openclaw-weixin` 一致：只接受扫码人（登录时返回的 `ilink_user_id`，与消息的 `from_user_id`
是同一套 ID）的消息，其他人发来的只在控制台提示。凭证文件里没有扫码人时接受所有人（官方插件对旧账号的做法）。

## 6. 会话过期与重连

- `ret` / `errcode` 为 `-14`，或 HTTP 401/403：删除 `wechat_token.json`、清空内存状态并重新扫码
- 网络错误、HTTP 5xx、响应解析失败：按 `reconnect.min_delay_ms` 起步翻倍，封顶 `reconnect.max_delay_ms`，叠加随机抖动
- 成功收到一次 `getupdates` 响应即重置退避

## 7. 限制

- iLink 目前仅支持私聊，不支持群聊
- ClawBot 名额通常只有一个，重复扫码可能顶掉之前的绑定
- 仅处理文本消息；图片、语音、文件等只在控制台打印“忽略…消息：暂只支持文字消息”，不输入、不回复
