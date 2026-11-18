package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wetype/internal/config"
	"wetype/internal/source"
	"wetype/internal/store"
	"wetype/internal/ws"
	"wetype/internal/ws/wstest"
)

func newTestClient(t *testing.T, mutate func(*config.Config)) (*Client, *bytes.Buffer) {
	t.Helper()
	cfg := &config.Config{Source: config.SourceFeishu}
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.LoadFeishu(filepath.Join(t.TempDir(), "feishu_token.json"))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	acked, err := store.LoadAckedIDs(filepath.Join(t.TempDir(), "feishu_acked.ids"), 0)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(cfg, st, acked, log.New(&logs, "", 0))
	c.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return c, &logs
}

// ---------------------------------------------------------------------------
// 扫码
// ---------------------------------------------------------------------------

type fakeRegistration struct {
	mu      sync.Mutex
	polls   int
	begins  int
	pending int    // 前几次 poll 返回 authorization_pending
	final   string // 之后返回的 error；为空表示成功
}

func (f *fakeRegistration) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != registrationPath || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		http.Error(w, "bad request", 400)
		return
	}
	r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Form.Get("action") {
	case "init":
		io.WriteString(w, `{"nonce":"n","supported_auth_methods":["client_secret","private_key_jwt"]}`)
	case "begin":
		f.begins++
		if r.Form.Get("archetype") != "PersonalAgent" || r.Form.Get("auth_method") != "client_secret" {
			http.Error(w, "bad begin", 400)
			return
		}
		fmt.Fprintf(w, `{"device_code":"dc%d","user_code":"AB-CD","verification_uri_complete":"https://open.feishu.cn/page/launcher?user_code=AB-CD","expires_in":3599,"interval":5}`, f.begins)
	case "poll":
		f.polls++
		if r.Form.Get("tp") != "" || !strings.HasPrefix(r.Form.Get("device_code"), "dc") {
			http.Error(w, "bad poll", 400)
			return
		}
		if f.polls <= f.pending {
			w.WriteHeader(http.StatusBadRequest) // 真实接口未扫码时就是非 2xx 加 JSON
			io.WriteString(w, `{"error":"authorization_pending","error_description":"","code":20094}`)
			return
		}
		if f.final != "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":%q}`, f.final)
			f.final = "" // 只失败一次，重新扫码后成功
			f.polls, f.pending = 0, 0
			return
		}
		io.WriteString(w, `{"client_id":"cli_x","client_secret":"sec","user_info":{"open_id":"ou_owner","tenant_brand":"feishu"}}`)
	default:
		http.Error(w, "unknown action", 400)
	}
}

func TestScanSuccessAfterPending(t *testing.T) {
	fake := &fakeRegistration{pending: 2}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c, logs := newTestClient(t, func(cfg *config.Config) { cfg.Login.AutoOpenQR = false })
	c.accountsBase = func(string) string { return srv.URL }

	res, err := c.scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *res != (scanResult{AppID: "cli_x", AppSecret: "sec", Domain: config.FeishuDomain, OpenID: "ou_owner"}) {
		t.Errorf("result = %+v", res)
	}
	if fake.polls != 3 {
		t.Errorf("polls = %d, want 3", fake.polls)
	}
	if !strings.Contains(logs.String(), "user_code=AB-CD") || !strings.Contains(logs.String(), "from=onboard") || strings.Contains(logs.String(), "tp=") {
		t.Errorf("QR link not logged with params: %s", logs.String())
	}
}

func TestScanRestartsAfterDeniedOrExpired(t *testing.T) {
	for _, final := range []string{"access_denied", "expired_token"} {
		fake := &fakeRegistration{final: final}
		srv := httptest.NewServer(fake)
		c, _ := newTestClient(t, func(cfg *config.Config) { cfg.Login.AutoOpenQR = false })
		c.accountsBase = func(string) string { return srv.URL }
		res, err := c.scan(context.Background())
		srv.Close()
		if err != nil || res.AppID != "cli_x" {
			t.Fatalf("%s: res=%+v err=%v", final, res, err)
		}
		if fake.begins != 2 {
			t.Errorf("%s: begins = %d, want 2 (new QR after failure)", final, fake.begins)
		}
	}
}

func TestScanUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"supported_auth_methods":["private_key_jwt"]}`)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	c.accountsBase = func(string) string { return srv.URL }
	if _, err := c.scan(context.Background()); !errors.Is(err, errScanUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestScanSwitchesToLark(t *testing.T) {
	var larkPolls int
	feishuSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.Form.Get("action") {
		case "init":
			io.WriteString(w, `{"supported_auth_methods":["client_secret"]}`)
		case "begin":
			io.WriteString(w, `{"device_code":"dc","verification_uri_complete":"https://open.feishu.cn/page/launcher?user_code=A","interval":1}`)
		case "poll":
			io.WriteString(w, `{"error":"authorization_pending","user_info":{"tenant_brand":"lark"}}`)
		}
	}))
	defer feishuSrv.Close()
	larkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		larkPolls++
		io.WriteString(w, `{"client_id":"cli_l","client_secret":"s","user_info":{"open_id":"ou_l","tenant_brand":"lark"}}`)
	}))
	defer larkSrv.Close()
	c, _ := newTestClient(t, func(cfg *config.Config) { cfg.Login.AutoOpenQR = false })
	c.accountsBase = func(d string) string {
		if d == config.LarkDomain {
			return larkSrv.URL
		}
		return feishuSrv.URL
	}
	res, err := c.scan(context.Background())
	if err != nil || res.Domain != config.LarkDomain || res.AppID != "cli_l" || larkPolls != 1 {
		t.Fatalf("res=%+v err=%v larkPolls=%d", res, err, larkPolls)
	}
}

