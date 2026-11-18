//go:build windows

package winlock

import "testing"

func TestRenderToast(t *testing.T) {
	img, ok := renderToast(LevelSuccess, "已锁定当前终端", "消息会在后台发送到「GoLand · PS E:\\work>」，不会切换窗口。", 96)
	if !ok {
		t.Fatal("render failed")
	}
	defer img.release()

	if img.cardW != 360 || img.w != img.cardW+2*img.margin || img.h != img.cardH+2*img.margin {
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
	if big.cardW != 720 {
		t.Errorf("200%% scaling: cardW = %d, want 720", big.cardW)
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

// 文字应当是灰度抗锯齿的：标题区域里除了底色和纯白，还应有大量中间亮度的边缘像素。
func TestRenderToastTextIsAntialiased(t *testing.T) {
	img, ok := renderToast(LevelInfo, "已解除锁定", "", 96)
	if !ok {
		t.Fatal("render failed")
	}
	defer img.release()
	// 标题所在的区域：图标右侧、卡片内边距以内
	x0 := img.margin + img.cardW/4
	x1 := img.margin + img.cardW*3/4
	y0, y1 := img.margin+8, img.margin+img.cardH-8
	edge, solid := 0, 0
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			p := img.pixels[int(y)*int(img.w)+int(x)]
			g := p >> 8 & 0xFF // 蓝紫底色的绿色分量较低，文字为白色
			switch {
			case g >= 250:
				solid++
			case g > 150:
				edge++
			}
		}
	}
	if solid == 0 {
		t.Fatal("no text pixels found")
	}
	if edge < solid/3 {
		t.Errorf("text looks aliased: %d edge pixels vs %d solid pixels", edge, solid)
	}
}

func TestTextGammaAndBlend(t *testing.T) {
	if textGamma(0) != 0 || textGamma(1) != 1 || textGamma(0.5) <= 0.5 {
		t.Error("gamma should keep 0 and 1 and lift mid values")
	}
	r, g, b := blendOver(0, 0, 0, 0xFFFFFF, 0.5)
	if r != 127.5 || g != 127.5 || b != 127.5 {
		t.Errorf("blendOver = %v %v %v", r, g, b)
	}
	if r, _, _ := blendOver(10, 20, 30, 0xFFFFFF, 0); r != 10 {
		t.Error("zero coverage should not change color")
	}
}
