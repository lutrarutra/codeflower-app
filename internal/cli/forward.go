package cli

import (
	"context"
	"os"
	"sync"

	"codeflower-app/internal/sandbox"
)

// forwardFlagHelp is --forward-to's help, the same for `sandbox` and `run`.
const forwardFlagHelp = "also open the worker's port on `HOST`'s 127.0.0.1 through ssh -R while the sandbox runs, " +
	"e.g. the login node a compute node's port cannot be reached from (in a batch job: \"$SLURM_SUBMIT_HOST\")"

// forwardWhenReady wraps ready so that, with --forward-to HOST, the worker's
// port is forwarded to HOST once the worker listens (its port is known only
// then). Call stop once the sandbox has stopped: the tunnel stays up while
// the sandbox wraps up, not after.
func forwardWhenReady(host, engine, listenHost string, ready func(sandbox.Connection)) (wrapped func(sandbox.Connection), stop func()) {
	if host == "" {
		return ready, func() {}
	}
	var mu sync.Mutex
	var stopForward func()
	var once sync.Once
	wrapped = func(connection sandbox.Connection) {
		once.Do(func() {
			if connection.Port <= 0 {
				return
			}
			forward := sandbox.Forward{
				Host:   host,
				Port:   connection.Port,
				Target: sandbox.ForwardTarget(engine, listenHost),
				Log:    os.Stderr,
			}
			mu.Lock()
			stopForward = forward.Start(context.Background())
			mu.Unlock()
		})
		ready(connection)
	}
	return wrapped, func() {
		mu.Lock()
		defer mu.Unlock()
		if stopForward != nil {
			stopForward()
		}
	}
}
