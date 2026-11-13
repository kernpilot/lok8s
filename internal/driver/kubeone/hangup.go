package kubeone

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// watchHangup cancels the returned context on a SIGHUP, while the run copy
// of Apply exists (bash: the EXIT trap of the run subshell, which bash
// runs on a SIGHUP too). It does nothing when SIGHUP is ignored (nohup
// lo ...): signal.Notify would undo the ignore, which is why
// cli.WatchInterrupt does not catch SIGHUP. stop ends the watch and
// reports whether a SIGHUP came.
func watchHangup(parent context.Context) (ctx context.Context, stop func() bool) {
	if signal.Ignored(syscall.SIGHUP) {
		return parent, func() bool { return false }
	}
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	var got atomic.Bool
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		select {
		case <-ch:
			got.Store(true)
			cancel()
		case <-done:
		}
	}()
	return ctx, func() bool {
		signal.Stop(ch)
		close(done)
		cancel()
		return got.Load()
	}
}

// raiseHangup ends lo with the SIGHUP that watchHangup caught, after the
// cleanup: with the handler reset, the signal takes its default action.
// The exit status 129 (128 + SIGHUP) is the fallback when the signal does
// not end the process at once. A variable, so a test can record the call.
var raiseHangup = func() {
	signal.Reset(syscall.SIGHUP)
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(time.Second)
	os.Exit(128 + int(syscall.SIGHUP))
}
