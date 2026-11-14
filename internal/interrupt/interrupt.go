// Package interrupt maps the signals that stop lo to the end of a run.
// The first SIGINT or SIGTERM, and a SIGHUP while a part of lo catches it
// (CatchHangup), cancel the command context: each caller returns and runs
// its deferred cleanup, and lo then ends as the bash entrypoint did.
//
// It is a package of its own (cli re-exports Watch as WatchInterrupt) so
// that a driver can catch a SIGHUP for the command context: cli imports
// the drivers.
package interrupt

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// watch is the state of one Watch, in the context that Watch returns.
type watch struct {
	cancel context.CancelFunc
	got    atomic.Int32 // the first signal that cancelled the context
}

type watchKey struct{}

// hungUp is set when a SIGHUP was the first signal of the run: Exit then
// ends lo with the SIGHUP.
var hungUp atomic.Bool

// Watch cancels the returned context on the first SIGINT or SIGTERM and
// reports the exit code that signal maps to. The bash entrypoint set no
// trap: the shell died of the signal, the process exit code was 128+n
// (130 for SIGINT, 143 for SIGTERM) and nothing was printed. The Go binary
// cancels the command context instead, so every Runner child and every
// wait loop stops, and main exits with the same code. The watch ends with
// the first signal: a second one takes the default action again and ends
// a process that did not stop on its own.
//
// Watch does not catch SIGHUP: signal.Notify would undo the SIGHUP ignore
// of `nohup lo …`. A part of lo that must remove files on a SIGHUP catches
// it for its time with CatchHangup.
//
// exitCode stops the watch and returns 0 while no signal arrived, else
// 128+n. Call it once, after the command returned.
func Watch(parent context.Context) (ctx context.Context, exitCode func() int) {
	ctx, cancel := context.WithCancel(parent)
	w := &watch{cancel: cancel}
	ctx = context.WithValue(ctx, watchKey{}, w)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s, ok := <-ch
		if !ok {
			return
		}
		signal.Stop(ch)
		// Signal numbers are small (below 128 on every platform Go
		// supports), so the narrowing cannot overflow; the guard makes
		// that explicit for the static checkers.
		if n, isSignal := s.(syscall.Signal); isSignal && n > 0 && n < 128 {
			w.got.CompareAndSwap(0, int32(n)) // #nosec G115 -- bounded by the guard above
		}
		cancel()
	}()
	return ctx, func() int {
		signal.Stop(ch)
		close(ch)
		cancel()
		if n := w.got.Load(); n != 0 {
			return 128 + int(n)
		}
		return 0
	}
}

// CatchHangup catches a SIGHUP until stop is called (bash: the EXIT trap
// of a subshell, which bash runs on a SIGHUP too). A SIGHUP cancels the
// returned context and the command context of the Watch in ctx: the
// callers return and run their cleanup, and Exit ends lo with the SIGHUP
// after that. Without a Watch in ctx, only the returned context ends. It
// does nothing when SIGHUP is ignored (nohup lo …): signal.Notify would
// undo the ignore. The catch ends with the first SIGHUP: a second one
// takes the default action again. stop reports whether a SIGHUP came.
//
// A SIGHUP that came before stop counts, also when it is still in the
// channel as stop runs: stop acts on it as on any caught SIGHUP. A SIGHUP
// after stop takes the default action.
func CatchHangup(ctx context.Context) (context.Context, func() bool) {
	if signal.Ignored(syscall.SIGHUP) {
		return ctx, func() bool { return false }
	}
	w, _ := ctx.Value(watchKey{}).(*watch)
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	finished := make(chan struct{})
	var got atomic.Bool
	hangup := func() {
		got.Store(true)
		if w != nil {
			if w.got.CompareAndSwap(0, int32(syscall.SIGHUP)) {
				hungUp.Store(true)
			}
			w.cancel()
		}
		cancel()
	}
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		defer close(finished)
		beforeCatchWait(done)
		select {
		case <-ch:
			signal.Stop(ch)
			hangup()
		case <-done:
		}
	}()
	return ctx, func() bool {
		// After signal.Stop, a SIGHUP that came before it is in ch (or
		// the goroutine has it): no SIGHUP is delivered to ch after
		// that. select picks done or ch at random when both are ready,
		// so the goroutine can end without the SIGHUP: stop waits for
		// the goroutine, then takes a SIGHUP that is still in ch.
		signal.Stop(ch)
		close(done)
		<-finished
		select {
		case <-ch:
			hangup()
		default:
		}
		cancel()
		return got.Load()
	}
}

// beforeCatchWait runs in the goroutine of CatchHangup before it waits for
// a SIGHUP or for stop. Its argument closes when stop runs. A test sets it
// to hold the goroutine until stop runs, so that a SIGHUP is still in the
// channel then.
var beforeCatchWait = func(<-chan struct{}) {}

// Exit ends lo with code. When a SIGHUP was the first signal of the run
// (CatchHangup), it ends lo with that SIGHUP instead: with the handler
// reset, the signal takes its default action, as the bash entrypoint
// died of it. The exit status 129 (128 + SIGHUP) is the fallback when the
// signal does not end the process at once. main calls it last, after each
// deferred cleanup ran.
func Exit(code int) {
	if hungUp.Load() {
		signal.Reset(syscall.SIGHUP)
		_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
		time.Sleep(time.Second)
		code = 128 + int(syscall.SIGHUP)
	}
	os.Exit(code)
}
