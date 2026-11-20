package wecom

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
	"testing"
	"time"

	"github.com/timandy/talktype/internal/config"
	"github.com/timandy/talktype/internal/source"
	"github.com/timandy/talktype/internal/store"
	"github.com/timandy/talktype/internal/ws"
	"github.com/timandy/talktype/internal/ws/wstest"
)

func newTestClient(t *testing.T, mutate func(*config.Config)) (*Client, *bytes.Buffer) {
	t.Helper()
	cfg := &config.Config{Source: config.SourceWeCom}
	cfg.Login.AutoOpenQR = false
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, err := store.LoadWeCom(filepath.Join(dir, "wecom_token.json"))
	if err != nil {
		t.Fatal(err)
	}
	acked, err := store.LoadAckedIDs(filepath.Join(dir, "wecom_acked.ids"), 0)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	c := NewClient(cfg, st, acked, log.New(&logs, "", 0))
	c.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return c, &logs
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

func (h *recordingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}

// ---------------------------------------------------------------------------
// 扫码
// ---------------------------------------------------------------------------

type fakeQC struct {
	mu        sync.Mutex
	generates int
	polls     int
	pending   int // 前几次轮询返回 init
}

func (f *fakeQC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/ai/qc/generate":
		if r.URL.Query().Get("source") != registrationSource {
			http.Error(w, "bad source", 400)
			return
		}
		f.generates++
		fmt.Fprintf(w, `{"data":{"scode":"sc%d","auth_url":"https://work.weixin.qq.com/ai/qc/c?s=sc%d"}}`, f.generates, f.generates)
	case "/ai/qc/query_result":
		f.polls++
		if !strings.HasPrefix(r.URL.Query().Get("scode"), "sc") {
			http.Error(w, "bad scode", 400)
			return
		}
		if f.polls <= f.pending {
			io.WriteString(w, `{"data":{"status":"init"}}`)
			return
		}
		io.WriteString(w, `{"data":{"status":"success","bot_info":{"botid":"bot_x","secret":"sec_x"}}}`)
	default:
		http.NotFound(w, r)
	}
}

func TestScanSuccess(t *testing.T) {
	qc := &fakeQC{pending: 2}
	srv := httptest.NewServer(qc)
	defer srv.Close()
	c, logs := newTestClient(t, nil)
	c.qcBase = srv.URL

	id, secret, err := c.scan(context.Background())
	if err != nil || id != "bot_x" || secret != "sec_x" {
		t.Fatalf("id=%s secret=%s err=%v", id, secret, err)
	}
	if qc.polls != 3 || qc.generates != 1 {
		t.Errorf("polls=%d generates=%d", qc.polls, qc.generates)
	}
	if !strings.Contains(logs.String(), "/ai/qc/gen?") || !strings.Contains(logs.String(), "scode=sc1") {
		t.Errorf("scan page not logged: %s", logs.String())
	}
}

func TestScanGenerateError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 502) }))
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	c.qcBase = srv.URL
	if _, _, err := c.scan(context.Background()); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// 消息处理
// ---------------------------------------------------------------------------

func callback(reqID, msgID, chatType, msgType, content, from string) *frame {
	body := map[string]any{"msgid": msgID, "chattype": chatType, "msgtype": msgType, "from": map[string]string{"userid": from}}
	if msgType == "text" {
		body["text"] = map[string]string{"content": content}
	}
	b, _ := json.Marshal(body)
	f := &frame{Cmd: cmdCallback, Body: b}
	f.Headers.ReqID = reqID
	return f
}

