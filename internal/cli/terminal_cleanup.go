package cli

import "context"

// restoreOnCommandSignal restores the terminal even while its reader or writer
// is blocked. Teardown either restores it itself or joins the signal callback.
func restoreOnCommandSignal(ctx context.Context, restore, afterRestore func()) func() {
	exit := func(s commandSignal) {
		afterRestore()
		exitProcess(128 + int(s.Signal))
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		if s, ok := context.Cause(ctx).(commandSignal); ok {
			restore()
			exit(s)
		}
	})
	return func() {
		if !stop() {
			<-done
			if _, ok := context.Cause(ctx).(commandSignal); ok {
				return
			}
		}
		restore()
		if s, ok := context.Cause(ctx).(commandSignal); ok {
			exit(s)
		}
	}
}
