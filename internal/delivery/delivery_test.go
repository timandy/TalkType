package delivery

import (
	"context"
	"errors"
	"testing"

	"github.com/timandy/talktype/internal/delivery/wintype"
	"github.com/timandy/talktype/internal/source"
)

type fakeInjector struct {
	texts   []string
	opts    []wintype.Options
	err     error
	notices []string
}

func (f *fakeInjector) Inject(text string, opts wintype.Options) error {
	f.texts = append(f.texts, text)
	f.opts = append(f.opts, opts)
	return f.err
}

func (f *fakeInjector) Notify(title, _ string) { f.notices = append(f.notices, title) }

func TestDeliver_Success(t *testing.T) {
	inj := &fakeInjector{}
	opts := wintype.Options{PressEnter: true, MaxRetries: 3}
	d := NewDeliverer(inj, opts)
	if err := d.Deliver(context.Background(), source.Message{From: "u", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(inj.texts) != 1 || inj.texts[0] != "hi" || inj.opts[0] != opts || len(inj.notices) != 0 {
		t.Errorf("inj = %+v", inj)
	}
}

func TestDeliver_TargetClosedIsDiscarded(t *testing.T) {
	inj := &fakeInjector{err: wintype.ErrTargetClosed}
	err := NewDeliverer(inj, wintype.Options{}).Deliver(context.Background(), source.Message{Text: "hi"})
	if !errors.Is(err, source.ErrDiscarded) || !errors.Is(err, wintype.ErrTargetClosed) {
		t.Fatalf("err = %v", err)
	}
	if len(inj.notices) != 1 || inj.notices[0] != closedNoticeTitle {
		t.Errorf("notices = %v", inj.notices)
	}
}

func TestDeliver_OtherErrorPassesThrough(t *testing.T) {
	boom := errors.New("boom")
	inj := &fakeInjector{err: boom}
	err := NewDeliverer(inj, wintype.Options{}).Deliver(context.Background(), source.Message{Text: "hi"})
	if !errors.Is(err, boom) || errors.Is(err, source.ErrDiscarded) || len(inj.notices) != 0 {
		t.Errorf("err=%v notices=%v", err, inj.notices)
	}
}

func TestDeliver_CancelledContext(t *testing.T) {
	inj := &fakeInjector{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewDeliverer(inj, wintype.Options{}).Deliver(ctx, source.Message{Text: "hi"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if len(inj.texts) != 0 {
		t.Error("should not inject after cancel")
	}
}

func TestNewDeliverer_NilInjectorUsesCursor(t *testing.T) {
	if _, ok := NewDeliverer(nil, wintype.Options{}).inj.(CursorInjector); !ok {
		t.Error("expected CursorInjector")
	}
}