func TestHandleMessageRules(t *testing.T) {
	c, logs := newTestClient(t, nil)
	c.store.SetOwner("zhangsan")
	h := &recordingHandler{}
	c.handle = h.handle

	cases := []struct {
		name  string
		f     *frame
		typed string
		log   string
	}{
		{"单聊文本", callback("r1", "m1", "single", "text", " 你好 ", "zhangsan"), "你好", ""},
		{"重复消息", callback("r1", "m1", "single", "text", "你好", "zhangsan"), "", ""},
		{"群聊", callback("r2", "m2", "group", "text", "@机器人 hi", "zhangsan"), "", "只支持机器人单聊"},
		{"非本人", callback("r3", "m3", "single", "text", "hi", "lisi"), "", "非本人"},
		{"语音", callback("r4", "m4", "single", "voice", "", "zhangsan"), "", "忽略语音消息"},
		{"空白", callback("r5", "m5", "single", "text", "  ", "zhangsan"), "", "空白文字"},
	}
	for _, tc := range cases {
		logs.Reset()
		before := h.count()
		c.handleMessage(context.Background(), nil, tc.f)
		typed := ""
		if h.count() > before {
			typed = h.msgs[len(h.msgs)-1].Text
		}
		if typed != tc.typed {
			t.Errorf("%s: typed %q, want %q", tc.name, typed, tc.typed)
		}
		if tc.log != "" && !strings.Contains(logs.String(), tc.log) {
			t.Errorf("%s: log %q missing in %q", tc.name, tc.log, logs.String())
		}
	}
	reloaded, _ := store.LoadAckedIDs(c.acked.Path(), 0)
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		if !reloaded.Has(id) {
			t.Errorf("%s not persisted", id)
		}
	}
}

func TestHandleMessageFailureNotAcked(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.store.SetOwner("u")
	h := &recordingHandler{err: errors.New("boom")}
	c.handle = h.handle
	c.handleMessage(context.Background(), nil, callback("r", "f1", "single", "text", "x", "u"))
	if c.acked.Has("f1") {
		t.Error("failed message must not be acked")
	}
	h.err = fmt.Errorf("%w: closed", source.ErrDiscarded)
	c.handleMessage(context.Background(), nil, callback("r", "f2", "single", "text", "x", "u"))
	if !c.acked.Has("f2") {
		t.Error("discarded message should be acked")
	}
}

func TestLoadCredentialsPairingState(t *testing.T) {
	// 没有白名单、没有配对记录：生成配对码
	c, logs := newTestClient(t, nil)
	c.store.SetCredentials("b", "s")
	if err := c.loadCredentials(context.Background(), false); err != nil || c.botID != "b" {
		t.Fatalf("err=%v botID=%s", err, c.botID)
	}
	if len(c.pairCode) != 6 || strings.Trim(c.pairCode, "0123456789") != "" {
		t.Errorf("pairCode = %q", c.pairCode)
	}
	c.logPairing()
	if !strings.Contains(logs.String(), c.pairCode) {
		t.Errorf("pair code not logged: %s", logs.String())
	}

	// 已配对：不再生成配对码
	c2, logs2 := newTestClient(t, nil)
	c2.store.SetCredentials("b", "s")
	c2.store.SetOwner("me")
	c2.loadCredentials(context.Background(), false)
	if c2.pairCode != "" || !strings.Contains(logs2.String(), "userid=me") {
		t.Errorf("pairCode=%q logs=%s", c2.pairCode, logs2.String())
	}
}

func TestPairing(t *testing.T) {
	c, logs := newTestClient(t, nil)
	c.pairCode = "123456"
	h := &recordingHandler{}
	c.handle = h.handle
	ctx := context.Background()

	// 配对前：任何消息都不键入，包括错误的配对码和非文字消息
	c.handleMessage(ctx, nil, callback("r1", "m1", "single", "text", "hello", "lisi"))
	c.handleMessage(ctx, nil, callback("r2", "m2", "single", "text", "654321", "lisi"))
	c.handleMessage(ctx, nil, callback("r3", "m3", "single", "voice", "", "lisi"))
	if h.count() != 0 || c.store.Owner() != "" || !strings.Contains(logs.String(), "尚未配对") {
		t.Fatalf("typed=%d owner=%q logs=%s", h.count(), c.store.Owner(), logs.String())
	}
	// 群聊里发配对码不算
	c.handleMessage(ctx, nil, callback("r4", "m4", "group", "text", "123456", "lisi"))
	if c.store.Owner() != "" {
		t.Fatal("pairing from group chat accepted")
	}

	// 发对配对码：记为本人并持久化，这条消息本身不键入
	c.handleMessage(ctx, nil, callback("r5", "m5", "single", "text", " 123456 ", "zhangsan"))
	if c.store.Owner() != "zhangsan" || c.pairCode != "" || h.count() != 0 {
		t.Fatalf("owner=%q pairCode=%q typed=%d", c.store.Owner(), c.pairCode, h.count())
	}
	reloaded, _ := store.LoadWeCom(c.store.Path())
	if reloaded.Owner() != "zhangsan" {
		t.Error("owner not persisted")
	}

	// 配对后：只接受本人
	c.handleMessage(ctx, nil, callback("r6", "m6", "single", "text", "来自本人", "zhangsan"))
	c.handleMessage(ctx, nil, callback("r7", "m7", "single", "text", "来自别人", "lisi"))
	if h.count() != 1 || h.msgs[0].Text != "来自本人" {
		t.Errorf("typed = %+v", h.msgs)
	}
}

