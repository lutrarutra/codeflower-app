package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"codeflower-app/internal/client"
	"codeflower-app/internal/config"
	"codeflower-app/internal/sandbox"
)

// doublePress is how soon a second Ctrl+C must follow the first to stop a
// wrap-up at once. A variable for tests.
var doublePress = 3 * time.Second

// sandboxQuit is what Ctrl+C does to a sandbox the terminal runs (sandbox,
// run). The first one stops it when no agent works; otherwise it asks the
// working chats to save their work and wrap up, and stops the sandbox once
// they are done or the time is up. Two within doublePress stop it at once,
// and one more while it stops kills it. SIGTERM and SIGHUP stop it at once.
type sandboxQuit struct {
	log io.Writer
	// wrapUp is how long the chats have at most.
	wrapUp time.Duration
	// poll is how often the bridge is asked whether its chats are done.
	poll       time.Duration
	connection atomic.Pointer[sandbox.Connection]
	cancel     context.CancelFunc
	kill       chan struct{}
	killOnce   sync.Once
}

// quitOnSignals watches the signals for a sandbox the terminal runs (see
// sandboxQuit). ctx ends when the sandbox should stop, kill closes when it
// should be killed, connected gives the worker's connection once it listens
// (before, Ctrl+C stops it at once), and stop ends the watch.
func quitOnSignals(parent context.Context, configDirectory string) (ctx context.Context, kill <-chan struct{}, connected func(sandbox.Connection), stop func()) {
	wrapUp := client.DefaultWrapUp
	if loaded, err := config.Load(config.Directory(configDirectory)); err == nil && loaded.Client.WrapUpMinutes > 0 {
		wrapUp = time.Duration(loaded.Client.WrapUpMinutes) * time.Minute
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	ctx, quit, done := watchQuit(parent, signals, os.Stderr, wrapUp, time.Second)
	return ctx, quit.kill, quit.connected, func() {
		signal.Stop(signals)
		done()
	}
}

// watchQuit runs the watch of quitOnSignals on signals.
func watchQuit(parent context.Context, signals <-chan os.Signal, log io.Writer, wrapUp, poll time.Duration) (context.Context, *sandboxQuit, func()) {
	ctx, cancel := context.WithCancel(parent)
	quit := &sandboxQuit{log: log, wrapUp: wrapUp, poll: poll, cancel: cancel, kill: make(chan struct{})}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		quit.run(ctx, signals, done)
	}()
	var once sync.Once
	return ctx, quit, func() {
		once.Do(func() {
			close(done)
			<-finished
			cancel()
		})
	}
}

func (q *sandboxQuit) connected(connection sandbox.Connection) {
	q.connection.Store(&connection)
}

// wrapUpAsked is the bridge's answer to a wrap-up: how many chats it asked.
type wrapUpAsked struct {
	connection sandbox.Connection
	chats      int
	err        error
}

const (
	quitRunning = iota
	// Asking the bridge to wrap up.
	quitAsking
	// Waiting for the chats to wrap up.
	quitWrapping
	quitStopping
)

