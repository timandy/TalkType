//go:build windows

package winlock

import "testing"

func TestRenderToast(t *testing.T) {
	img, ok := renderToast(LevelSuccess, "已锁定当前终端", "消息会在后台发送到「GoLand · PS E:\\work>」，不会切换窗口。", 96)
	if !ok {
		t.Fatal("render failed")
	}
	defer img.release()

	if img.cardW != 340 || img.w != img.cardW+2*img.margin || img.h != img.cardH+2*img.margin {
		t.Errorf("size: card %dx%d, window %dx%d, margin %d", img.cardW, img.cardH, img.w, img.h, img.margin)
	}
	at := func(x, y int32) uint32 { return img.pixels[int(y)*int(img.w)+int(x)] }
	alpha := func(p uint32) uint32 { return p >> 24 }

	if a := alpha(at(0, 0)); a != 0 {
		t.Errorf("window corner should be fully transparent, alpha=%d", a)
	}
	if a := alpha(at(img.margin, img.margin)); a == 255 {
		t.Error("card corner should be rounded (not fully opaque)")
	}
	if a := alpha(at(img.w/2, img.h/2)); a != 255 {
		t.Errorf("card center should be opaque, alpha=%d", a)
	}
	if a := alpha(at(img.w/2, img.h-img.margin/2)); a == 0 || a == 255 {
		t.Errorf("below the card should be a soft shadow, alpha=%d", a)
	}
	for i, p := range img.pixels {
		a := p >> 24
		if p>>16&0xFF > a || p>>8&0xFF > a || p&0xFF > a {
			t.Fatalf("pixel %d not premultiplied: %#08x", i, p)
		}
	}

	big, ok := renderToast(LevelSuccess, "已锁定当前终端", "消息会在后台发送。", 192)
	if !ok {
		t.Fatal("render at 192 dpi failed")
	}
	defer big.release()
	if big.cardW != 680 {
		t.Errorf("200%% scaling: cardW = %d, want 680", big.cardW)
	}
}

func TestRenderToastLongMessageIsCapped(t *testing.T) {
	short, _ := renderToast(LevelInfo, "标题", "一行", 96)
	defer short.release()
	long := ""
	for i := 0; i < 40; i++ {
		long += "很长的一段说明文字，"
	}
	tall, _ := renderToast(LevelInfo, "标题", long, 96)
	defer tall.release()
	if tall.cardH <= short.cardH {
		t.Errorf("long message should be taller: %d vs %d", tall.cardH, short.cardH)
	}
	if tall.cardH > short.cardH+4*30 {
		t.Errorf("long message should be capped at 4 lines, cardH=%d", tall.cardH)
	}
}