func TestPairingRepliesOverConnection(t *testing.T) {
	fw := &fakeWS{t: t, done: make(chan error, 1)}
	fw.script = func(s *wstest.ServerConn) error {
		msg, _ := json.Marshal(map[string]any{
			"cmd": cmdCallback, "headers": map[string]string{"req_id": "cb-pair"},
			"body": map[string]any{"msgid": "p1", "chattype": "single", "msgtype": "text", "from": map[string]string{"userid": "zhangsan"}, "text": map[string]string{"content": "246810"}},
		})
		s.Write(true, ws.OpText, msg)
		reply, err := readJSON(s)
		if err != nil {
			return err
		}
		stream := reply["body"].(map[string]any)["stream"].(map[string]any)
		if reply["headers"].(map[string]any)["req_id"] != "cb-pair" || !strings.Contains(stream["content"].(string), "配对成功") {
			return fmt.Errorf("bad pairing reply: %v", reply)
		}
		s.Write(true, ws.OpText, []byte(`{"cmd":"aibot_event_callback","headers":{"req_id":"ev"},"body":{"event":{"eventtype":"disconnected_event"}}}`))
		return nil
	}
	srv := httptest.NewServer(fw)
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	c.wsURL = wstest.URL(srv)
	c.botID, c.secret, c.pairCode = "b", "s", "246810"
	c.handle = (&recordingHandler{}).handle
	if err := c.session(context.Background(), func() {}); !errors.Is(err, errReplaced) {
		t.Fatalf("session: %v", err)
	}
	if err := <-fw.done; err != nil {
		t.Fatal(err)
	}
	if c.store.Owner() != "zhangsan" {
		t.Errorf("owner = %q", c.store.Owner())
	}
}

func TestSetCredentialsForNewBotClearsOwner(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.store.SetCredentials("bot1", "s")
	c.store.SetOwner("me")
	c.store.SetCredentials("bot1", "s2")
	if c.store.Owner() != "me" {
		t.Error("same bot should keep owner")
	}
	c.store.SetCredentials("bot2", "s")
	if c.store.Owner() != "" {
		t.Error("new bot must be paired again")
	}
}

// ---------------------------------------------------------------------------
// 长连接：本地假服务器
// ---------------------------------------------------------------------------

// fakeWS 模拟企业微信长连接服务端。script 在认证成功后执行。
type fakeWS struct {
	t       *testing.T
	authErr int // 认证响应的 errcode
	script  func(s *wstest.ServerConn) error
	auths   []map[string]string
	mu      sync.Mutex
	done    chan error
}

func (f *fakeWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s := wstest.Accept(f.t, w, r)
	defer s.Close()
	err := func() error {
		op, data, err := s.Read()
		if err != nil || op != ws.OpText {
			return fmt.Errorf("read auth: op=%d err=%v", op, err)
		}
		var auth struct {
			Cmd     string            `json:"cmd"`
			Headers map[string]string `json:"headers"`
			Body    map[string]string `json:"body"`
		}
		if err := json.Unmarshal(data, &auth); err != nil || auth.Cmd != cmdSubscribe || !strings.HasPrefix(auth.Headers["req_id"], cmdSubscribe+"_") {
			return fmt.Errorf("bad auth frame: %s", data)
		}
		f.mu.Lock()
		f.auths = append(f.auths, auth.Body)
		f.mu.Unlock()
		s.Write(true, ws.OpText, []byte(fmt.Sprintf(`{"headers":{"req_id":%q},"errcode":%d,"errmsg":"x"}`, auth.Headers["req_id"], f.authErr)))
		if f.authErr != 0 || f.script == nil {
			return nil
		}
		return f.script(s)
	}()
	if f.done != nil {
		f.done <- err
	}
}

func readJSON(s *wstest.ServerConn) (map[string]any, error) {
	_, data, err := s.Read()
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(data, &m)
}

