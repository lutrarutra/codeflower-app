package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The app keeps each job in the ledger, a folder beside the project
// (PROJECT.jobs/ID/) that no sandbox mounts: the spec its sandbox starts
// from, the batch script, its record and the results its sandbox writes. A
// copy of the record, and the job's output, are in the project's jobs folder
// (JOBS/ID/), where the agent reads them.
const (
	SpecFile      = "spec.json"
	ScriptFile    = "job.sh"
	SubmitFile    = "job.sub"
	RecordFile    = "job.json"
	ProjectFolder = "jobs"
	// ContainerFolder is the project's jobs folder in the sandbox.
	ContainerFolder = "/codeflower-state/jobs"
)

// LedgerDir is a project's ledger.
func LedgerDir(state string) string {
	return filepath.Clean(state) + ".jobs"
}

// Spec is what `codeflower job-run` starts a job's sandbox from; the app
// writes it in the ledger.
type Spec struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Project is the project folder, mounted at /codeflower-state.
	Project string `json:"project"`
	Engine  string `json:"engine"`
	Image   string `json:"image"`
	// ImageArchive, for Podman, is a saved copy of the image to load where
	// the engine does not have it (each job's node may have its own storage).
	ImageArchive string  `json:"imageArchive,omitempty"`
	Mounts       []Mount `json:"mounts"`
	// Allowlist, when set, isolates the job's network as the sandbox's.
	Isolated   bool              `json:"isolated,omitempty"`
	Allowlist  string            `json:"allowlist,omitempty"`
	EngineArgs string            `json:"engineArgs,omitempty"`
	Command    []string          `json:"command"`
	Workdir    string            `json:"workdir"`
	Env        map[string]string `json:"env,omitempty"`
	GPUs       int               `json:"gpus,omitempty"`
	// PassEnv are the scheduler's variables the sandbox gets; IndexEnv where
	// an array task's index is, in order.
	PassEnv  []string `json:"passEnv,omitempty"`
	IndexEnv []string `json:"indexEnv,omitempty"`
	// Indices are an array's task indices.
	Indices []int `json:"indices,omitempty"`
	// Ledger is the job's folder in the ledger, Logs its folder in the project.
	Ledger string `json:"ledger"`
	Logs   string `json:"logs"`
}

// Mount is a folder of the sandbox, which the job's sandbox mounts the same way.
type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// Record is a job as the app follows it. A copy is in the project, for the
// agent and the windows.
type Record struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Scheduler   string    `json:"scheduler"`
	SchedulerID string    `json:"schedulerId,omitempty"`
	State       string    `json:"state"`
	Resources   Resources `json:"resources"`
	GPUType     string    `json:"gpuType,omitempty"`
	Array       string    `json:"array,omitempty"`
	Tasks       int       `json:"tasks,omitempty"`
	After       []string  `json:"after,omitempty"`
	Command     []string  `json:"command"`
	Workdir     string    `json:"workdir"`
	// Logs is the job's folder in the sandbox, with output.log (or
	// output-INDEX.log for each array task) and the scheduler's own log.
	Logs      string `json:"logs"`
	Agent     string `json:"agent,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Notify    bool   `json:"notify,omitempty"`
	Approved  string `json:"approved,omitempty"`

	Submitted time.Time  `json:"submitted"`
	Updated   time.Time  `json:"updated"`
	Started   *time.Time `json:"started,omitempty"`
	Ended     *time.Time `json:"ended,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
	// Outcome counts an array's tasks by how they ended.
	Outcome *Outcome `json:"outcome,omitempty"`
	Error   string   `json:"error,omitempty"`

	// Host and PID are where a local job runs.
	Host string `json:"host,omitempty"`
	PID  int    `json:"pid,omitempty"`
}

// Outcome counts an array's tasks by how they ended.
type Outcome struct {
	Done      int `json:"done"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled,omitempty"`
	Lost      int `json:"lost,omitempty"`
}

