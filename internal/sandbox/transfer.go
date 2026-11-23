package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// A copy or move of a project, or of its workspace, that copies between
// file systems can take hours. One that is stopped (Ctrl+C) or fails keeps
// what it copied, in hidden folders (stagingPath), and says so in a
// Transfer next to the project; the same command continues it, copying
// only what is missing, and --abort (AbortTransfer) drops it. Until it is
// complete, the project stays where it was and as it was.

// The operations a Transfer is of, as the commands are named.
const (
	TransferCopy          = "copy"
	TransferMove          = "move"
	TransferMoveWorkspace = "move-workspace"
)

// Transfer is an unfinished copy or move of a project, or of its workspace.
type Transfer struct {
	Operation string `json:"operation"`
	// To is where it goes: the project folder for a copy or move, the
	// workspace folder for a move of the workspace.
	To string `json:"to"`
	// Partial are the hidden folders it copies into, kept for the next
	// attempt.
	Partial []string `json:"partial,omitempty"`
	// Removing are originals left to remove once a move of the workspace is
	// complete.
	Removing []string  `json:"removing,omitempty"`
	Started  time.Time `json:"started"`
}

// TransferFile is where the project's unfinished Transfer is: next to the
// project folder, out of the sandbox's reach, like its settings.
func TransferFile(state string) string {
	return filepath.Clean(state) + ".transfer.json"
}

// ReadTransfer reads the project's unfinished transfer, if it has one.
func ReadTransfer(state string) (Transfer, bool, error) {
	var transfer Transfer
	data, err := os.ReadFile(TransferFile(state))
	if errors.Is(err, os.ErrNotExist) {
		return transfer, false, nil
	}
	if err != nil {
		return transfer, false, err
	}
	if err := json.Unmarshal(data, &transfer); err != nil {
		return transfer, false, fmt.Errorf("%s: %w", TransferFile(state), err)
	}
	return transfer, true, nil
}

func writeTransfer(state string, transfer Transfer) error {
	if transfer.Started.IsZero() {
		transfer.Started = time.Now().UTC()
	}
	data, err := json.MarshalIndent(transfer, "", "  ")
	if err != nil {
		return err
	}
	return writePrivately(TransferFile(state), append(data, '\n'))
}

func removeTransfer(state string) {
	os.Remove(TransferFile(state))
}

// PendingTransferError refuses a copy or move of a project that has
// another one unfinished.
type PendingTransferError struct {
	Project  Project
	Transfer Transfer
}

func (e PendingTransferError) Error() string {
	return fmt.Sprintf("the project %s has an unfinished %s to %s (started %s): run that again to finish it, "+
		"or drop it with: codeflower projects %s %s --abort", e.Project.Name, e.Transfer.Operation, e.Transfer.To,
		e.Transfer.Started.Local().Format("2006-01-02 15:04"), e.Transfer.Operation, e.Project.State)
}

// openTransfer starts the operation on the project, or continues the same
// one, which then names where it goes (To); another unfinished one refuses.
func (p Project) openTransfer(operation string) (Transfer, bool, error) {
	transfer, found, err := ReadTransfer(p.State)
	if err != nil {
		return Transfer{}, false, err
	}
	if !found {
		return Transfer{Operation: operation}, false, nil
	}
	if transfer.Operation != operation {
		return Transfer{}, false, PendingTransferError{p, transfer}
	}
	return transfer, true, nil
}

// checkResumes refuses to continue a transfer to somewhere else than where
// it went.
func (p Project) checkResumes(transfer Transfer, resuming bool, to string) error {
	if resuming && transfer.To != to {
		return PendingTransferError{p, transfer}
	}
	return nil
}

// ours says whether a hidden partial folder is the transfer's, so the next
// attempt continues it; another one in the way is not touched.
func (t Transfer) ours(path string) bool {
	return slices.Contains(t.Partial, path)
}

// checkStaging refuses a hidden partial folder in the way that is not the
// transfer's own.
func (t Transfer) checkStaging(paths ...string) error {
	for _, path := range paths {
		if path != "" && exists(stagingPath(path)) && !t.ours(stagingPath(path)) {
			return fmt.Errorf("%s is in the way: an unfinished copy of something else? Remove it first", stagingPath(path))
		}
	}
	return nil
}

// AbortTransfer drops the project's unfinished copy or move: it removes
// what it copied so far. The project itself is as it was.
func AbortTransfer(ctx context.Context, p Project, progress Progress) (Transfer, error) {
	transfer, found, err := ReadTransfer(p.State)
	if err != nil {
		return transfer, err
	}
	if !found {
		return transfer, fmt.Errorf("the project %s has no unfinished copy or move", p.Name)
	}
	if len(transfer.Partial) == 0 && len(transfer.Removing) > 0 {
		return transfer, fmt.Errorf("the %s of %s to %s is complete; only removing its original is left: "+
			"run it again to finish", transfer.Operation, p.Name, transfer.To)
	}
	m, stop := newMeter(progress)
	defer stop()
	m.phase("Removing the partial copy of")
	work := m.work(ctx)
	for _, path := range transfer.Partial {
		if !isStaging(path) {
			continue // never anything but what a transfer made
		}
		if err := work.remove(path); err != nil {
			return transfer, removeError(err)
		}
	}
	removeTransfer(p.State)
	return transfer, nil
}
