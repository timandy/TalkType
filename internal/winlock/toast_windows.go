//go:build windows

package winlock

// 右下角提示卡片：每条通知是一个分层窗口（WS_EX_LAYERED），用 UpdateLayeredWindow
// 按像素透明度显示，得到抗锯齿圆角、阴影和整体淡入淡出。窗口不抢焦点、不挡鼠标、
// 不出现在任务栏。所有窗口都在 host 的 UI 线程上创建和更新。

import (
	"math"
	"syscall"
	"time"
	"unsafe"
)

const (
	toastClassName = "ClawbotIlinkRelayToast"
	toastTimerID   = 2
	toastFrame     = 15 * time.Millisecond

	wsPopup          = 0x80000000
	wsExLayered      = 0x00080000
	wsExTransparent  = 0x00000020
	wsExTopmost      = 0x00000008
	wsExToolWindow   = 0x00000080
	wsExNoActivate   = 0x08000000
	swShowNoActivate = 4
	ulwAlpha         = 0x02
	acSrcAlpha       = 0x01
	monitorPrimary   = 1
	dtCenter         = 0x0001
	dtVCenter        = 0x0004
	dtWordBreak      = 0x0010
	dtSingleLine     = 0x0020
	dtNoPrefix       = 0x0800
	dtCalcRect       = 0x0400
	dtEditControl    = 0x2000
	dtEndEllipsis    = 0x8000
	fwNormal         = 400
	fwBold           = 700
	cleartypeQuality = 5
	transparentBk    = 1
	dpiPerMonitorV2  = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
)

var (
	gdi32                  = syscall.NewLazyDLL("gdi32.dll")
	shcore                 = syscall.NewLazyDLL("shcore.dll")
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procCreateFontW        = gdi32.NewProc("CreateFontW")
	procSetBkMode          = gdi32.NewProc("SetBkMode")
	procSetTextColor       = gdi32.NewProc("SetTextColor")
	procGdiFlush           = gdi32.NewProc("GdiFlush")
	procDrawTextW          = user32.NewProc("DrawTextW")
	procUpdateLayeredWin   = user32.NewProc("UpdateLayeredWindow")
	procShowWindow         = user32.NewProc("ShowWindow")
	procMonitorFromWindow  = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW    = user32.NewProc("GetMonitorInfoW")
	procGetForegroundWnd   = user32.NewProc("GetForegroundWindow")
	procSetThreadDpiCtx    = user32.NewProc("SetThreadDpiAwarenessContext")
	procGetDpiForMonitor   = shcore.NewProc("GetDpiForMonitor")
)

type rect struct{ left, top, right, bottom int32 }

type monitorInfo struct {
	cbSize  uint32
	monitor rect
	work    rect
	flags   uint32
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type blendFunction struct {
	op, flags, alpha, format byte
}

type sizeStruct struct{ cx, cy int32 }

// toastStyle 是每种通知级别的配色和图标。
type toastStyle struct {
	from, to uint32 // 渐变起止颜色 0xRRGGBB
	glyph    string
}

var toastStyles = map[Level]toastStyle{
	LevelInfo:    {0x3B82F6, 0x6366F1, "\u2139"}, // 蓝 → 靛，ℹ
	LevelSuccess: {0x10B981, 0x0D9488, "\u2713"}, // 翠绿 → 青，✓
	LevelWarning: {0xF59E0B, 0xEA580C, "!"},      // 琥珀 → 橙
	LevelError:   {0xEF4444, 0xDB2777, "\u2715"}, // 红 → 粉，✕
}

// toastImage 是渲染好的卡片位图，选入在一个内存 DC 中，供 UpdateLayeredWindow 使用。
type toastImage struct {
	dc, bmp, oldBmp uintptr
	w, h            int32 // 位图（窗口）尺寸，含阴影边距
	cardW, cardH    int32
	margin          int32
	pixels          []uint32 // 预乘 alpha 的 BGRA，供测试检查
}

func (img *toastImage) release() {
	if img.dc != 0 {
		procSelectObject.Call(img.dc, img.oldBmp)
		procDeleteDC.Call(img.dc)
	}
	if img.bmp != 0 {
		procDeleteObject.Call(img.bmp)
	}
	*img = toastImage{}
}

func utf16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		p, _ = syscall.UTF16PtrFromString("")
	}
	return p
}

func createFont(height int32, weight int32, face string) uintptr {
	f, _, _ := procCreateFontW.Call(uintptr(uint32(-height)), 0, 0, 0, uintptr(weight), 0, 0, 0,
		1 /* DEFAULT_CHARSET */, 0, 0, cleartypeQuality, 0, uintptr(unsafe.Pointer(utf16Ptr(face))))
	return f
}

