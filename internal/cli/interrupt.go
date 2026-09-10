package cli

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

// WatchInterrupt cancels the returned context on the first SIGINT or
// SIGTERM and reports the exit code that signal maps to. The bash
// entrypoint set no trap: the shell died of the signal, the process exit
// code was 128+n (130 for SIGINT, 143 for SIGTERM) and nothing was
// printed. The Go binary cancels the command context instead, so every
// Runner child and every wait loop stops, and main exits with the same
// code. The watch ends with the first signal: a second one takes the
// default action again and ends a process that did not stop on its own.
//
// exitCode stops the watch and returns 0 while no signal arrived, else
// 128+n. Call it once, after the command returned.
func WatchInterrupt(parent context.Context) (ctx context.Context, exitCode func() int) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	var got atomic.Int32
	go func() {
		s, ok := <-ch
		if !ok {
			return
		}
		signal.Stop(ch)
		if n, isSignal := s.(syscall.Signal); isSignal {
			got.Store(int32(n))
		}
		cancel()
	}()
	return ctx, func() int {
		signal.Stop(ch)
		close(ch)
		cancel()
		if n := got.Load(); n != 0 {
			return 128 + int(n)
		}
		return 0
	}
}