func TestSessionEndToEnd(t *testing.T) {
	fw := &fakeWS{t: t, done: make(chan error, 1)}
	fw.script = func(s *wstest.ServerConn) error {
		msg, _ := json.Marshal(map[string]any{
			"cmd": cmdCallback, "headers": map[string]string{"req_id": "cb-1"},
			"body": map[string]any{"msgid": "m1", "chattype": "single", "msgtype": "text", "from": map[string]string{"userid": "zhangsan"}, "text": map[string]string{"content": "来自企业微信"}},
		})
		s.Write(true, ws.OpText, msg)
		// 回执：沿用回调的 req_id
		reply, err := readJSON(s)
		if err != nil {
			return err
		}
		if reply["cmd"] != cmdRespond || reply["headers"].(map[string]any)["req_id"] != "cb-1" {
			return fmt.Errorf("bad reply: %v", reply)
		}
		stream := reply["body"].(map[string]any)["stream"].(map[string]any)
		if stream["content"] != "收到" || stream["finish"] != true {
			return fmt.Errorf("bad reply body: %v", reply)
		}
		// 被别处的新连接挤下线
		s.Write(true, ws.OpText, []byte(`{"cmd":"aibot_event_callback","headers":{"req_id":"ev"},"body":{"msgtype":"event","event":{"eventtype":"disconnected_event"}}}`))
		return nil
	}
	srv := httptest.NewServer(fw)
	defer srv.Close()

	c, _ := newTestClient(t, func(cfg *config.Config) { cfg.Ack.ReplyText = "收到" })
	c.wsURL = wstest.URL(srv)
	c.botID, c.secret = "bot_x", "sec_x"
	c.store.SetOwner("zhangsan")
	h := &recordingHandler{}
	c.handle = h.handle

	healthy := false
	err := c.session(context.Background(), func() { healthy = true })
	if !errors.Is(err, errReplaced) {
		t.Fatalf("session ended with %v", err)
	}
	if err := <-fw.done; err != nil {
		t.Fatal(err)
	}
	if !healthy {
		t.Error("onHealthy not called")
	}
	if len(fw.auths) != 1 || fw.auths[0]["bot_id"] != "bot_x" || fw.auths[0]["secret"] != "sec_x" {
		t.Errorf("auth body = %v", fw.auths)
	}
	if len(h.msgs) != 1 || h.msgs[0].Text != "来自企业微信" || h.msgs[0].From != "zhangsan" {
		t.Errorf("typed = %+v", h.msgs)
	}
}

func TestSessionAuthFailure(t *testing.T) {
	fw := &fakeWS{t: t, authErr: 40001}
	srv := httptest.NewServer(fw)
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	c.wsURL = wstest.URL(srv)
	c.botID, c.secret = "b", "s"
	c.handle = (&recordingHandler{}).handle
	if err := c.session(context.Background(), func() {}); !errors.Is(err, errAuthFailed) || !strings.Contains(err.Error(), "40001") {
		t.Fatalf("err = %v", err)
	}
}

func TestHeartbeatAndDeadConnection(t *testing.T) {
	pings := make(chan string, 4)
	fw := &fakeWS{t: t, done: make(chan error, 1)}
	fw.script = func(s *wstest.ServerConn) error {
		// 回应第一个心跳，之后不再回应，客户端应在连续两次没有回应后断开
		m, err := readJSON(s)
		if err != nil {
			return err
		}
		id := m["headers"].(map[string]any)["req_id"].(string)
		pings <- m["cmd"].(string) + " " + id
		s.Write(true, ws.OpText, []byte(fmt.Sprintf(`{"headers":{"req_id":%q},"errcode":0,"errmsg":"ok"}`, id)))
		for {
			if _, err := readJSON(s); err != nil {
				return nil
			}
		}
	}
	srv := httptest.NewServer(fw)
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	c.wsURL = wstest.URL(srv)
	c.botID, c.secret = "b", "s"
	c.heartbeat = 20 * time.Millisecond
	c.handle = (&recordingHandler{}).handle

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.session(ctx, func() {})
	if err == nil || !strings.Contains(err.Error(), "心跳") {
		t.Fatalf("err = %v", err)
	}
	if p := <-pings; !strings.HasPrefix(p, "ping ping_") {
		t.Errorf("ping frame = %q", p)
	}
}