func (q *sandboxQuit) run(ctx context.Context, signals <-chan os.Signal, done <-chan struct{}) {
	state := quitRunning
	var lastInterrupt time.Time
	asked := make(chan wrapUpAsked, 1)
	wrappedUp := make(chan string, 1)
	wrapping, stopWrapping := context.WithCancel(context.Background())
	defer stopWrapping()
	stopNow := func(message string) {
		fmt.Fprintln(q.log, message)
		state = quitStopping
		stopWrapping()
		q.cancel()
	}
	const killHint = " Press Ctrl+C again to kill it now."
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			// The sandbox stopped by itself, or the command ends.
			if state != quitStopping {
				state = quitStopping
				stopWrapping()
			}
			ctx = context.Background()
			continue
		case received := <-signals:
			now := time.Now()
			interrupt := received == os.Interrupt
			double := interrupt && !lastInterrupt.IsZero() && now.Sub(lastInterrupt) <= doublePress
			if interrupt {
				lastInterrupt = now
			}
			switch {
			case state == quitStopping:
				q.killOnce.Do(func() {
					fmt.Fprintln(q.log, "\nKilling the sandbox.")
					close(q.kill)
				})
			case !interrupt:
				stopNow("\nStopping the sandbox." + killHint)
			case state == quitRunning:
				connection := q.connection.Load()
				if connection == nil {
					stopNow("\nStopping the sandbox." + killHint)
					continue
				}
				state = quitAsking
				go func() { asked <- q.ask(wrapping, *connection) }()
			case double:
				stopNow("\nStopping the sandbox now, without waiting for the chats." + killHint)
			default:
				fmt.Fprintf(q.log, "\nThe chats are still wrapping up; press Ctrl+C again within %s to stop the sandbox now.\n", doublePress)
			}
		case answer := <-asked:
			if state != quitAsking {
				continue
			}
			switch {
			case answer.err != nil:
				stopNow(fmt.Sprintf("\nThe chats cannot wrap up (%v); stopping the sandbox.%s", answer.err, killHint))
			case answer.chats == 0:
				stopNow("\nNo agent is working; stopping the sandbox." + killHint)
			default:
				state = quitWrapping
				fmt.Fprintf(q.log, "\nAsking %s to save their work and wrap up; the sandbox stops once they are done, at most in %s.\n"+
					"Press Ctrl+C twice within %s to stop it now.\n",
					counted(answer.chats, "working chat", "working chats"), q.wrapUp.Round(time.Second), doublePress)
				go q.wait(wrapping, answer.connection, wrappedUp)
			}
		case message := <-wrappedUp:
			if state == quitWrapping {
				stopNow(message)
			}
		}
	}
}

// ask asks the bridge to wrap up its working chats, the agents' and their
// subagents' that work: idle chats are left alone.
func (q *sandboxQuit) ask(ctx context.Context, connection sandbox.Connection) wrapUpAsked {
	minutes := max(1, int(math.Ceil(q.wrapUp.Minutes())))
	body, _ := json.Marshal(map[string]any{"reason": "quit", "minutes": minutes})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.BridgeURL("/wrap-up"), bytes.NewReader(body))
	if err != nil {
		return wrapUpAsked{err: err}
	}
	request.Header.Set("Authorization", "Bearer "+connection.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := connection.Client(5 * time.Second).Do(request)
	if err != nil {
		return wrapUpAsked{err: errors.New("the sandbox did not answer")}
	}
	defer response.Body.Close()
	var answer struct {
		Chats int `json:"chats"`
	}
	switch {
	case response.StatusCode == http.StatusAccepted:
		if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
			return wrapUpAsked{err: fmt.Errorf("the sandbox answered: %w", err)}
		}
		return wrapUpAsked{connection: connection, chats: answer.Chats}
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusNotImplemented:
		return wrapUpAsked{err: errors.New("the sandbox image is older than wrap-ups on request")}
	default:
		return wrapUpAsked{err: fmt.Errorf("the sandbox answered %s", response.Status)}
	}
}

// wait sends on wrappedUp once the bridge says its chats are done, or the
// time is up.
func (q *sandboxQuit) wait(ctx context.Context, connection sandbox.Connection, wrappedUp chan<- string) {
	deadline := time.NewTimer(q.wrapUp)
	defer deadline.Stop()
	ticker := time.NewTicker(q.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			wrappedUp <- "\nThe wrap-up took too long; stopping the sandbox. Press Ctrl+C again to kill it now."
			return
		case <-ticker.C:
		}
		if q.isDone(ctx, connection) {
			wrappedUp <- "\nThe chats wrapped up; stopping the sandbox."
			return
		}
	}
}

func (q *sandboxQuit) isDone(ctx context.Context, connection sandbox.Connection) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.BridgeURL("/status"), nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+connection.Token)
	response, err := connection.Client(5 * time.Second).Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	var status struct {
		WrapUp *struct {
			State string `json:"state"`
		} `json:"wrapUp"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&status) != nil {
		return false
	}
	return status.WrapUp != nil && status.WrapUp.State == "done"
}
