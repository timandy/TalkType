package ilink

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wetype/internal/config"
	"wetype/internal/relay"
	"wetype/internal/store"
)

func newTestClient(t *testing.T, mutate func(*Client)) *Client {
	t.Helper()
	cfg := &config.Config{Ack: config.AckConfig{MaxAckedIDs: 10}}
	st, err := store.Load(filepath.Join(t.TempDir(), "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	acked, err := store.LoadAckedIDs(filepath.Join(t.TempDir(), "wechat_acked.ids"), 10)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(cfg, st, acked, log.New(io.Discard, "", 0))
	c.set.baseURL = "https://default.example.com/"
	c.set.channelVersion = "1.0"
	if mutate != nil {
		mutate(c)
	}
	return c
}

func textMsg(text string) []MessageItem {
	return []MessageItem{{Type: itemTypeText, TextItem: &TextItem{Text: text}}}
}

func TestExtractText(t *testing.T) {
	tests := []struct {
		name string
		msg  *WeixinMessage
		want string
	}{
		{"empty", &WeixinMessage{}, ""},
		{"single", &WeixinMessage{ItemList: textMsg("  hi  ")}, "hi"},
		{"multiple joined by newline", &WeixinMessage{ItemList: []MessageItem{
			{Type: itemTypeText, TextItem: &TextItem{Text: "a"}},
			{Type: itemTypeText, TextItem: &TextItem{Text: "b"}},
		}}, "a\nb"},
		{"skips non-text, nil and blank items", &WeixinMessage{ItemList: []MessageItem{
			{Type: 2, TextItem: &TextItem{Text: "image"}},
			{Type: itemTypeText},
			{Type: itemTypeText, TextItem: &TextItem{Text: "   "}},
			{Type: itemTypeText, TextItem: &TextItem{Text: "ok"}},
		}}, "ok"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractText(tc.msg); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTokenSecret(t *testing.T) {
	tests := map[string]string{
		"abc:def":   "def",
		"a:b:c":     "c",
		"plain":     "plain",
		"trailing:": "trailing:",
		"":          "",
	}
	for in, want := range tests {
		if got := tokenSecret(in); got != want {
			t.Errorf("tokenSecret(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSenderAllowed(t *testing.T) {
	c := newTestClient(t, nil)
	if !c.senderAllowed("anyone") {
		t.Error("no scanner recorded (legacy credentials): should allow everyone, like the official plugin")
	}
	c.store.SetSession("tok", "bot", "me", "")
	if !c.senderAllowed("me") || c.senderAllowed("other") {
		t.Error("only the scanner should be allowed")
	}
}

func TestBaseURL(t *testing.T) {
	c := newTestClient(t, nil)
	if got := c.baseURL(); got != "https://default.example.com" {
		t.Errorf("got %q", got)
	}
	c.store.SetSession("t", "b", "u", "https://stored.example.com/")
	if got := c.baseURL(); got != "https://stored.example.com" {
		t.Errorf("got %q", got)
	}
}

func TestMarkAcked(t *testing.T) {
	c := newTestClient(t, nil)
	c.markAcked(0)
	if c.acked.Has("0") {
		t.Error("key 0 should be ignored")
	}
	c.markAcked(5)
	if !c.acked.Has("5") {
		t.Error("key 5 should be acked")
	}
}

func TestSignRequest(t *testing.T) {
	body := []byte(`{"a":1}`)

	t.Run("signature matches canonical form", func(t *testing.T) {
		c := newTestClient(t, nil)
		c.store.SetSession("bot:s3cret", "", "", "") // 密钥取 bot_token 最后一个冒号之后的部分
		req, _ := http.NewRequest(http.MethodPost, "https://x.com/ilink/bot/getupdates", nil)
		c.signRequest(req, body)

		ts, nonce, sig := req.Header.Get("X-ILink-Timestamp"), req.Header.Get("X-ILink-Nonce"), req.Header.Get("X-ILink-Signature")
		if ts == "" || nonce == "" || !strings.HasPrefix(sig, "v1=") {
			t.Fatalf("headers missing: ts=%q nonce=%q sig=%q", ts, nonce, sig)
		}
		sum := sha256.Sum256(body)
		canonical := strings.Join([]string{"POST", "/ilink/bot/getupdates", ts, nonce, hex.EncodeToString(sum[:])}, "\n")
		mac := hmac.New(sha256.New, []byte("s3cret"))
		mac.Write([]byte(canonical))
		want := "v1=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if sig != want {
			t.Errorf("signature = %q, want %q", sig, want)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		c := newTestClient(t, func(c *Client) { c.set.sign = false })
		req, _ := http.NewRequest(http.MethodPost, "https://x.com/p", nil)
		c.signRequest(req, body)
		if req.Header.Get("X-ILink-Signature") != "" {
			t.Error("should not sign when disabled")
		}
	})

	t.Run("no token", func(t *testing.T) {
		c := newTestClient(t, nil)
		req, _ := http.NewRequest(http.MethodPost, "https://x.com/p", nil)
		c.signRequest(req, body)
		if req.Header.Get("X-ILink-Signature") != "" {
			t.Error("should not sign without token")
		}
	})
}

func TestDoPost(t *testing.T) {
	var gotPath, gotAuth, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotCT = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ret":0}`))
	}))
	defer srv.Close()

	c := newTestClient(t, func(c *Client) { c.set.baseURL = srv.URL })
	c.store.SetSession("mytoken", "", "", "")
	data, err := c.doPost(context.Background(), "/ilink/bot/x", map[string]int{"k": 1}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"ret":0}` || gotPath != "/ilink/bot/x" || gotAuth != "Bearer mytoken" || gotCT != "application/json" || string(gotBody) != `{"k":1}` {
		t.Errorf("unexpected request/response: path=%q auth=%q ct=%q body=%q data=%q", gotPath, gotAuth, gotCT, gotBody, data)
	}
}

func TestDoPost_StatusErrors(t *testing.T) {
	tests := []struct {
		status      int
		wantExpired bool
	}{
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusInternalServerError, false},
	}
	for _, tc := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte("nope"))
		}))
		c := newTestClient(t, func(c *Client) { c.set.baseURL = srv.URL })
		_, err := c.doPost(context.Background(), "p", struct{}{}, time.Second)
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		if errors.Is(err, ErrSessionExpired) != tc.wantExpired {
			t.Errorf("status %d: ErrSessionExpired = %v, want %v (err=%v)", tc.status, !tc.wantExpired, tc.wantExpired, err)
		}
	}
}

func TestDoGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"status":"wait"}`))
	}))
	defer srv.Close()
	c := newTestClient(t, nil)

	var out QRStatusResponse
	if err := c.doGet(context.Background(), srv.URL+"/ok", &out, time.Second); err != nil {
		t.Fatal(err)
	}
	if out.Status != "wait" {
		t.Errorf("status = %q", out.Status)
	}
	if err := c.doGet(context.Background(), srv.URL+"/bad", &out, time.Second); err == nil {
		t.Error("expected error for 502")
	}
}

