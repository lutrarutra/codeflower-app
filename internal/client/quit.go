package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"
)

// DefaultWrapUp is how long a quitting client waits for the chats in its
// sandboxes to wrap up before it stops the sandboxes anyway.
const DefaultWrapUp = 10 * time.Minute

// States of a sandbox while the client quits.
const (
	// The bridge was asked to wrap up its chats.
	quitAsked = "asked"
	// Its chats wrapped up; the sandbox was stopped.
	quitDone = "done"
	// Its bridge cannot wrap up on request (an older image) or did not
	// answer; it stops with the others.
	quitUnsupported = "unsupported"
	// It stopped by itself meanwhile.
	quitStopped = "stopped"
)

// quitting is a quit in progress: the client asked the chats in the
// sandboxes it started to wrap up, and stops each sandbox when they are done.
type quitting struct {
	Started  time.Time         `json:"started"`
	Deadline time.Time         `json:"deadline"`
	Workers  map[string]string `json:"workers"`
	cancel   context.CancelFunc
}

// QuitPlan says what quitting does: which sandboxes stop, how many of their
// chats are asked to wrap up first, and which keep running elsewhere.
type QuitPlan struct {
	// Sandboxes this client started that are still running; they stop.
	Sandboxes int `json:"sandboxes"`
	// Working: their chats with a turn running or a question waiting.
	Working int `json:"working"`
	// Busy: the sandboxes with such chats.
	Busy int `json:"busy"`
	// Elsewhere: sandboxes added by address, which keep running.
	Elsewhere int `json:"elsewhere"`
	// Minutes the client waits at most for the chats to wrap up.
	Minutes int `json:"minutes"`
	// Quitting: a wrap-up in progress.
	Quitting *quitting `json:"quitting,omitempty"`
	// Stopping: the client is stopping its sandboxes and quits.
	Stopping bool `json:"stopping"`
}

func (s *Server) wrapUpWait() time.Duration {
	if s.options.WrapUp > 0 {
		return s.options.WrapUp
	}
	return DefaultWrapUp
}

// busyWorkers are the running sandboxes this client started, and their
// working or waiting chats, as last polled.
func (s *Server) busyWorkers() (running []*worker, busy map[*worker]int) {
	s.mu.Lock()
	items := make([]*worker, 0, len(s.order))
	for _, id := range s.order {
		items = append(items, s.workers[id])
	}
	s.mu.Unlock()
	busy = map[*worker]int{}
	for _, item := range items {
		if item.Source != sourceStarted {
			continue
		}
		item.mu.Lock()
		status, activity := item.status, item.activity
		item.mu.Unlock()
		if status != "starting" && status != "ready" {
			continue
		}
		running = append(running, item)
		if activity != nil && activity.Working+activity.Waiting > 0 {
			busy[item] = activity.Working + activity.Waiting
		}
	}
	return running, busy
}

