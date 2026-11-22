package jobs

import (
	"maps"
	"os"
	"path/filepath"
	"time"
)

// tasksEvery is how often the broker reads the files of an array's tasks, to
// show which run and how those that ended did, between the scheduler's polls.
const tasksEvery = 5 * time.Second

// taskStates are an array's tasks' states from their files: done or failed by
// the result its sandbox wrote, else unfinished when it is given (how the job
// ended, for tasks that never finished), else running once its sandbox opened
// its output and queued before. Nil for a job that is no array.
func (t *Tracker) taskStates(record *Record, unfinished string) map[int]string {
	if record.Array == "" {
		return nil
	}
	array, err := ParseArray(record.Array, 0)
	if err != nil {
		return nil
	}
	results, _ := t.Ledger.Results(record.ID)
	states := make(map[int]string, len(array.Indices))
	for _, index := range array.Indices {
		if result, ok := results[index]; ok {
			states[index] = StateFailed
			if result.ExitCode == 0 {
				states[index] = StateDone
			}
			continue
		}
		if unfinished != "" {
			states[index] = unfinished
			continue
		}
		states[index] = StateQueued
		if _, err := os.Stat(filepath.Join(t.Ledger.LogDir(record.ID), OutputFile(&index))); err == nil {
			states[index] = StateRunning
		}
	}
	return states
}

// followArray has FollowTasks read an active array's tasks.
func (t *Tracker) followArray(record *Record) {
	if record.Array == "" {
		return
	}
	if t.arrays == nil {
		t.arrays = map[string]bool{}
	}
	t.arrays[record.ID] = true
}

// FollowTasks brings the states of the active arrays' tasks up to date from
// their files, which costs no call to the scheduler; an array whose first task
// started runs.
func (t *Tracker) FollowTasks() {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	for id := range t.arrays {
		record, err := t.Ledger.Load(id)
		if err != nil || !Active(record.State) || record.SchedulerID == "" {
			delete(t.arrays, id)
			continue
		}
		states := t.taskStates(record, "")
		changed := !maps.Equal(states, record.TaskStates)
		if record.State == StateQueued {
			for _, state := range states {
				if state != StateQueued {
					now := time.Now().UTC().Truncate(time.Second)
					record.State = StateRunning
					if record.Started == nil {
						record.Started = &now
					}
					changed = true
					break
				}
			}
		}
		if changed {
			record.TaskStates = states
			t.save(record)
		}
	}
}
