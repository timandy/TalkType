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
	antialiasQuality = 4
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
		1 /* DEFAULT_CHARSET */, 0, 0, antialiasQuality, 0, uintptr(unsafe.Pointer(utf16Ptr(face))))
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

	cardW, pad, icon, iconGap, radius, margin := px(360), px(15), px(36), px(12), float64(px(12)), px(14)
	lineGap := px(4)
	textW := cardW - pad*2 - icon - iconGap
	ceilDiv := func(v int32) int32 { return (v + textSS - 1) / textSS }

	dc, _, _ := procCreateCompatibleDC.Call(0)
	if dc == 0 {
		return nil, false
	}
	img := &toastImage{dc: dc, margin: margin, cardW: cardW}

	// 文字按 textSS 倍尺寸排版和绘制，再缩小回来（见第 3 步）。
	titleFont := createFont(px(16)*textSS, fwBold, "Microsoft YaHei UI")
	msgFont := createFont(px(14)*textSS, fwNormal, "Microsoft YaHei UI")
	glyphFont := createFont(px(20)*textSS, fwBold, "Segoe UI Symbol")
	defer func() {
		for _, f := range []uintptr{titleFont, msgFont, glyphFont} {
			procDeleteObject.Call(f)
		}
	}()

	old, _, _ := procSelectObject.Call(dc, titleFont)
	titleHS := measureText(dc, title, textW*textSS, dtSingleLine|dtNoPrefix)
	procSelectObject.Call(dc, msgFont)
	msgFmt := uint32(dtWordBreak | dtEditControl | dtEndEllipsis | dtNoPrefix)
	msgHS := int32(0)
	if msg != "" {
		lineHS := measureText(dc, "国", textW*textSS, dtSingleLine|dtNoPrefix)
		msgHS = measureText(dc, msg, textW*textSS, msgFmt)
		if limit := lineHS * toastMaxLines; msgHS > limit {
			msgHS = limit
		}
	}
	procSelectObject.Call(dc, old)
	titleH, msgH := ceilDiv(titleHS), ceilDiv(msgHS)

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
	// 3. 文字：按 textSS 倍尺寸画到灰度蒙版上，再按块平均缩小，得到平滑的抗锯齿文字，
	//    不依赖 ClearType 和系统字体平滑设置。标题和图标画在红色通道，正文画在绿色通道，
	//    以便分别着色。
	tx, ty := margin+pad+icon+iconGap, margin+pad
	if content < icon { // 内容比图标矮时垂直居中
		ty += (icon - content) / 2
	}
	rx0, ry0 := margin+pad, margin+pad // 蒙版覆盖的区域：卡片内边距以内
	rw, rh := cardW-pad*2, cardH-pad*2
	mask, ok := newTextMask(rw*textSS, rh*textSS)
	if !ok {
		img.release()
		return nil, false
	}
	defer mask.release()
	mdc := mask.dc
	procSetBkMode.Call(mdc, transparentBk)
	rel := func(x, y, w, h int32) *rect { // 卡片坐标 → 蒙版坐标
		return &rect{(x - rx0) * textSS, (y - ry0) * textSS, (x - rx0 + w) * textSS, (y - ry0 + h) * textSS}
	}
	procSetTextColor.Call(mdc, 0x0000FF) // COLORREF 0x00BBGGRR：纯红
	mold, _, _ := procSelectObject.Call(mdc, glyphFont)
	drawText(mdc, style.glyph, rel(rx0, ry0, icon, icon), dtCenter|dtVCenter|dtSingleLine|dtNoPrefix)
	procSelectObject.Call(mdc, titleFont)
	drawText(mdc, title, rel(tx, ty, textW, titleH), dtSingleLine|dtEndEllipsis|dtNoPrefix)
	if msgHS > 0 {
		procSetTextColor.Call(mdc, 0x00FF00) // 纯绿
		procSelectObject.Call(mdc, msgFont)
		r := rel(tx, ty+titleH+lineGap, textW, 0)
		r.bottom = r.top + msgHS
		drawText(mdc, msg, r, msgFmt)
	}
	procSelectObject.Call(mdc, mold)
	procGdiFlush.Call()

	const titleColor, msgColor = 0xFFFFFF, 0xF1F5F9 // 白色；正文为略暗的白
	for y := int32(0); y < rh; y++ {
		for x := int32(0); x < rw; x++ {
			ct, cm := mask.coverage(x, y)
			if ct == 0 && cm == 0 {
				continue
			}
			i := int(ry0+y)*int(img.w) + int(rx0+x)
			p := pixels[i]
			r, g, b := float64(p>>16&0xFF), float64(p>>8&0xFF), float64(p&0xFF)
			r, g, b = blendOver(r, g, b, titleColor, ct)
			r, g, b = blendOver(r, g, b, msgColor, cm)
			pixels[i] = uint32(r+0.5)<<16 | uint32(g+0.5)<<8 | uint32(b+0.5)
		}
	}
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
	fadeDur   time.Duration // 淡出用时
	x, y      float64       // 当前窗口位置
	shown     bool

	// 上一次提交给系统的位置和透明度，没变化时跳过 UpdateLayeredWindow。
	lastX, lastY int32
	lastAlpha    byte
}