func TestRunStopsWhenReplaced(t *testing.T) {
	fw := &fakeWS{t: t}
	fw.script = func(s *wstest.ServerConn) error {
		s.Write(true, ws.OpText, []byte(`{"cmd":"aibot_event_callback","headers":{"req_id":"ev"},"body":{"event":{"eventtype":"disconnected_event"}}}`))
		readJSON(s)
		return nil
	}
	srv := httptest.NewServer(fw)
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	c.wsURL = wstest.URL(srv)
	c.store.SetCredentials("b", "s")
	err := c.Run(context.Background(), (&recordingHandler{}).handle)
	if err == nil || !strings.Contains(err.Error(), "别处") {
		t.Fatalf("err = %v", err)
	}
}

// 认证失败但服务端没有明确说凭证无效（未知错误码）时，只退避重连，不清除凭证、不扫码。
func TestRunKeepsCredentialsOnOtherAuthFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	attempts := 0
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := wstest.Accept(t, w, r)
		defer s.Close()
		m, err := readJSON(s)
		if err != nil {
			return
		}
		s.Write(true, ws.OpText, []byte(fmt.Sprintf(`{"headers":{"req_id":%q},"errcode":40001}`, m["headers"].(map[string]any)["req_id"])))
		mu.Lock()
		attempts++
		if attempts >= 5 {
			cancel()
		}
		mu.Unlock()
	}))
	defer wsSrv.Close()
	qc := httptest.NewServer(&fakeQC{})
	defer qc.Close()
	c, _ := newTestClient(t, nil)
	c.wsURL = wstest.URL(wsSrv)
	c.qcBase = qc.URL
	c.store.SetCredentials("bot_old", "old")
	if err := c.Run(ctx, (&recordingHandler{}).handle); err != nil {
		t.Fatal(err)
	}
	if id, _ := c.store.Credentials(); id != "bot_old" {
		t.Errorf("凭证被改动: %q", id)
	}
}

// rescanServers 搭一个扫码服务和一个长连接服务：bot_old 认证返回 oldCode，扫码得到的 bot_x 认证成功后结束测试。
func rescanServers(t *testing.T, oldCode int, cancel context.CancelFunc) (qc, wsSrv *httptest.Server, auths func() []string) {
	t.Helper()
	qc = httptest.NewServer(&fakeQC{})
	var mu sync.Mutex
	var got []string
	wsSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := wstest.Accept(t, w, r)
		defer s.Close()
		m, err := readJSON(s)
		if err != nil {
			return
		}
		id := m["body"].(map[string]any)["bot_id"].(string)
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
		code := 0
		if id == "bot_old" {
			code = oldCode
		} else {
			cancel()
		}
		s.Write(true, ws.OpText, []byte(fmt.Sprintf(`{"headers":{"req_id":%q},"errcode":%d}`, m["headers"].(map[string]any)["req_id"], code)))
	}))
	return qc, wsSrv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
}

// 服务端明确返回凭证无效（853000）时不再重试，直接清除凭证并重新扫码。
func TestRunRescansImmediatelyOnInvalidCredentials(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	qc, wsSrv, auths := rescanServers(t, errcodeInvalidCredentials, cancel)
	defer qc.Close()
	defer wsSrv.Close()
	c, _ := newTestClient(t, nil)
	c.wsURL = wstest.URL(wsSrv)
	c.qcBase = qc.URL
	c.store.SetCredentials("bot_old", "old")
	if err := c.Run(ctx, (&recordingHandler{}).handle); err != nil {
		t.Fatal(err)
	}
	if got := auths(); len(got) != 2 || got[0] != "bot_old" || got[1] != "bot_x" {
		t.Errorf("auths = %v, want one attempt with bot_old then bot_x", got)
	}
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		saved    bool
		authErr  int
		wantAuth bool
		wantKept bool
	}{
		{"没有凭证", false, 0, true, false},
		{"凭证有效", true, 0, false, true},
		{"凭证无效", true, errcodeInvalidCredentials, true, false},
		{"其他认证错误视为无法判断", true, 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fw := &fakeWS{t: t, authErr: tc.authErr}
			srv := httptest.NewServer(fw)
			defer srv.Close()
			c, _ := newTestClient(t, nil)
			c.wsURL = wstest.URL(srv)
			if tc.saved {
				c.store.SetCredentials("bot", "secret")
			}
			err := c.Check(context.Background())
			if errors.Is(err, source.ErrAuthRequired) != tc.wantAuth {
				t.Errorf("err = %v", err)
			}
			if id, _ := c.store.Credentials(); (id != "") != tc.wantKept {
				t.Errorf("凭证保留 = %v", id != "")
			}
		})
	}
}