func drawText(dc uintptr, s string, r *rect, format uint32) {
	procDrawTextW.Call(dc, uintptr(unsafe.Pointer(utf16Ptr(s))), ^uintptr(0), uintptr(unsafe.Pointer(r)), uintptr(format))
}

// measureText 返回文字在给定宽度下排版后的高度。
func measureText(dc uintptr, s string, width int32, format uint32) int32 {
	r := rect{0, 0, width, 0}
	drawText(dc, s, &r, format|dtCalcRect)
	return r.bottom
}

func lerpColor(a, b uint32, t float64) (r, g, bl float64) {
	ar, ag, ab := float64(a>>16&0xFF), float64(a>>8&0xFF), float64(a&0xFF)
	br, bg, bb := float64(b>>16&0xFF), float64(b>>8&0xFF), float64(b&0xFF)
	return ar + (br-ar)*t, ag + (bg-ag)*t, ab + (bb-ab)*t
}

// renderToast 按 dpi 渲染一张卡片：渐变底色、左侧半透明圆形图标、标题和正文，
// 外加抗锯齿圆角和柔和阴影。
func renderToast(level Level, title, msg string, dpi uint32) (*toastImage, bool) {
	style, ok := toastStyles[level]
	if !ok {
		style = toastStyles[LevelInfo]
	}
	s := float64(dpi) / 96
	px := func(v float64) int32 { return int32(math.Round(v * s)) }

	cardW, pad, icon, iconGap, radius, margin := px(340), px(14), px(34), px(12), float64(px(12)), px(14)
	lineGap := px(3)
	textW := cardW - pad*2 - icon - iconGap

	dc, _, _ := procCreateCompatibleDC.Call(0)
	if dc == 0 {
		return nil, false
	}
	img := &toastImage{dc: dc, margin: margin, cardW: cardW}

	titleFont := createFont(px(15), fwBold, "Microsoft YaHei UI")
	msgFont := createFont(px(13), fwNormal, "Microsoft YaHei UI")
	glyphFont := createFont(px(19), fwBold, "Segoe UI Symbol")
	defer func() {
		for _, f := range []uintptr{titleFont, msgFont, glyphFont} {
			procDeleteObject.Call(f)
		}
	}()

	old, _, _ := procSelectObject.Call(dc, titleFont)
	titleH := measureText(dc, title, textW, dtSingleLine|dtNoPrefix)
	procSelectObject.Call(dc, msgFont)
	msgFmt := uint32(dtWordBreak | dtEditControl | dtEndEllipsis | dtNoPrefix)
	msgH := int32(0)
	if msg != "" {
		lineH := measureText(dc, "国", textW, dtSingleLine|dtNoPrefix)
		msgH = measureText(dc, msg, textW, msgFmt)
		if limit := lineH * 4; msgH > limit {
			msgH = limit
		}
	}
	procSelectObject.Call(dc, old)

	content := titleH
	if msgH > 0 {
		content += lineGap + msgH
	}
	cardH := content
	if icon > cardH {
		cardH = icon
	}
	cardH += pad * 2
	img.cardH = cardH
	img.w, img.h = cardW+margin*2, cardH+margin*2

	bi := bitmapInfoHeader{biWidth: img.w, biHeight: -img.h, biPlanes: 1, biBitCount: 32}
	bi.biSize = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	bmp, _, _ := procCreateDIBSection.Call(dc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == nil {
		img.release()
		return nil, false
	}
	img.bmp = bmp
	img.oldBmp, _, _ = procSelectObject.Call(dc, bmp)
	pixels := unsafe.Slice((*uint32)(bits), int(img.w)*int(img.h))

	// 1. 底色：从左到右渐变，顶部略微提亮。
	m, cw, ch := float64(margin), float64(cardW), float64(cardH)
	for y := int32(0); y < img.h; y++ {
		v := clamp01((float64(y) - m) / ch)
		lift := 0.10 * (1 - v) // 顶部提亮 10%，向下渐弱
		for x := int32(0); x < img.w; x++ {
			r, g, b := lerpColor(style.from, style.to, clamp01((float64(x)-m)/cw))
			r, g, b = r+(255-r)*lift, g+(255-g)*lift, b+(255-b)*lift
			pixels[int(y)*int(img.w)+int(x)] = uint32(r)<<16 | uint32(g)<<8 | uint32(b)
		}
	}

	// 2. 图标底圆：白色 22% 叠在底色上。
	icx, icy, ir := m+float64(pad)+float64(icon)/2, m+float64(pad)+float64(icon)/2, float64(icon)/2
	for y := int32(icy - ir - 1); y <= int32(icy+ir+1); y++ {
		for x := int32(icx - ir - 1); x <= int32(icx+ir+1); x++ {
			if x < 0 || y < 0 || x >= img.w || y >= img.h {
				continue
			}
			d := math.Hypot(float64(x)+0.5-icx, float64(y)+0.5-icy) - ir
			a := 0.22 * clamp01(0.5-d)
			if a <= 0 {
				continue
			}
			i := int(y)*int(img.w) + int(x)
			p := pixels[i]
			r, g, b := float64(p>>16&0xFF), float64(p>>8&0xFF), float64(p&0xFF)
			pixels[i] = uint32(r+(255-r)*a)<<16 | uint32(g+(255-g)*a)<<8 | uint32(b+(255-b)*a)
		}
	}
	procGdiFlush.Call()

	// 3. 文字：图标符号、标题、正文，白色。
	procSetBkMode.Call(dc, transparentBk)
	procSetTextColor.Call(dc, 0xFFFFFF)
	ix, iy := margin+pad, margin+pad
	procSelectObject.Call(dc, glyphFont)
	drawText(dc, style.glyph, &rect{ix, iy, ix + icon, iy + icon}, dtCenter|dtVCenter|dtSingleLine|dtNoPrefix)

	tx, ty := margin+pad+icon+iconGap, margin+pad
	if content < icon { // 内容比图标矮时垂直居中
		ty += (icon - content) / 2
	}
	procSelectObject.Call(dc, titleFont)
	drawText(dc, title, &rect{tx, ty, tx + textW, ty + titleH}, dtSingleLine|dtEndEllipsis|dtNoPrefix)
	if msgH > 0 {
		procSetTextColor.Call(dc, 0xF9F5F1) // COLORREF 为 0x00BBGGRR：接近白色的浅灰
		procSelectObject.Call(dc, msgFont)
		my := ty + titleH + lineGap
		drawText(dc, msg, &rect{tx, my, tx + textW, my + msgH}, msgFmt)
	}
	procSelectObject.Call(dc, old)
	procGdiFlush.Call()

	// 4. 透明度：圆角卡片内不透明（边缘抗锯齿），外面是向下偏移的柔和阴影。
	shadowOff, shadowR := float64(px(3)), float64(margin)
	for y := int32(0); y < img.h; y++ {
		for x := int32(0); x < img.w; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			cov := clamp01(0.5 - roundedRectSDF(fx, fy, m, m, cw, ch, radius))
			sd := roundedRectSDF(fx, fy-shadowOff, m, m, cw, ch, radius)
			sh := 0.0
			if sd < shadowR {
				t := clamp01(1 - math.Max(sd, 0)/shadowR)
				sh = 0.22 * t * t
			}
			a := cov + sh*(1-cov)
			i := int(y)*int(img.w) + int(x)
			p := pixels[i]
			r, g, b := float64(p>>16&0xFF)*cov, float64(p>>8&0xFF)*cov, float64(p&0xFF)*cov
			pixels[i] = uint32(a*255+0.5)<<24 | uint32(r+0.5)<<16 | uint32(g+0.5)<<8 | uint32(b+0.5)
		}
	}
	img.pixels = pixels
	return img, true
}