// Result is what a job's sandbox wrote when it ended.
type Result struct {
	Index    *int      `json:"index,omitempty"`
	ExitCode int       `json:"exitCode"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended"`
	Node     string    `json:"node,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// ResultFile is where a task's result goes: result.json, or
// result-INDEX.json for an array's task.
func ResultFile(index *int) string {
	if index == nil {
		return "result.json"
	}
	return "result-" + strconv.Itoa(*index) + ".json"
}

// OutputFile is where a task's output goes in the job's project folder.
func OutputFile(index *int) string {
	if index == nil {
		return "output.log"
	}
	return "output-" + strconv.Itoa(*index) + ".log"
}

// Ledger is a project's ledger.
type Ledger struct {
	Dir     string
	Project string
}

// OpenLedger returns a project's ledger, creating it privately.
func OpenLedger(state string) (Ledger, error) {
	ledger := Ledger{Dir: LedgerDir(state), Project: filepath.Clean(state)}
	if err := os.MkdirAll(ledger.Dir, 0o700); err != nil {
		return ledger, fmt.Errorf("could not create the jobs folder %s: %w", ledger.Dir, err)
	}
	return ledger, os.Chmod(ledger.Dir, 0o700)
}

// JobDir is a job's folder in the ledger.
func (l Ledger) JobDir(id string) string { return filepath.Join(l.Dir, id) }

// LogDir is a job's folder in the project.
func (l Ledger) LogDir(id string) string { return filepath.Join(l.Project, ProjectFolder, id) }

var jobID = regexp.MustCompile(`^j-[0-9a-f]{6,16}$`)

// IsJobID says whether a value is a codeflower job id.
func IsJobID(value string) bool { return jobID.MatchString(value) }

// Create makes a new job's folders, in the ledger and in the project.
func (l Ledger) Create() (string, error) {
	for range 20 {
		buffer := make([]byte, 3)
		_, _ = rand.Read(buffer)
		id := "j-" + hex.EncodeToString(buffer)
		if err := os.Mkdir(l.JobDir(id), 0o700); errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			return "", err
		}
		if err := os.MkdirAll(l.LogDir(id), 0o700); err != nil {
			return "", err
		}
		return id, nil
	}
	return "", fmt.Errorf("could not choose a job id")
}

// WriteFile writes a file of a job's ledger folder atomically.
func (l Ledger) WriteFile(id, name string, data []byte, mode os.FileMode) error {
	return writeAtomically(filepath.Join(l.JobDir(id), name), data, mode)
}

// Save writes a record to the ledger and its copy to the project.
func (l Ledger) Save(record *Record) error {
	record.Updated = time.Now().UTC().Truncate(time.Second)
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := l.WriteFile(record.ID, RecordFile, data, 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(l.LogDir(record.ID), 0o700); err != nil {
		return err
	}
	return writeAtomically(filepath.Join(l.LogDir(record.ID), RecordFile), data, 0o600)
}

// Load reads a job's record from the ledger.
func (l Ledger) Load(id string) (*Record, error) {
	if !IsJobID(id) {
		return nil, fmt.Errorf("%q is not a job id", id)
	}
	data, err := os.ReadFile(filepath.Join(l.JobDir(id), RecordFile))
	if err != nil {
		return nil, err
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("%s: %w", id, err)
	}
	return &record, nil
}

// Records are the project's jobs, newest first.
func (l Ledger) Records() ([]*Record, error) {
	entries, err := os.ReadDir(l.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []*Record
	for _, entry := range entries {
		if !entry.IsDir() || !IsJobID(entry.Name()) {
			continue
		}
		record, err := l.Load(entry.Name())
		if err != nil {
			// A job being created has no record yet.
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Submitted.After(records[j].Submitted) })
	return records, nil
}

// Find is a job by its codeflower or scheduler id.
func (l Ledger) Find(id string) (*Record, error) {
	if IsJobID(id) {
		return l.Load(id)
	}
	records, err := l.Records()
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.SchedulerID != "" && record.SchedulerID == id {
			return record, nil
		}
	}
	return nil, &RequestError{Code: CodeUnknown, Message: fmt.Sprintf("no job %s in this project", id)}
}

// Results are the results a job's sandboxes wrote, by array index (-1 for a
// job that is no array).
func (l Ledger) Results(id string) (map[int]Result, error) {
	entries, err := os.ReadDir(l.JobDir(id))
	if err != nil {
		return nil, err
	}
	results := map[int]Result{}
	for _, entry := range entries {
		name := entry.Name()
		index := -1
		switch {
		case name == "result.json":
		case strings.HasPrefix(name, "result-") && strings.HasSuffix(name, ".json"):
			number, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "result-"), ".json"))
			if err != nil {
				continue
			}
			index = number
		default:
			continue
		}
		data, err := os.ReadFile(filepath.Join(l.JobDir(id), name))
		if err != nil {
			continue
		}
		var result Result
		if json.Unmarshal(data, &result) == nil {
			results[index] = result
		}
	}
	return results, nil
}

// ReadSpec reads a job's spec.
func ReadSpec(path string) (Spec, error) {
	var spec Spec
	data, err := os.ReadFile(path)
	if err != nil {
		return spec, err
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return spec, fmt.Errorf("%s: %w", path, err)
	}
	if spec.ID == "" || spec.Image == "" || spec.Project == "" || len(spec.Command) == 0 || spec.Ledger == "" {
		return spec, fmt.Errorf("%s is not a job's spec", path)
	}
	return spec, nil
}

// WriteResult writes a task's result into its job's ledger folder.
func WriteResult(ledger string, result Result) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomically(filepath.Join(ledger, ResultFile(result.Index)), append(data, '\n'), 0o600)
}

// writeAtomically replaces a file with a temporary one, so readers on other
// nodes never see half of it.
func writeAtomically(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		os.Remove(name)
		return err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