func (s *Server) quitPlan() QuitPlan {
	running, busy := s.busyWorkers()
	plan := QuitPlan{Sandboxes: len(running), Busy: len(busy), Minutes: int(math.Ceil(s.wrapUpWait().Minutes()))}
	for _, chats := range busy {
		plan.Working += chats
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.workers {
		if item.Source != sourceStarted {
			plan.Elsewhere++
		}
	}
	if s.quitting != nil {
		copied := *s.quitting
		copied.Workers = make(map[string]string, len(s.quitting.Workers))
		for id, state := range s.quitting.Workers {
			copied.Workers[id] = state
		}
		plan.Quitting = &copied
	}
	select {
	case <-s.quit:
		plan.Stopping = true
	default:
	}
	return plan
}

// beginQuit quits the client. With wrapUp and chats working in its
// sandboxes, it first asks them to wrap up and waits for them (at most
// wrapUpWait); otherwise, or when a wrap-up already runs and wrapUp is
// false, it stops the sandboxes now.
func (s *Server) beginQuit(wrapUp bool) QuitPlan {
	select {
	case <-s.quit:
		// Already stopping.
		return s.quitPlan()
	default:
	}
	s.mu.Lock()
	if s.quitting != nil {
		s.mu.Unlock()
		if !wrapUp {
			s.stopNow()
		}
		return s.quitPlan()
	}
	s.mu.Unlock()
	_, busy := s.busyWorkers()
	if !wrapUp || len(busy) == 0 {
		s.closeQuit()
		return s.quitPlan()
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	state := &quitting{Started: now, Deadline: now.Add(s.wrapUpWait()), Workers: map[string]string{}, cancel: cancel}
	items := make([]*worker, 0, len(busy))
	for item := range busy {
		items = append(items, item)
		state.Workers[item.ID] = quitAsked
	}
	s.mu.Lock()
	if s.quitting != nil {
		// Another quit began meanwhile.
		s.mu.Unlock()
		cancel()
		return s.quitPlan()
	}
	s.quitting = state
	s.mu.Unlock()
	working := 0
	for _, chats := range busy {
		working += chats
	}
	fmt.Fprintf(s.options.Log, "\nAsking %d chat(s) in %d sandbox(es) to save their work and wrap up before stopping them (at most %s).\n",
		working, len(items), s.wrapUpWait().Round(time.Second))
	go s.wrapUp(ctx, state, items)
	return s.quitPlan()
}

func (s *Server) setQuitState(state *quitting, id, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quitting == state {
		state.Workers[id] = value
	}
}

// wrapUp asks each bridge to wrap up its chats, then stops each sandbox once
// its bridge says they are done, and quits when all are, at the deadline, or
// not at all when ctx ends (stop now, keep running).
func (s *Server) wrapUp(ctx context.Context, state *quitting, items []*worker) {
	client := &http.Client{Timeout: 5 * time.Second}
	minutes := int(math.Ceil(time.Until(state.Deadline).Minutes()))
	for _, item := range items {
		if err := askWrapUp(ctx, client, item, minutes); err != nil {
			fmt.Fprintf(s.options.Log, "Sandbox %s cannot wrap up (%v); it stops with the others\n", item.ID, err)
			s.setQuitState(state, item.ID, quitUnsupported)
		}
	}
	deadline := time.NewTimer(time.Until(state.Deadline))
	defer deadline.Stop()
	ticker := time.NewTicker(wrapUpPoll)
	defer ticker.Stop()
	for {
		pending := 0
		for _, item := range items {
			s.mu.Lock()
			current := state.Workers[item.ID]
			s.mu.Unlock()
			if current != quitAsked {
				continue
			}
			select {
			case <-item.done:
				s.setQuitState(state, item.ID, quitStopped)
				continue
			default:
			}
			if wrappedUp(ctx, client, item) {
				fmt.Fprintf(s.options.Log, "Sandbox %s wrapped up; stopping it\n", item.ID)
				s.setQuitState(state, item.ID, quitDone)
				item.stop()
				continue
			}
			pending++
		}
		if ctx.Err() != nil {
			return
		}
		if pending == 0 {
			s.closeQuit()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			fmt.Fprintln(s.options.Log, "The wrap-up took too long; stopping the sandboxes")
			s.closeQuit()
			return
		case <-ticker.C:
		}
	}
}

// wrapUpPoll is how often a quitting client asks the bridges whether their
// chats have wrapped up.
var wrapUpPoll = time.Second

var errCannotWrapUp = errors.New("its image is older than wrap-ups on request")

func bridgeURL(item *worker, path string) (string, string, bool) {
	item.mu.Lock()
	connection := item.connection
	item.mu.Unlock()
	if connection == nil {
		return "", "", false
	}
	return "http://" + net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port)) + path, connection.Token, true
}