// toaster 管理所有卡片，只在 UI 线程上使用。
type toaster struct {
	host     uintptr // 驱动动画定时器的窗口
	toasts   []*toast
	interval time.Duration // 当前定时器间隔，0 表示定时器未开启
	last     time.Time

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

// warmUp 在启动时按当前显示器 DPI 渲染一张卡片并丢弃。第一次渲染要加载字体等资源，
// 实测比之后慢约 30 毫秒，提前做掉，第一次按快捷键时就不用等。
func (t *toaster) warmUp() {
	t.anchor()
	if img, ok := renderToast(LevelInfo, "预热", "预热", t.dpi); ok {
		img.release()
	}
}

// setInterval 调整动画定时器的间隔；d 为 0 时关闭。
func (t *toaster) setInterval(d time.Duration) {
	if d == t.interval {
		return
	}
	if d == 0 {
		procKillTimer.Call(t.host, toastTimerID)
	} else {
		procSetTimer.Call(t.host, toastTimerID, uintptr(d.Milliseconds()), 0)
	}
	t.interval = d
}

// show 弹出一张新卡片，放在最下面，把已有卡片往上顶。
func (t *toaster) show(n Notice) {
	if len(t.toasts) == 0 {
		t.anchor()
	}
	img, ok := renderToast(n.Level, n.Title, n.Msg, t.dpi)
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
	nt := &toast{hwnd: hwnd, img: img, born: now, fadeOutAt: now.Add(toastFadeIn + toastStay), fadeDur: toastFadeOut}
	nt.y = t.bottom - float64(img.cardH) - float64(img.margin)
	t.toasts = append([]*toast{nt}, t.toasts...)
	t.tick()
}

// tick 推进一帧动画：更新每张卡片的位置和透明度，销毁已经消失的卡片。
// 所有卡片都静止时，把定时器放慢到下一张卡片开始淡出的时刻，避免空转。
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

	moving := false
	nextFade := time.Duration(math.MaxInt64)
	alive := t.toasts[:0]
	for i, ts := range t.toasts {
		if i >= toastMaxVisible && ts.fadeOutAt.After(now) {
			ts.fadeOutAt = now // 被顶出可见范围：提前淡出
		}
		op, done := toastOpacity(now, ts.born, ts.fadeOutAt, ts.fadeDur)
		if done {
			ts.destroy()
			continue
		}
		op *= math.Max(stackOpacity(i), stackOpacity(toastMaxVisible-1))

		m := float64(ts.img.margin)
		ty := targets[i] - m
		ts.y = approach(ts.y, ty, dt)
		slide := slideOffset(now, ts.born)
		ts.x = t.right - float64(ts.img.cardW) - m + slide*t.scale(toastSlideDist)
		ts.update(byte(math.Round(op * 255)))
		alive = append(alive, ts)

		switch {
		case !now.Before(ts.fadeOutAt), slide > 0, ts.y != ty, now.Sub(ts.born) < toastFadeIn:
			moving = true
		default:
			if d := ts.fadeOutAt.Sub(now); d < nextFade {
				nextFade = d
			}
		}
	}
	for i := len(alive); i < len(t.toasts); i++ {
		t.toasts[i] = nil
	}
	t.toasts = alive

	switch {
	case len(t.toasts) == 0:
		t.setInterval(0)
		t.last = time.Time{}
	case moving:
		t.setInterval(toastFrame)
	default:
		t.setInterval(max(nextFade, toastFrame))
		t.last = time.Time{} // 静止期间不需要平滑追随
	}
}

