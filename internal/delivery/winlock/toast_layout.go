package winlock

import (
	"math"
	"time"
)

// 提示卡片的动画参数。卡片从屏幕右下角滑入；新卡片出现时把旧卡片往上顶，
// 越旧的卡片越透明；每张卡片停留一段时间后淡出消失。
const (
	toastFadeIn     = 220 * time.Millisecond
	toastStay       = 3500 * time.Millisecond
	toastFadeOut    = 600 * time.Millisecond
	toastMaxVisible = 4                     // 超过这个数量，最旧的卡片提前淡出
	toastMoveTau    = 70 * time.Millisecond // 位置追随目标的时间常数，越小越快
	toastSlideDist  = 48.0                  // 滑入距离（96 DPI 下的像素）
)

// toastStackOpacity 是按新旧顺序（0 为最新）的透明度系数。
var toastStackOpacity = [toastMaxVisible]float64{1, 0.82, 0.64, 0.46}

func stackOpacity(i int) float64 {
	if i < 0 || i >= len(toastStackOpacity) {
		return 0
	}
	return toastStackOpacity[i]
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

func easeOutCubic(t float64) float64 {
	t = clamp01(t)
	return 1 - math.Pow(1-t, 3)
}

// toastOpacity 返回卡片在 now 时刻自身的不透明度（不含堆叠系数）：
// 出现时淡入，到 fadeOutAt 后在 fade 时间内淡出。done 表示已经完全消失，可以销毁。
func toastOpacity(now, born, fadeOutAt time.Time, fade time.Duration) (opacity float64, done bool) {
	in := easeOutCubic(float64(now.Sub(born)) / float64(toastFadeIn))
	if now.Before(fadeOutAt) {
		return in, false
	}
	if fade <= 0 {
		return 0, true
	}
	out := 1 - float64(now.Sub(fadeOutAt))/float64(fade)
	if out <= 0 {
		return 0, true
	}
	return in * out * out, false // 二次曲线，结尾消失得更柔和
}

// slideOffset 返回卡片在 now 时刻向右偏移的比例：出现时从 1 滑到 0。
func slideOffset(now, born time.Time) float64 {
	return 1 - easeOutCubic(float64(now.Sub(born))/float64(toastFadeIn))
}

// stackTargets 计算每张卡片的目标顶端坐标。heights 按新旧顺序（0 为最新）排列，
// bottom 是最新卡片底边的位置，卡片之间留 gap。
func stackTargets(bottom float64, heights []float64, gap float64) []float64 {
	out := make([]float64, len(heights))
	y := bottom
	for i, h := range heights {
		y -= h
		out[i] = y
		y -= gap
	}
	return out
}

// approach 让 cur 以指数方式追随 target，dt 是距离上一帧的时间。
func approach(cur, target float64, dt time.Duration) float64 {
	if dt <= 0 {
		return cur
	}
	k := 1 - math.Exp(-float64(dt)/float64(toastMoveTau))
	next := cur + (target-cur)*k
	if math.Abs(target-next) < 0.5 {
		return target
	}
	return next
}

// roundedRectSDF 返回点 (px, py) 到圆角矩形边界的有符号距离：内部为负，外部为正。
// 矩形左上角 (x, y)，宽 w、高 h，圆角半径 r。
func roundedRectSDF(px, py, x, y, w, h, r float64) float64 {
	cx, cy := x+w/2, y+h/2
	qx := math.Abs(px-cx) - (w/2 - r)
	qy := math.Abs(py-cy) - (h/2 - r)
	outside := math.Hypot(math.Max(qx, 0), math.Max(qy, 0))
	inside := math.Min(math.Max(qx, qy), 0)
	return outside + inside - r
}