// ---------------------------------------------------------------------------
// 事件处理
// ---------------------------------------------------------------------------

func messageEventJSON(msgID, chatType, msgType, content, openID string, mentions ...string) []byte {
	var ms []map[string]string
	for _, k := range mentions {
		ms = append(ms, map[string]string{"key": k})
	}
	ev := map[string]any{
		"schema": "2.0",
		"header": map[string]string{"event_id": "ev-" + msgID, "event_type": eventMessageReceive},
		"event": map[string]any{
			"sender": map[string]any{"sender_id": map[string]string{"open_id": openID}, "sender_type": "user"},
			"message": map[string]any{
				"message_id": msgID, "chat_type": chatType, "message_type": msgType,
				"content": content, "mentions": ms,
			},
		},
	}
	b, _ := json.Marshal(ev)
	return b
}

type recordingHandler struct {
	mu   sync.Mutex
	msgs []source.Message
	err  error
}

func (h *recordingHandler) handle(_ context.Context, m source.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return h.err
	}
	h.msgs = append(h.msgs, m)
	return nil
}

func TestProcessEventRules(t *testing.T) {
	c, logs := newTestClient(t, nil)
	h := &recordingHandler{}
	c.handle = h.handle
	c.owner = "ou_owner"

	cases := []struct {
		name    string
		payload []byte
		want    int
		typed   string
		log     string
	}{
		{"单聊文本", messageEventJSON("m1", "p2p", "text", `{"text":"你好 世界"}`, "ou_owner"), 200, "你好 世界", ""},
		{"重复消息", messageEventJSON("m1", "p2p", "text", `{"text":"你好 世界"}`, "ou_owner"), 200, "", ""},
		{"群聊", messageEventJSON("m2", "group", "text", `{"text":"@_user_1 hi"}`, "ou_owner", "@_user_1"), 200, "", "只支持机器人单聊"},
		{"非扫码人", messageEventJSON("m3", "p2p", "text", `{"text":"hi"}`, "ou_other"), 200, "", "非白名单"},
		{"图片", messageEventJSON("m4", "p2p", "image", `{"image_key":"k"}`, "ou_owner"), 200, "", "忽略图片消息"},
		{"提及占位符", messageEventJSON("m5", "p2p", "text", `{"text":"@_user_1  打开文件"}`, "ou_owner", "@_user_1"), 200, "打开文件", ""},
		{"空白", messageEventJSON("m6", "p2p", "text", `{"text":"  "}`, "ou_owner"), 200, "", "空白文字"},
		{"其他事件", []byte(`{"header":{"event_type":"im.chat.member.bot.added_v1"},"event":{}}`), 200, "", ""},
		{"坏数据", []byte(`not json`), 200, "", "解析飞书事件失败"},
	}
	for _, tc := range cases {
		logs.Reset()
		before := len(h.msgs)
		if got := c.processEvent(context.Background(), tc.payload); got != tc.want {
			t.Errorf("%s: code = %d, want %d", tc.name, got, tc.want)
		}
		typed := ""
		if len(h.msgs) > before {
			typed = h.msgs[len(h.msgs)-1].Text
		}
		if typed != tc.typed {
			t.Errorf("%s: typed %q, want %q", tc.name, typed, tc.typed)
		}
		if tc.log != "" && !strings.Contains(logs.String(), tc.log) {
			t.Errorf("%s: log %q missing in %q", tc.name, tc.log, logs.String())
		}
	}
	// 已处理的消息 ID 已持久化
	st, _ := store.LoadAckedIDs(c.acked.Path(), 0)
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5", "m6"} {
		if !st.Has(id) {
			t.Errorf("%s not persisted as acked", id)
		}
	}
}