func (ts *toast) update(alpha byte) {
	x, y := int32(math.Round(ts.x)), int32(math.Round(ts.y))
	if ts.shown && x == ts.lastX && y == ts.lastY && alpha == ts.lastAlpha {
		return
	}
	ts.lastX, ts.lastY, ts.lastAlpha = x, y, alpha
	pt := struct{ x, y int32 }{x, y}
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
	t.setInterval(0)
}

// 文字超采样倍数：按 textSS 倍尺寸绘制，再缩小到原尺寸，相当于 textSS² 倍抗锯齿。
const textSS = 4

// toastMaxLines 是正文最多显示的行数，超出部分以省略号结尾。
const toastMaxLines = 4

// textMask 是绘制超采样文字用的 32 位位图，背景为黑色。
type textMask struct {
	dc, bmp, oldBmp uintptr
	w, h            int32
	pixels          []uint32
}

func newTextMask(w, h int32) (*textMask, bool) {
	if w <= 0 || h <= 0 {
		return nil, false
	}
	dc, _, _ := procCreateCompatibleDC.Call(0)
	if dc == 0 {
		return nil, false
	}
	bi := bitmapInfoHeader{biWidth: w, biHeight: -h, biPlanes: 1, biBitCount: 32}
	bi.biSize = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	bmp, _, _ := procCreateDIBSection.Call(dc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == nil {
		procDeleteDC.Call(dc)
		return nil, false
	}
	m := &textMask{dc: dc, bmp: bmp, w: w, h: h}
	m.oldBmp, _, _ = procSelectObject.Call(dc, bmp)
	m.pixels = unsafe.Slice((*uint32)(bits), int(w)*int(h)) // CreateDIBSection 的内容初始为 0，即黑色
	return m, true
}

func (m *textMask) release() {
	procSelectObject.Call(m.dc, m.oldBmp)
	procDeleteDC.Call(m.dc)
	procDeleteObject.Call(m.bmp)
}

// coverage 返回原尺寸像素 (x, y) 处标题（红色通道）和正文（绿色通道）的覆盖率，取值 0～1。
func (m *textMask) coverage(x, y int32) (title, body float64) {
	var sr, sg uint32
	for dy := int32(0); dy < textSS; dy++ {
		row := int(y*textSS+dy) * int(m.w)
		for dx := int32(0); dx < textSS; dx++ {
			p := m.pixels[row+int(x*textSS+dx)]
			sr += p >> 16 & 0xFF
			sg += p >> 8 & 0xFF
		}
	}
	n := float64(textSS*textSS) * 255
	return textGamma(float64(sr) / n), textGamma(float64(sg) / n)
}

// textGamma 略微加粗半覆盖的边缘像素：浅色文字叠在彩色底上时，线性混合会让笔画显得偏细。
func textGamma(c float64) float64 {
	if c <= 0 {
		return 0
	}
	return math.Pow(c, 0.8)
}

// blendOver 把颜色 c（0xRRGGBB）按覆盖率 a 叠加到 (r, g, b) 上。
func blendOver(r, g, b float64, c uint32, a float64) (float64, float64, float64) {
	if a <= 0 {
		return r, g, b
	}
	cr, cg, cb := float64(c>>16&0xFF), float64(c>>8&0xFF), float64(c&0xFF)
	return r + (cr-r)*a, g + (cg-g)*a, b + (cb-b)*a
}
