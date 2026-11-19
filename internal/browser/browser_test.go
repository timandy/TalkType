package browser

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestCommand(t *testing.T) {
	const u = "https://example.com/qr?a=1&b=2"
	for _, tc := range []struct {
		goos string
		want []string
	}{
		{"darwin", []string{"open", u}},
		{"windows", []string{"rundll32", "url.dll,FileProtocolHandler", u}},
	} {
		cmd := command(tc.goos, u)
		got := append([]string{filepath.Base(cmd.Args[0])}, cmd.Args[1:]...)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: args = %q，期望 %q", tc.goos, got, tc.want)
		}
	}
}