func askWrapUp(ctx context.Context, client *http.Client, item *worker, minutes int) error {
	url, token, ok := bridgeURL(item, "/wrap-up")
	if !ok {
		return errors.New("it is not connected")
	}
	body, _ := json.Marshal(map[string]any{"reason": "quit", "minutes": max(1, minutes)})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	switch {
	case response.StatusCode == http.StatusAccepted:
		return nil
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusNotImplemented:
		return errCannotWrapUp
	default:
		return fmt.Errorf("the sandbox answered %s", response.Status)
	}
}

// wrappedUp says whether the bridge reports its wrap-up done.
func wrappedUp(ctx context.Context, client *http.Client, item *worker) bool {
	url, token, ok := bridgeURL(item, "/status")
	if !ok {
		return false
	}
	current, err := fetchActivity(ctx, client, url, token)
	return err == nil && current.WrapUp != nil && current.WrapUp.State == "done"
}

// closeQuit makes Run return, which stops every sandbox still running.
func (s *Server) closeQuit() {
	s.quitOnce.Do(func() { close(s.quit) })
}

// stopNow ends a wrap-up and stops the sandboxes at once.
func (s *Server) stopNow() {
	s.mu.Lock()
	state := s.quitting
	s.mu.Unlock()
	if state != nil {
		state.cancel()
	}
	s.closeQuit()
}

// keepRunning calls a wrap-up off: the client and its sandboxes stay up.
// Chats already asked finish their wrap-up turn. It fails when the client
// already stops its sandboxes.
func (s *Server) keepRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.quit:
		return false
	default:
	}
	if s.quitting != nil {
		s.quitting.cancel()
		s.quitting = nil
		fmt.Fprintln(s.options.Log, "Quitting was called off; the sandboxes keep running")
	}
	return true
}

func (s *Server) quitStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.quitPlan())
}

type quitRequest struct {
	// WrapUp: ask working chats to wrap up first (the default).
	WrapUp *bool `json:"wrapUp"`
}

func (s *Server) quitHandler(w http.ResponseWriter, r *http.Request) {
	var request quitRequest
	if err := readJSON(r, &request); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.beginQuit(request.WrapUp == nil || *request.WrapUp))
}

func (s *Server) cancelQuit(w http.ResponseWriter, _ *http.Request) {
	if !s.keepRunning() {
		writeError(w, http.StatusConflict, errors.New("the client already stops its sandboxes"))
		return
	}
	writeJSON(w, http.StatusOK, s.quitPlan())
}

// Control lets the terminal and the tray quit the client as the page does.
type Control struct {
	server *Server
	// URL opens the page, with its secret.
	URL string
}

// Plan says what quitting would do now.
func (c *Control) Plan() QuitPlan { return c.server.quitPlan() }

// Quit quits, asking working chats to wrap up first when wrapUp is set.
func (c *Control) Quit(wrapUp bool) QuitPlan { return c.server.beginQuit(wrapUp) }

// StopNow ends a wrap-up and stops the sandboxes at once.
func (c *Control) StopNow() { c.server.stopNow() }

// KeepRunning calls a wrap-up off.
func (c *Control) KeepRunning() bool { return c.server.keepRunning() }

// HandleSignals quits on the signals: the first interrupt asks working chats
// to wrap up, another one (or SIGTERM) stops the sandboxes at once. It
// returns when signals is closed.
func HandleSignals(control *Control, signals <-chan os.Signal, log io.Writer) {
	for received := range signals {
		plan := control.Plan()
		if received == syscall.SIGTERM || plan.Quitting != nil || plan.Stopping {
			if !plan.Stopping {
				fmt.Fprintln(log, "\nStopping the sandboxes now.")
			}
			control.StopNow()
			continue
		}
		if plan = control.Quit(true); plan.Quitting != nil {
			fmt.Fprintf(log, "Press Ctrl+C again to stop them now, or open %s to keep them running.\n", control.URL)
		}
	}
}