// toast 是屏幕上的一张卡片。
type toast struct {
	hwnd      uintptr
	img       *toastImage
	born      time.Time
	fadeOutAt time.Time
	x, y      float64 // 当前窗口位置
	shown     bool
}

// toaster 管理所有卡片，只在 UI 线程上使用。
type toaster struct {
	host    uintptr // 驱动动画定时器的窗口
	toasts  []*toast
	timerOn bool
	last    time.Time

	// 卡片堆叠的锚点：工作区右下角与 DPI，在第一张卡片出现时确定。
	right, bottom float64
	dpi           uint32
}

func registerToastClass(hInst uintptr) {
	wc := wndClassEx{
		lpfnWndProc:   procDefWindowProcW.Addr(),
		hInstance:     hInst,
		lpszClassName: utf16Ptr(toastClassName),
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
}

// anchor 取前台窗口所在显示器的工作区（去掉任务栏）右下角和 DPI。
func (t *toaster) anchor() {
	fg, _, _ := procGetForegroundWnd.Call()
	mon, _, _ := procMonitorFromWindow.Call(fg, monitorPrimary)
	mi := monitorInfo{}
	mi.cbSize = uint32(unsafe.Sizeof(mi))
	procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))
	var dx, dy uint32 = 96, 96
	if procGetDpiForMonitor.Find() == nil {
		procGetDpiForMonitor.Call(mon, 0, uintptr(unsafe.Pointer(&dx)), uintptr(unsafe.Pointer(&dy)))
	}
	if dx == 0 {
		dx = 96
	}
	t.dpi = dx
	edge := 16 * float64(dx) / 96
	t.right, t.bottom = float64(mi.work.right)-edge, float64(mi.work.bottom)-edge
}

