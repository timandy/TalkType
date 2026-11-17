package feishu

import (
	"bytes"
	"reflect"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	in := &frame{
		SeqID:           7,
		LogID:           1 << 40,
		Service:         12345,
		Method:          methodData,
		Headers:         []header{{headerType, typeEvent}, {headerMessageID, "m1"}, {"empty", ""}},
		PayloadEncoding: "json",
		PayloadType:     "x",
		Payload:         []byte(`{"a":1}`),
		LogIDNew:        "log-new",
	}
	out, err := decodeFrame(in.encode())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
	if out.header(headerMessageID) != "m1" || out.header("missing") != "" {
		t.Error("header lookup wrong")
	}
}

func TestFrameNegativeInt32(t *testing.T) {
	in := &frame{Service: -1, Method: methodControl}
	out, err := decodeFrame(in.encode())
	if err != nil {
		t.Fatal(err)
	}
	if out.Service != -1 {
		t.Errorf("Service = %d, want -1", out.Service)
	}
}

// ping 帧按 protobuf 规则手算的编码：SeqID=0 LogID=0 service=5 method=0 header type=ping。
// 前四个字段即使为 0 也写出，与官方 SDK 发 ping 时的字段一致。
func TestFrameEncodeMatchesSDK(t *testing.T) {
	f := &frame{Service: 5, Method: methodControl, Headers: []header{{headerType, typePing}}}
	want := []byte{0x08, 0x00, 0x10, 0x00, 0x18, 0x05, 0x20, 0x00,
		0x2a, 0x0c, 0x0a, 0x04, 't', 'y', 'p', 'e', 0x12, 0x04, 'p', 'i', 'n', 'g'}
	if got := f.encode(); !bytes.Equal(got, want) {
		t.Errorf("encode = % x\nwant     % x", got, want)
	}
}

func TestDecodeFrameSkipsUnknownFields(t *testing.T) {
	var b []byte
	b = appendVarintField(b, 4, methodData)
	b = appendVarintField(b, 20, 99)               // 未知 varint 字段
	b = appendBytesField(b, 21, []byte("ignored")) // 未知 bytes 字段
	b = appendTag(b, 22, wire32)
	b = append(b, 1, 2, 3, 4)
	b = appendTag(b, 23, wire64)
	b = append(b, 1, 2, 3, 4, 5, 6, 7, 8)
	b = appendBytesField(b, 8, []byte("p"))
	f, err := decodeFrame(b)
	if err != nil {
		t.Fatal(err)
	}
	if f.Method != methodData || string(f.Payload) != "p" {
		t.Errorf("got %+v", f)
	}
}

func TestDecodeFrameTruncated(t *testing.T) {
	full := (&frame{Headers: []header{{"k", "v"}}, Payload: []byte("hello")}).encode()
	for i := 1; i < len(full); i++ {
		_, _ = decodeFrame(full[:i]) // 任意截断都不能 panic；截在字段边界上时可能解出部分帧
	}
	if _, err := decodeFrame([]byte{0x42, 0x10, 'a'}); err == nil {
		t.Error("bytes field longer than data should fail")
	}
}
