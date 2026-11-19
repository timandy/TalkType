package source

import (
	"testing"
	"time"
)

func TestIsStale(t *testing.T) {
	start := time.UnixMilli(1_700_000_000_000)
	for _, tc := range []struct {
		name    string
		created int64
		want    bool
	}{
		{"未知时间不算过期", 0, false},
		{"毫秒，启动前", 1_699_999_999_999, true},
		{"毫秒，启动时", 1_700_000_000_000, false},
		{"毫秒，启动后", 1_700_000_000_001, false},
		{"秒，启动前", 1_699_999_999, true},
		{"秒，启动后", 1_700_000_001, false},
	} {
		if got := IsStale(tc.created, start); got != tc.want {
			t.Errorf("%s: IsStale(%d) = %v，期望 %v", tc.name, tc.created, got, tc.want)
		}
	}
}