func TestProcessEventFailureAndDiscard(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.owner = "ou_any"
	h := &recordingHandler{err: errors.New("boom")}
	c.handle = h.handle

	ev := messageEventJSON("f1", "p2p", "text", `{"text":"x"}`, "ou_any")
	if got := c.processEvent(context.Background(), ev); got != http.StatusInternalServerError {
		t.Errorf("failure code = %d, want 500", got)
	}
	if c.acked.Has("f1") {
		t.Error("failed message must not be acked, so that a redelivery is typed")
	}
	h.err = nil
	if got := c.processEvent(context.Background(), ev); got != 200 || len(h.msgs) != 1 {
		t.Errorf("redelivery: code=%d typed=%d", got, len(h.msgs))
	}

	h.err = fmt.Errorf("%w: closed", source.ErrDiscarded)
	ev2 := messageEventJSON("f2", "p2p", "text", `{"text":"y"}`, "ou_any")
	if got := c.processEvent(context.Background(), ev2); got != 200 || !c.acked.Has("f2") {
		t.Errorf("discarded: code=%d acked=%v", got, c.acked.Has("f2"))
	}
}

func TestSenderAllowed(t *testing.T) {
	c, _ := newTestClient(t, nil)
	if c.senderAllowed("anyone") || c.senderAllowed("") {
		t.Error("no owner and no allowlist: should accept nobody")
	}
	c.owner = "ou_owner"
	if c.senderAllowed("anyone") || !c.senderAllowed("ou_owner") {
		t.Error("owner known: only owner allowed")
	}
}

func TestMergeFragments(t *testing.T) {
	c, _ := newTestClient(t, nil)
	mk := func(seq int, data string) *frame {
		return &frame{Headers: []header{{headerMessageID, "big"}, {headerSum, "3"}, {headerSeq, fmt.Sprint(seq)}}, Payload: []byte(data)}
	}
	if _, ok := c.mergeFragments(mk(2, "C")); ok {
		t.Fatal("incomplete")
	}
	if _, ok := c.mergeFragments(mk(0, "A")); ok {
		t.Fatal("incomplete")
	}
	if _, ok := c.mergeFragments(mk(0, "A")); ok { // 重复分片不计数
		t.Fatal("duplicate counted")
	}
	got, ok := c.mergeFragments(mk(1, "B"))
	if !ok || string(got) != "ABC" {
		t.Fatalf("merged = %q ok=%v", got, ok)
	}
	if len(c.fragments) != 0 {
		t.Error("cache not cleared")
	}
	if _, ok := c.mergeFragments(&frame{Headers: []header{{headerSum, "2"}, {headerSeq, "5"}, {headerMessageID, "x"}}}); ok {
		t.Error("out-of-range seq accepted")
	}
	if got, ok := c.mergeFragments(&frame{Payload: []byte("single")}); !ok || string(got) != "single" {
		t.Error("unfragmented payload")
	}
}

