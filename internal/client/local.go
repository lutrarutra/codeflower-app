package client

import (
	"context"
	"net"
	"os"
	"strconv"
	"time"

	"codeflower-app/internal/sandbox"
)

// watchLocal lists the sandboxes started on this computer outside this
// client (`codeflower run`, `interactive` or `task` in a terminal, or another
// client) until ctx ends, so they need no pasted line. They come and go with
// their runs and are never saved.
func (s *Server) watchLocal(ctx context.Context) {
	ticker := time.NewTicker(activityInterval)
	defer ticker.Stop()
	for {
		s.syncLocal(sandbox.ListRunning())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// syncLocal adds the runs not listed yet, connects those whose bridge now
// listens, and drops those that ended.
func (s *Server) syncLocal(runs []sandbox.Running) {
	own := os.Getpid()
	seen := map[string]bool{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, run := range runs {
		if run.PID == own {
			// Started by this client: already listed as started.
			continue
		}
		id := localID(run)
		seen[id] = true
		item := s.workers[id]
		if item == nil {
			item = s.addLocal(id, run)
		}
		if run.Kind == "task" {
			continue
		}
		item.mu.Lock()
		waiting := item.connection == nil
		item.mu.Unlock()
		if waiting {
			if connection, ok := run.ReadConnection(); ok {
				item.mu.Lock()
				item.connection, item.url = &connection, connection.URL
				item.address = net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port))
				item.mu.Unlock()
				go item.watchActivity(item.watching, s.noticesFor(item))
			}
		}
	}
	for _, id := range append([]string(nil), s.order...) {
		item := s.workers[id]
		if item.Source != sourceLocal || seen[id] {
			continue
		}
		item.cancel()
		item.relays.closeAll()
		delete(s.workers, id)
		s.order = removeString(s.order, id)
	}
}

// localID keeps a run's id apart from the ids of started and added workers.
func localID(run sandbox.Running) string {
	id := run.ID
	if len(id) > 8 {
		id = id[:8]
	}
	return "local-" + id
}

// addLocal lists a run found on this computer; s.mu is held.
func (s *Server) addLocal(id string, run sandbox.Running) *worker {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	close(done)
	status := "starting"
	if run.Kind == "task" {
		status = "task"
	}
	item := &worker{
		ID:       id,
		Source:   sourceLocal,
		Kind:     run.Kind,
		Engine:   run.Engine,
		Project:  run.Project,
		State:    run.State,
		Inputs:   []string{},
		Outputs:  []string{},
		Code:     []string{},
		Skills:   []string{},
		Started:  run.Started,
		agent:    run.Agent,
		label:    sandbox.AgentLabel(run.Agent),
		status:   status,
		run:      &run,
		cancel:   cancel,
		watching: ctx,
		done:     done,
	}
	s.workers[id] = item
	s.order = append(s.order, id)
	return item
}