func TestGetUpdates(t *testing.T) {
	var gotReq GetUpdatesReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_, _ = w.Write([]byte(`{"get_updates_buf":"nb","msgs":[{"from_user_id":"u","message_id":3}]}`))
	}))
	defer srv.Close()
	c := newTestClient(t, func(c *Client) { c.set.baseURL = srv.URL; c.set.httpTimeout = 5 * time.Second })

	resp, err := c.getUpdates(context.Background(), "oldbuf", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.GetUpdatesBuf != "oldbuf" || gotReq.BaseInfo == nil || gotReq.BaseInfo.ChannelVersion != "1.0" {
		t.Errorf("request = %+v", gotReq)
	}
	if resp.GetUpdatesBuf != "nb" || len(resp.Msgs) != 1 || resp.Msgs[0].MessageID != 3 {
		t.Errorf("response = %+v", resp)
	}
}

func TestGetUpdates_ContextCancelled(t *testing.T) {
	c := newTestClient(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.getUpdates(ctx, "", time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestGetUpdates_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := newTestClient(t, func(c *Client) { c.set.baseURL = srv.URL })
	if _, err := c.getUpdates(context.Background(), "", time.Second); err == nil {
		t.Error("expected parse error")
	}
}

func TestSendText(t *testing.T) {
	var gotPath string
	var got SendMessageReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newTestClient(t, func(c *Client) { c.set.baseURL = srv.URL })

	if err := c.sendText(context.Background(), "user1", "pong", "ctx"); err != nil {
		t.Fatal(err)
	}
	m := got.Msg
	if gotPath != "/ilink/bot/sendmessage" || m == nil {
		t.Fatalf("path=%q msg=%v", gotPath, m)
	}
	if m.ToUserID != "user1" || m.MessageType != msgTypeBot || m.MessageState != stateFinish || m.ContextToken != "ctx" ||
		!strings.HasPrefix(m.ClientID, "wetype-") || len(m.ItemList) != 1 || m.ItemList[0].TextItem.Text != "pong" {
		t.Errorf("msg = %+v", m)
	}
}

// 以下用例都不会走到 wintype 键盘输入，避免在测试中真实模拟按键。
func TestHandleMessage_SkipsWithoutTyping(t *testing.T) {
	ctx := context.Background()

	t.Run("non-user message", func(t *testing.T) {
		c := newTestClient(t, nil)
		if !c.handleMessage(ctx, &WeixinMessage{MessageType: msgTypeBot, FromUserID: "u", MessageID: 1, ItemList: textMsg("x")}) {
			t.Error("want true")
		}
		if c.acked.Has("1") {
			t.Error("non-user message should not be acked")
		}
	})

	t.Run("empty sender", func(t *testing.T) {
		c := newTestClient(t, nil)
		if !c.handleMessage(ctx, &WeixinMessage{MessageType: msgTypeUser, MessageID: 1, ItemList: textMsg("x")}) {
			t.Error("want true")
		}
	})

	t.Run("already acked", func(t *testing.T) {
		c := newTestClient(t, nil)
		c.acked.Add("9")
		if !c.handleMessage(ctx, &WeixinMessage{MessageType: msgTypeUser, FromUserID: "u", MessageID: 9, ContextToken: "ct", ItemList: textMsg("x")}) {
			t.Error("want true")
		}
		if _, ok := c.store.ContextTokens["u"]; ok {
			t.Error("acked message should short-circuit before storing context token")
		}
	})

	t.Run("no text acks and stores context token", func(t *testing.T) {
		c := newTestClient(t, nil)
		msg := &WeixinMessage{MessageType: msgTypeUser, FromUserID: "u", Seq: 4, ContextToken: "ct"}
		if !c.handleMessage(ctx, msg) {
			t.Error("want true")
		}
		if !c.acked.Has("4") {
			t.Error("Seq should be used as key when MessageID is 0")
		}
		if c.store.ContextTokens["u"] != "ct" {
			t.Error("context token not stored")
		}
	})

	t.Run("sender is not the scanner", func(t *testing.T) {
		c := newTestClient(t, nil)
		c.store.SetSession("tok", "bot", "vip", "")
		if !c.handleMessage(ctx, &WeixinMessage{MessageType: msgTypeUser, FromUserID: "other", MessageID: 2, ItemList: textMsg("x")}) {
			t.Error("want true")
		}
		if !c.acked.Has("2") {
			t.Error("ignored message should be acked")
		}
	})
}

func TestPollLoop_SessionExpiredAndErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr func(error) bool
	}{
		{"ret -14", `{"ret":-14}`, func(e error) bool { return errors.Is(e, ErrSessionExpired) }},
		{"errcode -14", `{"errcode":-14}`, func(e error) bool { return errors.Is(e, ErrSessionExpired) }},
		{"other failure", `{"ret":5,"errmsg":"boom"}`, func(e error) bool { return e != nil && strings.Contains(e.Error(), "boom") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := newTestClient(t, func(c *Client) {
				c.set.baseURL = srv.URL
				c.set.pollTimeout = time.Second
				c.set.httpTimeout = 5 * time.Second
			})
			healthy := false
			err := c.pollLoop(context.Background(), func() { healthy = true })
			if !tc.wantErr(err) {
				t.Errorf("err = %v", err)
			}
			if healthy {
				t.Error("onHealthy must not be called on error responses")
			}
		})
	}
}

