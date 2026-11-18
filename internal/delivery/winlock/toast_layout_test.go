package winlock

import (
	"math"
	"testing"
	"time"
)

func TestToastOpacityLifecycle(t *testing.T) {
	born := time.Unix(1000, 0)
	fadeOutAt := born.Add(toastFadeIn + toastStay)

	if op, done := toastOpacity(born, born, fadeOutAt, toastFadeOut); op != 0 || done {
		t.Errorf("at birth: op=%v done=%v", op, done)
	}
	mid, _ := toastOpacity(born.Add(toastFadeIn/2), born, fadeOutAt, toastFadeOut)
	if mid <= 0 || mid >= 1 {
		t.Errorf("fading in: op=%v", mid)
	}
	if op, done := toastOpacity(born.Add(toastFadeIn+time.Second), born, fadeOutAt, toastFadeOut); op != 1 || done {
		t.Errorf("staying: op=%v done=%v", op, done)
	}
	out, done := toastOpacity(fadeOutAt.Add(toastFadeOut/2), born, fadeOutAt, toastFadeOut)
	if out <= 0 || out >= 1 || done {
		t.Errorf("fading out: op=%v done=%v", out, done)
	}
	if op, done := toastOpacity(fadeOutAt.Add(toastFadeOut), born, fadeOutAt, toastFadeOut); op != 0 || !done {
		t.Errorf("after fade out: op=%v done=%v", op, done)
	}
}

func TestToastOpacityForcedEarlyFade(t *testing.T) {
	born := time.Unix(1000, 0)
	now := born.Add(time.Second)
	// 被顶出可见范围时，fadeOutAt 提前到 now
	if op, done := toastOpacity(now.Add(toastFadeOut), born, now, toastFadeOut); op != 0 || !done {
		t.Errorf("op=%v done=%v", op, done)
	}
}

func TestStackOpacityDecreases(t *testing.T) {
	prev := 2.0
	for i := 0; i < toastMaxVisible; i++ {
		op := stackOpacity(i)
		if op <= 0 || op >= prev {
			t.Fatalf("stackOpacity(%d) = %v, should be positive and decreasing", i, op)
		}
		prev = op
	}
	if stackOpacity(toastMaxVisible) != 0 || stackOpacity(-1) != 0 {
		t.Error("out of range should be 0")
	}
}

func TestSlideOffset(t *testing.T) {
	born := time.Unix(1000, 0)
	if slideOffset(born, born) != 1 || slideOffset(born.Add(toastFadeIn), born) != 0 {
		t.Error("slide should go from 1 to 0 during fade in")
	}
}

func TestStackTargets(t *testing.T) {
	got := stackTargets(1000, []float64{80, 100, 60}, 10)
	want := []float64{920, 810, 740} // 最新的在最下面，旧的依次往上
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestApproach(t *testing.T) {
	if approach(0, 100, 0) != 0 {
		t.Error("dt=0 should not move")
	}
	v := approach(0, 100, toastMoveTau)
	if math.Abs(v-63.2) > 0.5 {
		t.Errorf("after one time constant: %v, want ~63.2", v)
	}
	if approach(99.8, 100, time.Millisecond) != 100 {
		t.Error("should snap when close")
	}
}

func TestRoundedRectSDF(t *testing.T) {
	// 100x50 的矩形，圆角 10
	if d := roundedRectSDF(50, 25, 0, 0, 100, 50, 10); d >= 0 {
		t.Errorf("center should be inside: %v", d)
	}
	if d := roundedRectSDF(50, 0, 0, 0, 100, 50, 10); math.Abs(d) > 1e-9 {
		t.Errorf("top edge midpoint should be on boundary: %v", d)
	}
	if d := roundedRectSDF(0.5, 0.5, 0, 0, 100, 50, 10); d <= 0 {
		t.Errorf("corner pixel should be outside the rounded corner: %v", d)
	}
	if d := roundedRectSDF(-5, 25, 0, 0, 100, 50, 10); math.Abs(d-5) > 1e-9 {
		t.Errorf("5px left of the edge: %v", d)
	}
}

func TestToastOpacityZeroFade(t *testing.T) {
	born := time.Unix(1000, 0)
	if _, done := toastOpacity(born, born, born, 0); !done {
		t.Error("zero fade duration should finish immediately")
	}
}
