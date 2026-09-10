package clock

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSleepEndsWithTheContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Error("a cancelled context must end the sleep at once")
	}
	if err := Sleep(t.Context(), time.Millisecond); err != nil {
		t.Errorf("a full wait returns nil, got %v", err)
	}
	if err := NoSleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("NoSleep must report the ended context, got %v", err)
	}
	if err := NoSleep(t.Context(), time.Hour); err != nil {
		t.Errorf("NoSleep on a live context returns nil, got %v", err)
	}
}