func TestPollLoop_AdvancesCursorAndPersists(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// 非用户消息会被直接跳过，不触发键盘输入
			_, _ = w.Write([]byte(`{"sync_buf":"cursor2","msgs":[{"message_type":2,"from_user_id":"bot"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ret":-14}`))
	}))
	defer srv.Close()
	c := newTestClient(t, func(c *Client) {
		c.set.baseURL = srv.URL
		c.set.pollTimeout = time.Second
		c.set.httpTimeout = 5 * time.Second
	})

	healthy := 0
	err := c.pollLoop(context.Background(), func() { healthy++ })
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("err = %v", err)
	}
	if healthy != 1 || c.store.Buf() != "cursor2" {
		t.Errorf("healthy=%d buf=%q", healthy, c.store.Buf())
	}
	reloaded, err := store.Load(c.store.Path())
	if err != nil || reloaded.Buf() != "cursor2" {
		t.Errorf("cursor not persisted: %v %v", err, reloaded)
	}
}

type recorder struct {
	msgs []relay.Message
	err  error
}

func (r *recorder) handle(_ context.Context, m relay.Message) error {
	r.msgs = append(r.msgs, m)
	return r.err
}

func userMsg(id int64, text string) *WeixinMessage {
	return &WeixinMessage{MessageType: msgTypeUser, FromUserID: "u", MessageID: id, ItemList: textMsg(text)}
}