func TestEndpointErrors(t *testing.T) {
	for _, tc := range []struct {
		body    string
		badCred bool
	}{
		{`{"code":1000040346,"msg":"app_id is invalid","data":{"URL":""}}`, true},
		{`{"code":514,"msg":"auth failed","data":{}}`, true},
		{`{"code":1,"msg":"system busy","data":{}}`, false},
		{`{"code":0,"msg":"ok","data":{"URL":""}}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) }))
		c, _ := newTestClient(t, nil)
		c.openBase = func(string) string { return srv.URL }
		_, _, err := c.endpoint(context.Background())
		srv.Close()
		if err == nil || errors.Is(err, errBadCredentials) != tc.badCred {
			t.Errorf("%s: err = %v", tc.body, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 端到端：本地假服务器模拟长连接地址接口和长连接
// ---------------------------------------------------------------------------

func TestSessionEndToEnd(t *testing.T) {
	type result struct {
		resp *frame
		err  error
	}
	results := make(chan result, 1)
	var gotEndpointBody map[string]string

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/callback/ws/endpoint", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotEndpointBody)
		fmt.Fprintf(w, `{"code":0,"data":{"URL":"%s/ws?device_id=d1&service_id=42","ClientConfig":{"PingInterval":120}}}`, wstest.URL(srv))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		s := wstest.Accept(t, w, r)
		defer s.Close()
		// 事件分两片发送
		payload := messageEventJSON("om_1", "p2p", "text", `{"text":"来自飞书"}`, "ou_owner")
		half := len(payload) / 2
		for i, part := range [][]byte{payload[:half], payload[half:]} {
			f := &frame{SeqID: 9, LogID: 8, Service: 42, Method: methodData, Payload: part, Headers: []header{
				{headerType, typeEvent}, {headerMessageID, "msg-1"}, {headerSum, "2"}, {headerSeq, fmt.Sprint(i)}, {headerTraceID, "t"},
			}}
			s.Write(true, ws.OpBinary, f.encode())
		}
		op, data, err := s.Read()
		if err != nil || op != ws.OpBinary {
			results <- result{err: fmt.Errorf("read response: op=%d err=%v", op, err)}
			return
		}
		f, err := decodeFrame(data)
		results <- result{resp: f, err: err}
		s.Write(true, ws.OpClose, []byte{0x03, 0xE8})
		s.Read()
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	c, _ := newTestClient(t, nil)
	c.openBase = func(string) string { return srv.URL }
	c.appID, c.appSecret, c.owner = "cli_x", "sec", "ou_owner"
	h := &recordingHandler{}
	c.handle = h.handle

	healthy := false
	err := c.session(context.Background(), func() { healthy = true })
	if !errors.Is(err, ws.ErrClosed) {
		t.Fatalf("session ended with %v", err)
	}
	if !healthy {
		t.Error("onHealthy not called")
	}
	if gotEndpointBody["AppID"] != "cli_x" || gotEndpointBody["AppSecret"] != "sec" {
		t.Errorf("endpoint body = %v", gotEndpointBody)
	}
	if len(h.msgs) != 1 || h.msgs[0].Text != "来自飞书" || h.msgs[0].From != "ou_owner" {
		t.Fatalf("typed = %+v", h.msgs)
	}
	r := <-results
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.resp.SeqID != 9 || r.resp.Service != 42 || r.resp.header(headerBizRT) == "" || r.resp.header(headerSeq) != "1" {
		t.Errorf("response frame = %+v", r.resp)
	}
	var code map[string]int
	if json.Unmarshal(r.resp.Payload, &code); code["code"] != 200 {
		t.Errorf("response payload = %s", r.resp.Payload)
	}
}

func TestSendTextReply(t *testing.T) {
	var tokenCalls int
	var sent map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			tokenCalls++
			io.WriteString(w, `{"code":0,"tenant_access_token":"t-1","expire":7200}`)
		case "/open-apis/im/v1/messages":
			if r.URL.Query().Get("receive_id_type") != "open_id" || r.Header.Get("Authorization") != "Bearer t-1" {
				http.Error(w, "bad", 400)
				return
			}
			json.NewDecoder(r.Body).Decode(&sent)
			io.WriteString(w, `{"code":0}`)
		}
	}))
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	c.openBase = func(string) string { return srv.URL }
	c.appID, c.appSecret = "cli_x", "sec"
	for i := 0; i < 2; i++ {
		if err := c.sendText(context.Background(), "ou_1", "已收到"); err != nil {
			t.Fatal(err)
		}
	}
	if tokenCalls != 1 {
		t.Errorf("token fetched %d times, want cached", tokenCalls)
	}
	if sent["receive_id"] != "ou_1" || sent["msg_type"] != "text" || sent["content"] != `{"text":"已收到"}` {
		t.Errorf("sent = %v", sent)
	}
}

func TestRunRescansWhenStoredCredentialsInvalid(t *testing.T) {
	fake := &fakeRegistration{}
	accounts := httptest.NewServer(fake)
	defer accounts.Close()
	var endpointCalls int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		endpointCalls++
		if body["AppID"] == "cli_old" {
			io.WriteString(w, `{"code":514,"msg":"auth failed","data":{}}`)
			return
		}
		cancel() // 换成扫码得到的新凭证后结束测试
		io.WriteString(w, `{"code":1,"msg":"busy","data":{}}`)
	}))
	defer open.Close()

	c, _ := newTestClient(t, func(cfg *config.Config) { cfg.Login.AutoOpenQR = false })
	c.accountsBase = func(string) string { return accounts.URL }
	c.openBase = func(string) string { return open.URL }
	c.store.SetCredentials("cli_old", "old", config.FeishuDomain, "ou_old")

	if err := c.Run(ctx, (&recordingHandler{}).handle); err != nil {
		t.Fatal(err)
	}
	if id, _, _, owner := c.store.Credentials(); id != "cli_x" || owner != "ou_owner" || endpointCalls != 2 {
		t.Errorf("credentials after rescan: id=%s owner=%s endpointCalls=%d", id, owner, endpointCalls)
	}
}

func TestPingLoopSendsPingFrame(t *testing.T) {
	got := make(chan *frame, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := wstest.Accept(t, w, r)
		defer s.Close()
		_, data, err := s.Read()
		if err != nil {
			got <- nil
			return
		}
		f, _ := decodeFrame(data)
		got <- f
	}))
	defer srv.Close()
	conn, err := ws.Dial(context.Background(), ws.NewTransport(), wstest.URL(srv))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c, _ := newTestClient(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ping, last atomic.Int64
	ping.Store(int64(20 * time.Millisecond))
	last.Store(time.Now().UnixNano())
	var dead atomic.Bool
	go c.pingLoop(ctx, conn, 42, &ping, &last, &dead)
	select {
	case f := <-got:
		if f == nil || f.Method != methodControl || f.Service != 42 || f.header(headerType) != typePing {
			t.Fatalf("ping frame = %+v", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no ping sent")
	}
	if dead.Load() {
		t.Error("connection wrongly marked dead")
	}
}

func TestHandleControlUpdatesPingInterval(t *testing.T) {
	c, _ := newTestClient(t, nil)
	var ping atomic.Int64
	ping.Store(int64(defaultPingInterval))
	c.handleControl(&frame{Headers: []header{{headerType, typePong}}, Payload: []byte(`{"PingInterval":30}`)}, &ping)
	if time.Duration(ping.Load()) != 30*time.Second {
		t.Errorf("ping = %v", time.Duration(ping.Load()))
	}
	c.handleControl(&frame{Headers: []header{{headerType, typePong}}, Payload: []byte(`bad`)}, &ping)
	if time.Duration(ping.Load()) != 30*time.Second {
		t.Error("bad pong payload changed interval")
	}
}

func TestLoadCredentialsFromStoreResetsTokenAndWarnsWithoutOwner(t *testing.T) {
	c, logs := newTestClient(t, nil)
	c.store.SetCredentials("cli_s", "s", config.LarkDomain, "")
	c.tenantToken, c.tokenExpiry = "old-app-token", time.Now().Add(time.Hour)
	if err := c.loadCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.tenantToken != "" {
		t.Error("tenant token of the previous app must be dropped")
	}
	if c.appID != "cli_s" || c.domain != config.LarkDomain || !strings.Contains(logs.String(), "-relogin") {
		t.Errorf("appID=%s domain=%s logs=%s", c.appID, c.domain, logs.String())
	}

	c2, logs2 := newTestClient(t, nil)
	c2.store.SetCredentials("cli_s", "s", "", "ou_me")
	c2.loadCredentials(context.Background())
	if c2.owner != "ou_me" || c2.domain != config.FeishuDomain || strings.Contains(logs2.String(), "警告") {
		t.Errorf("owner=%s domain=%s logs=%s", c2.owner, c2.domain, logs2.String())
	}
}