func (t *toaster) scale(v float64) float64 { return v * float64(t.dpi) / 96 }

// show 弹出一张新卡片，放在最下面，把已有卡片往上顶。
func (t *toaster) show(level Level, title, msg string) {
	if len(t.toasts) == 0 {
		t.anchor()
	}
	img, ok := renderToast(level, title, msg, t.dpi)
	if !ok {
		return
	}
	hwnd, _, _ := procCreateWindowExW.Call(
		wsExLayered|wsExTransparent|wsExTopmost|wsExToolWindow|wsExNoActivate,
		uintptr(unsafe.Pointer(utf16Ptr(toastClassName))), 0, wsPopup,
		0, 0, uintptr(img.w), uintptr(img.h), 0, 0, 0, 0)
	if hwnd == 0 {
		img.release()
		return
	}
	now := time.Now()
	nt := &toast{hwnd: hwnd, img: img, born: now, fadeOutAt: now.Add(toastFadeIn + toastStay)}
	nt.y = t.bottom - float64(img.cardH) - float64(img.margin)
	t.toasts = append([]*toast{nt}, t.toasts...)
	t.tick()
	if !t.timerOn {
		procSetTimer.Call(t.host, toastTimerID, uintptr(toastFrame.Milliseconds()), 0)
		t.timerOn = true
	}
}

// tick 推进一帧动画：更新每张卡片的位置和透明度，销毁已经消失的卡片。
func (t *toaster) tick() {
	now := time.Now()
	dt := now.Sub(t.last)
	if t.last.IsZero() || dt > 200*time.Millisecond {
		dt = toastFrame
	}
	t.last = now

	heights := make([]float64, len(t.toasts))
	for i, ts := range t.toasts {
		heights[i] = float64(ts.img.cardH)
	}
	targets := stackTargets(t.bottom, heights, t.scale(10))

	alive := t.toasts[:0]
	for i, ts := range t.toasts {
		if i >= toastMaxVisible && ts.fadeOutAt.After(now) {
			ts.fadeOutAt = now // 被顶出可见范围：提前淡出
		}
		op, done := toastOpacity(now, ts.born, ts.fadeOutAt)
		if done {
			ts.destroy()
			continue
		}
		op *= math.Max(stackOpacity(i), stackOpacity(toastMaxVisible-1))

		m := float64(ts.img.margin)
		ts.y = approach(ts.y, targets[i]-m, dt)
		ts.x = t.right - float64(ts.img.cardW) - m + slideOffset(now, ts.born)*t.scale(toastSlideDist)
		ts.update(byte(math.Round(op * 255)))
		alive = append(alive, ts)
	}
	for i := len(alive); i < len(t.toasts); i++ {
		t.toasts[i] = nil
	}
	t.toasts = alive

	if len(t.toasts) == 0 && t.timerOn {
		procKillTimer.Call(t.host, toastTimerID)
		t.timerOn = false
		t.last = time.Time{}
	}
}

func (ts *toast) update(alpha byte) {
	pt := struct{ x, y int32 }{int32(math.Round(ts.x)), int32(math.Round(ts.y))}
	size := sizeStruct{ts.img.w, ts.img.h}
	src := struct{ x, y int32 }{}
	blend := blendFunction{op: 0, alpha: alpha, format: acSrcAlpha}
	procUpdateLayeredWin.Call(ts.hwnd, 0, uintptr(unsafe.Pointer(&pt)), uintptr(unsafe.Pointer(&size)),
		ts.img.dc, uintptr(unsafe.Pointer(&src)), 0, uintptr(unsafe.Pointer(&blend)), ulwAlpha)
	if !ts.shown {
		procShowWindow.Call(ts.hwnd, swShowNoActivate)
		ts.shown = true
	}
}

func (ts *toast) destroy() {
	procDestroyWindow.Call(ts.hwnd)
	ts.img.release()
}

func (t *toaster) closeAll() {
	for _, ts := range t.toasts {
		ts.destroy()
	}
	t.toasts = nil
	if t.timerOn {
		procKillTimer.Call(t.host, toastTimerID)
		t.timerOn = false
	}
}