func TestName(t *testing.T) {
	if newTestClient(t, nil).Name() != "微信" {
		t.Error("unexpected name")
	}
}

func TestHandleMessage_Delivers(t *testing.T) {
	c := newTestClient(t, nil)
	rec := &recorder{}
	c.handle = rec.handle
	if !c.handleMessage(context.Background(), userMsg(1, "hello")) {
		t.Fatal("want true")
	}
	if len(rec.msgs) != 1 || rec.msgs[0].Text != "hello" || rec.msgs[0].From != "u" || !c.acked.Has("1") {
		t.Errorf("msgs=%v acked=%v", rec.msgs, c.acked.Has("1"))
	}
}

func TestHandleMessage_DeliverFailureKeepsCursor(t *testing.T) {
	c := newTestClient(t, nil)
	c.handle = (&recorder{err: errors.New("boom")}).handle
	if c.handleMessage(context.Background(), userMsg(1, "hello")) {
		t.Error("want false so the batch is retried")
	}
	if c.acked.Has("1") {
		t.Error("failed message must not be acked")
	}
}

func TestHandleMessage_DiscardedIsAcked(t *testing.T) {
	c := newTestClient(t, nil)
	c.handle = (&recorder{err: fmt.Errorf("%w: closed", relay.ErrDiscarded)}).handle
	if !c.handleMessage(context.Background(), userMsg(1, "hello")) {
		t.Error("want true so the poll loop does not retry forever")
	}
	if !c.acked.Has("1") {
		t.Error("discarded message should be acked")
	}
}

func TestHandleMessage_SendsReplyAfterDelivery(t *testing.T) {
	var got SendMessageReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newTestClient(t, func(c *Client) { c.set.baseURL = srv.URL; c.cfg.Ack.ReplyText = " 收到 " })
	c.handle = (&recorder{}).handle
	if !c.handleMessage(context.Background(), userMsg(1, "hello")) {
		t.Fatal("want true")
	}
	if got.Msg == nil || got.Msg.ToUserID != "u" || got.Msg.ItemList[0].TextItem.Text != "收到" {
		t.Errorf("reply = %+v", got.Msg)
	}
}

func TestDescribeItems(t *testing.T) {
	tests := []struct {
		items []MessageItem
		want  string
	}{
		{[]MessageItem{{Type: itemTypeVoice}}, "语音消息"},
		{[]MessageItem{{Type: itemTypeImage}, {Type: itemTypeImage}, {Type: itemTypeFile}}, "图片、文件消息"},
		{[]MessageItem{{Type: 99}}, "未知类型 99消息"},
		{[]MessageItem{{Type: itemTypeText, TextItem: &TextItem{Text: "  "}}}, "空白文字消息"},
		{nil, "空消息"},
	}
	for _, tc := range tests {
		if got := describeItems(tc.items); got != tc.want {
			t.Errorf("describeItems(%+v) = %q, want %q", tc.items, got, tc.want)
		}
	}
}

// 非文字消息只在控制台提示：不投递、不回复，并且 ACK。
func TestHandleMessage_NonTextIsLoggedOnly(t *testing.T) {
	var replies int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replies++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	var logBuf strings.Builder
	c := newTestClient(t, func(c *Client) { c.set.baseURL = srv.URL; c.cfg.Ack.ReplyText = "收到" })
	c.logger = log.New(&logBuf, "", 0)
	rec := &recorder{}
	c.handle = rec.handle

	msg := &WeixinMessage{MessageType: msgTypeUser, FromUserID: "u", MessageID: 7, ItemList: []MessageItem{{Type: itemTypeVoice}}}
	if !c.handleMessage(context.Background(), msg) {
		t.Fatal("want true")
	}
	if len(rec.msgs) != 0 || replies != 0 {
		t.Errorf("non-text message must not be delivered or replied: delivered=%d replies=%d", len(rec.msgs), replies)
	}
	if !c.acked.Has("7") {
		t.Error("non-text message should be acked")
	}
	if out := logBuf.String(); !strings.Contains(out, "忽略语音消息") || !strings.Contains(out, "暂只支持文字消息") {
		t.Errorf("log = %q", out)
	}
}