func TestCheck_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := wstest.URL(srv)
	srv.Close()
	c, _ := newTestClient(t, nil)
	c.wsURL = url
	c.store.SetCredentials("bot", "secret")
	if err := c.Check(context.Background()); err != nil {
		t.Errorf("err = %v", err)
	}
	if id, _ := c.store.Credentials(); id == "" {
		t.Error("网络不通时不能清凭证")
	}
}

// 只有一半凭证（缺 secret 或 bot_id）视为未授权，清除后重新扫码。
func TestCheck_Incomplete(t *testing.T) {
	for _, tc := range []struct{ name, id, secret string }{{"缺 secret", "bot", ""}, {"缺 bot_id", "", "secret"}} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, nil)
			c.store.SetCredentials(tc.id, tc.secret)
			if err := c.Check(context.Background()); !errors.Is(err, source.ErrAuthRequired) {
				t.Fatalf("err = %v", err)
			}
			if id, secret := c.store.Credentials(); id != "" || secret != "" {
				t.Error("不完整的凭证应被清除")
			}
		})
	}
}

// 重新扫码：扫成功后才覆盖旧凭证，并且要求重新配对（即使扫到的是同一个机器人）。
func TestReauthorizeReplacesAndRepairs(t *testing.T) {
	srv := httptest.NewServer(&fakeQC{})
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	c.qcBase = srv.URL
	c.store.SetCredentials("bot_x", "old") // 与扫码结果是同一个 bot_id
	c.store.SetOwner("me")
	if err := c.Reauthorize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if id, secret := c.store.Credentials(); id != "bot_x" || secret != "sec_x" {
		t.Errorf("credentials = %s %s", id, secret)
	}
	if c.store.Owner() != "" || c.pairCode == "" {
		t.Errorf("应重新配对: owner=%q pairCode=%q", c.store.Owner(), c.pairCode)
	}
}

// 扫码没完成时，旧凭证和配对记录原样保留。
func TestReauthorizeKeepsOldOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 502) }))
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	c.qcBase = srv.URL
	c.store.SetCredentials("bot_old", "old")
	c.store.SetOwner("me")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Reauthorize(ctx); err == nil {
		t.Fatal("期望超时错误")
	}
	if id, secret := c.store.Credentials(); id != "bot_old" || secret != "old" || c.store.Owner() != "me" {
		t.Errorf("旧凭证被改动: %s %s %s", id, secret, c.store.Owner())
	}
}

func TestHandleMessage_StaleHistoryNotTyped(t *testing.T) {
	c, logs := newTestClient(t, nil)
	c.store.SetOwner("zhangsan")
	h := &recordingHandler{}
	c.handle = h.handle
	mk := func(id string, created int64) *frame {
		f := callback("r-"+id, id, "single", "text", "hi", "zhangsan")
		var body map[string]any
		_ = json.Unmarshal(f.Body, &body)
		body["create_time"] = created
		f.Body, _ = json.Marshal(body)
		return f
	}
	c.handleMessage(context.Background(), nil, mk("old", c.startedAt.Add(-time.Hour).Unix()))
	if h.count() != 0 || !c.acked.Has("old") || !strings.Contains(logs.String(), "积压的历史消息") {
		t.Errorf("count=%d acked=%v logs=%s", h.count(), c.acked.Has("old"), logs.String())
	}
	c.handleMessage(context.Background(), nil, mk("new", c.startedAt.Add(time.Minute).Unix()))
	if h.count() != 1 {
		t.Errorf("新消息应键入: count=%d", h.count())
	}
	// 服务端不带时间时不做过期判断
	c.handleMessage(context.Background(), nil, callback("r3", "m3", "single", "text", "hi", "zhangsan"))
	if h.count() != 2 {
		t.Errorf("无时间的消息应键入: count=%d", h.count())
	}
}
