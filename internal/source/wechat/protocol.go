package wechat

// iLink 协议结构，字段名与官方 getupdates / sendmessage 的 JSON 保持一致。

type BaseInfo struct {
	ChannelVersion string `json:"channel_version,omitempty"`
}

type TextItem struct {
	Text string `json:"text,omitempty"`
}

type MessageItem struct {
	Type     int       `json:"type,omitempty"`
	MsgID    string    `json:"msg_id,omitempty"`
	TextItem *TextItem `json:"text_item,omitempty"`
}

type WeixinMessage struct {
	Seq          int64         `json:"seq,omitempty"`
	MessageID    int64         `json:"message_id,omitempty"`
	FromUserID   string        `json:"from_user_id"`
	ToUserID     string        `json:"to_user_id,omitempty"`
	ClientID     string        `json:"client_id,omitempty"`
	CreateTimeMs int64         `json:"create_time_ms,omitempty"`
	SessionID    string        `json:"session_id,omitempty"`
	MessageType  int           `json:"message_type,omitempty"`
	MessageState int           `json:"message_state,omitempty"`
	ItemList     []MessageItem `json:"item_list,omitempty"`
	ContextToken string        `json:"context_token,omitempty"`
}

type GetUpdatesReq struct {
	GetUpdatesBuf string    `json:"get_updates_buf"`
	BaseInfo      *BaseInfo `json:"base_info,omitempty"`
}

type GetUpdatesResp struct {
	Ret                  int             `json:"ret,omitempty"`
	ErrCode              int             `json:"errcode,omitempty"`
	ErrMsg               string          `json:"errmsg,omitempty"`
	Msgs                 []WeixinMessage `json:"msgs,omitempty"`
	GetUpdatesBuf        string          `json:"get_updates_buf,omitempty"`
	SyncBuf              string          `json:"sync_buf,omitempty"`
	LongPollingTimeoutMs int64           `json:"longpolling_timeout_ms,omitempty"`
}

type SendMessageReq struct {
	Msg      *WeixinMessage `json:"msg,omitempty"`
	BaseInfo *BaseInfo      `json:"base_info,omitempty"`
}

type QRCodeResponse struct {
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

type QRStatusResponse struct {
	Status       string `json:"status"` // wait / scaned / confirmed / expired / scaned_but_redirect
	BotToken     string `json:"bot_token,omitempty"`
	ILinkBotID   string `json:"ilink_bot_id,omitempty"`
	BaseURL      string `json:"baseurl,omitempty"`
	ILinkUserID  string `json:"ilink_user_id,omitempty"`
	RedirectHost string `json:"redirect_host,omitempty"`
}
