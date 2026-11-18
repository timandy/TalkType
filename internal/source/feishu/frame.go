package feishu

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// 长连接上收发的帧是 protobuf 消息 pbbp2.Frame，字段号取自飞书官方 Node SDK：
//
//	message Header { string key = 1; string value = 2; }
//	message Frame {
//	  uint64 SeqID = 1; uint64 LogID = 2; int32 service = 3; int32 method = 4;
//	  repeated Header headers = 5; string payloadEncoding = 6; string payloadType = 7;
//	  bytes payload = 8; string LogIDNew = 9;
//	}
//
// 只用到这一种消息，手写编解码，不引入 protobuf 依赖。

// 帧类型（Frame.method）。
const (
	methodControl = 0 // 控制帧：ping / pong
	methodData    = 1 // 数据帧：事件
)

// header 里常用的 key 和 type 取值。
const (
	headerType      = "type"
	headerMessageID = "message_id"
	headerSum       = "sum"
	headerSeq       = "seq"
	headerTraceID   = "trace_id"
	headerBizRT     = "biz_rt"

	typeEvent = "event"
	typePing  = "ping"
	typePong  = "pong"
)

type header struct {
	Key, Value string
}

type frame struct {
	SeqID           uint64
	LogID           uint64
	Service         int32
	Method          int32
	Headers         []header
	PayloadEncoding string
	PayloadType     string
	Payload         []byte
	LogIDNew        string
}

// header 返回 key 对应的值，不存在时返回空串。
func (f *frame) header(key string) string {
	for _, h := range f.Headers {
		if h.Key == key {
			return h.Value
		}
	}
	return ""
}

const (
	wireVarint = 0
	wire64     = 1
	wireBytes  = 2
	wire32     = 5
)

func appendTag(b []byte, field, wire int) []byte {
	return binary.AppendUvarint(b, uint64(field<<3|wire))
}

func appendBytesField(b []byte, field int, v []byte) []byte {
	b = appendTag(b, field, wireBytes)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

func appendVarintField(b []byte, field int, v uint64) []byte {
	b = appendTag(b, field, wireVarint)
	return binary.AppendUvarint(b, v)
}

// encode 编码帧。前四个字段总是写出（与官方 SDK 一致），其余字段为空时省略。
// int32 负数按 protobuf 规则符号扩展成 64 位。
func (f *frame) encode() []byte {
	var b []byte
	b = appendVarintField(b, 1, f.SeqID)
	b = appendVarintField(b, 2, f.LogID)
	b = appendVarintField(b, 3, uint64(int64(f.Service)))
	b = appendVarintField(b, 4, uint64(int64(f.Method)))
	for _, h := range f.Headers {
		var hb []byte
		hb = appendBytesField(hb, 1, []byte(h.Key))
		hb = appendBytesField(hb, 2, []byte(h.Value))
		b = appendBytesField(b, 5, hb)
	}
	if f.PayloadEncoding != "" {
		b = appendBytesField(b, 6, []byte(f.PayloadEncoding))
	}
	if f.PayloadType != "" {
		b = appendBytesField(b, 7, []byte(f.PayloadType))
	}
	if len(f.Payload) > 0 {
		b = appendBytesField(b, 8, f.Payload)
	}
	if f.LogIDNew != "" {
		b = appendBytesField(b, 9, []byte(f.LogIDNew))
	}
	return b
}

var errTruncated = errors.New("帧数据不完整")

// pbReader 逐个读取 protobuf 字段。
type pbReader struct {
	b []byte
}

func (r *pbReader) varint() (uint64, error) {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		return 0, errTruncated
	}
	r.b = r.b[n:]
	return v, nil
}

func (r *pbReader) bytes() ([]byte, error) {
	n, err := r.varint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(r.b)) {
		return nil, errTruncated
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v, nil
}

// next 读取下一个字段的编号和类型。
func (r *pbReader) next() (field, wire int, err error) {
	t, err := r.varint()
	if err != nil {
		return 0, 0, err
	}
	return int(t >> 3), int(t & 7), nil
}

// skip 跳过不认识的字段。
func (r *pbReader) skip(wire int) error {
	switch wire {
	case wireVarint:
		_, err := r.varint()
		return err
	case wire64:
		if len(r.b) < 8 {
			return errTruncated
		}
		r.b = r.b[8:]
	case wire32:
		if len(r.b) < 4 {
			return errTruncated
		}
		r.b = r.b[4:]
	case wireBytes:
		_, err := r.bytes()
		return err
	default:
		return fmt.Errorf("不支持的字段类型 %d", wire)
	}
	return nil
}

func decodeFrame(data []byte) (*frame, error) {
	f := &frame{}
	r := &pbReader{b: data}
	for len(r.b) > 0 {
		field, wire, err := r.next()
		if err != nil {
			return nil, err
		}
		if (field <= 4 && wire != wireVarint) || (field >= 5 && field <= 9 && wire != wireBytes) {
			if err := r.skip(wire); err != nil {
				return nil, err
			}
			continue
		}
		switch field {
		case 1, 2, 3, 4:
			v, err := r.varint()
			if err != nil {
				return nil, err
			}
			switch field {
			case 1:
				f.SeqID = v
			case 2:
				f.LogID = v
			case 3:
				f.Service = int32(v)
			case 4:
				f.Method = int32(v)
			}
		case 5:
			hb, err := r.bytes()
			if err != nil {
				return nil, err
			}
			h, err := decodeHeader(hb)
			if err != nil {
				return nil, err
			}
			f.Headers = append(f.Headers, h)
		case 6, 7, 8, 9:
			v, err := r.bytes()
			if err != nil {
				return nil, err
			}
			switch field {
			case 6:
				f.PayloadEncoding = string(v)
			case 7:
				f.PayloadType = string(v)
			case 8:
				f.Payload = append([]byte(nil), v...)
			case 9:
				f.LogIDNew = string(v)
			}
		default:
			if err := r.skip(wire); err != nil {
				return nil, err
			}
		}
	}
	return f, nil
}

func decodeHeader(data []byte) (header, error) {
	var h header
	r := &pbReader{b: data}
	for len(r.b) > 0 {
		field, wire, err := r.next()
		if err != nil {
			return h, err
		}
		if (field == 1 || field == 2) && wire == wireBytes {
			v, err := r.bytes()
			if err != nil {
				return h, err
			}
			if field == 1 {
				h.Key = string(v)
			} else {
				h.Value = string(v)
			}
			continue
		}
		if err := r.skip(wire); err != nil {
			return h, err
		}
	}
	return h, nil
}
