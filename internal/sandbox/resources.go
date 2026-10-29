package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Resources is what the worker may use: the batch scheduler's allocation, or
// what the user gave with --cpus, --memory and --time-limit. Zero values are
// unknown. The sandbox reports them to the agent; Docker and Podman also
// enforce CPUs and memory.
type Resources struct {
	CPUs   int
	Memory int64 // bytes
	// GPUs is a count or the scheduler's list of devices.
	GPUs string
	End  time.Time
	// Source names where the figures come from, e.g. "Slurm job 123".
	Source string
}

// Empty says whether nothing is known.
func (r Resources) Empty() bool {
	return r.CPUs == 0 && r.Memory == 0 && r.GPUs == "" && r.End.IsZero()
}

// With returns r with the figures override sets replacing r's.
func (r Resources) With(override Resources) Resources {
	if override.Empty() {
		return r
	}
	if override.CPUs > 0 {
		r.CPUs = override.CPUs
	}
	if override.Memory > 0 {
		r.Memory = override.Memory
	}
	if override.GPUs != "" {
		r.GPUs = override.GPUs
	}
	if !override.End.IsZero() {
		r.End = override.End
	}
	if r.Source == "" {
		r.Source = override.Source
	} else {
		r.Source += ", " + override.Source
	}
	return r
}

// Summary is a one-line description for the launcher's output.
func (r Resources) Summary() string {
	var parts []string
	if r.CPUs > 0 {
		parts = append(parts, fmt.Sprintf("%d CPUs", r.CPUs))
	}
	if r.Memory > 0 {
		parts = append(parts, FormatBytes(r.Memory)+" memory")
	}
	if r.GPUs != "" {
		parts = append(parts, "GPUs "+r.GPUs)
	}
	if !r.End.IsZero() {
		parts = append(parts, "until "+r.End.Local().Format("2006-01-02 15:04"))
	}
	return strings.Join(parts, ", ") + " (" + r.Source + ")"
}

// environment adds the figures for the sandbox's codeflower-doctor.
func (r Resources) environment(values map[string]string) {
	if r.Empty() {
		return
	}
	values["CODEFLOWER_ALLOCATION_SOURCE"] = r.Source
	if r.CPUs > 0 {
		values["CODEFLOWER_ALLOCATION_CPUS"] = strconv.Itoa(r.CPUs)
	}
	if r.Memory > 0 {
		values["CODEFLOWER_ALLOCATION_MEMORY"] = strconv.FormatInt(r.Memory, 10)
	}
	if r.GPUs != "" {
		values["CODEFLOWER_ALLOCATION_GPUS"] = r.GPUs
	}
	if !r.End.IsZero() {
		values["CODEFLOWER_ALLOCATION_END"] = r.End.UTC().Format(time.RFC3339)
	}
}

// FormatBytes writes a size in GiB, or MiB below one GiB.
func FormatBytes(bytes int64) string {
	if bytes < 1<<30 {
		return fmt.Sprintf("%d MiB", bytes>>20)
	}
	return strconv.FormatFloat(float64(bytes)/(1<<30), 'f', -1, 64) + " GiB"
}

// ParseMemory reads a size such as 32G, 512M or 1.5T (binary units; a plain
// number is in bytes).
func ParseMemory(given string) (int64, error) {
	text := strings.TrimSpace(strings.ToUpper(given))
	text = strings.TrimSuffix(strings.TrimSuffix(text, "B"), "I")
	multiplier := int64(1)
	if text != "" {
		switch text[len(text)-1] {
		case 'K':
			multiplier = 1 << 10
		case 'M':
			multiplier = 1 << 20
		case 'G':
			multiplier = 1 << 30
		case 'T':
			multiplier = 1 << 40
		}
		if multiplier > 1 {
			text = text[:len(text)-1]
		}
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("invalid memory size %q: use a number with K, M, G or T, e.g. 32G", given)
	}
	return int64(number * float64(multiplier)), nil
}

// ParseTimeLimit reads a wall time as a Go duration (8h, 90m) or as the
// schedulers write it: [DAYS-][HOURS:]MINUTES:SECONDS.
func ParseTimeLimit(text string) (time.Duration, error) {
	text = strings.TrimSpace(text)
	if duration, err := time.ParseDuration(text); err == nil && duration > 0 {
		return duration, nil
	}
	if duration, ok := parseClock(text); ok && duration > 0 {
		return duration, nil
	}
	return 0, fmt.Errorf("invalid time limit %q: use e.g. 8h, 90m or 1-12:00:00", text)
}

// parseClock reads [DAYS-][HOURS:]MINUTES:SECONDS, as Slurm does.
func parseClock(text string) (time.Duration, bool) {
	days := 0
	if before, after, found := strings.Cut(text, "-"); found {
		var err error
		if days, err = strconv.Atoi(before); err != nil {
			return 0, false
		}
		text = after
	}
	fields := strings.Split(text, ":")
	if len(fields) < 2 || len(fields) > 3 {
		return 0, false
	}
	total := 0
	for _, field := range fields {
		number, err := strconv.Atoi(field)
		if err != nil || number < 0 {
			return 0, false
		}
		total = total*60 + number
	}
	return time.Duration(days)*24*time.Hour + time.Duration(total)*time.Second, true
}

// schedulerQuery runs a scheduler's command (squeue, qstat) on the host; the
// sandbox has neither the command nor the job's environment.
var schedulerQuery = func(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).Output()
	return string(output), err
}

// DetectResources reads the allocation of the batch job the launcher runs in:
// Slurm, PBS Pro/OpenPBS/Torque, LSF, Grid Engine or HTCondor. Outside a job
// it returns nothing.
func DetectResources(ctx context.Context, getenv func(string) string) Resources {
	switch {
	case getenv("SLURM_JOB_ID") != "":
		return slurmResources(ctx, getenv)
	case getenv("PBS_JOBID") != "":
		return pbsResources(ctx, getenv)
	case getenv("LSB_JOBID") != "":
		return Resources{
			CPUs:   atoi(getenv("LSB_DJOB_NUMPROC")),
			GPUs:   countList(getenv("CUDA_VISIBLE_DEVICES")),
			Source: "LSF job " + getenv("LSB_JOBID"),
		}
	case getenv("SGE_ROOT") != "" && getenv("JOB_ID") != "":
		return Resources{
			CPUs:   atoi(getenv("NSLOTS")),
			GPUs:   countList(getenv("CUDA_VISIBLE_DEVICES")),
			Source: "Grid Engine job " + getenv("JOB_ID"),
		}
	case getenv("_CONDOR_MACHINE_AD") != "" || getenv("_CONDOR_JOB_AD") != "":
		return condorResources(getenv)
	}
	return Resources{}
}

func slurmResources(ctx context.Context, getenv func(string) string) Resources {
	id := getenv("SLURM_JOB_ID")
	r := Resources{Source: "Slurm job " + id}
	r.CPUs = atoi(getenv("SLURM_CPUS_ON_NODE"))
	if r.CPUs == 0 {
		r.CPUs = atoi(getenv("SLURM_CPUS_PER_TASK"))
	}
	// Megabytes, per node or per CPU.
	if perNode := atoi(getenv("SLURM_MEM_PER_NODE")); perNode > 0 {
		r.Memory = int64(perNode) << 20
	} else if perCPU := atoi(getenv("SLURM_MEM_PER_CPU")); perCPU > 0 && r.CPUs > 0 {
		r.Memory = int64(perCPU*r.CPUs) << 20
	}
	if gpus := getenv("SLURM_GPUS_ON_NODE"); gpus != "" {
		r.GPUs = gpus
	} else {
		r.GPUs = countList(getenv("SLURM_JOB_GPUS"))
	}
	if end := atoi(getenv("SLURM_JOB_END_TIME")); end > 0 {
		r.End = time.Unix(int64(end), 0)
	} else if output, err := schedulerQuery(ctx, "squeue", "--noheader", "--jobs", id, "--format", "%e"); err == nil {
		// Local time; N/A or Unknown when the job has no limit.
		if end, err := time.ParseInLocation("2006-01-02T15:04:05", strings.TrimSpace(output), time.Local); err == nil {
			r.End = end
		}
	}
	return r
}

func pbsResources(ctx context.Context, getenv func(string) string) Resources {
	id := getenv("PBS_JOBID")
	r := Resources{Source: "PBS job " + id}
	for _, name := range []string{"NCPUS", "PBS_NUM_PPN", "PBS_NP"} {
		if r.CPUs = atoi(getenv(name)); r.CPUs > 0 {
			break
		}
	}
	output, err := schedulerQuery(ctx, "qstat", "-f", id)
	if err != nil {
		return r
	}
	attributes := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		if name, value, found := strings.Cut(scanner.Text(), " = "); found {
			attributes[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	if r.CPUs == 0 {
		r.CPUs = atoi(attributes["Resource_List.ncpus"])
	}
	if mem, err := parsePBSSize(attributes["Resource_List.mem"]); err == nil {
		r.Memory = mem
	}
	if gpus := attributes["Resource_List.ngpus"]; gpus != "" && gpus != "0" {
		r.GPUs = gpus
	}
	walltime, ok := parseClock(attributes["Resource_List.walltime"])
	start := attributes["stime"] // PBS Pro, OpenPBS
	if start == "" {
		start = attributes["start_time"] // Torque
	}
	if began, err := time.ParseInLocation(time.ANSIC, start, time.Local); ok && err == nil {
		r.End = began.Add(walltime)
	}
	return r
}

// parsePBSSize reads PBS sizes such as 32gb or 4000mb.
func parsePBSSize(text string) (int64, error) {
	text = strings.TrimSuffix(strings.ToLower(text), "w")
	return ParseMemory(strings.TrimSuffix(text, "b"))
}

// condorResources reads the slot's machine ad, or failing that the job's
// requests (RequestMemory is in megabytes).
func condorResources(getenv func(string) string) Resources {
	r := Resources{Source: "HTCondor job"}
	if ad := readClassAd(getenv("_CONDOR_MACHINE_AD")); ad != nil {
		r.CPUs = atoi(ad["Cpus"])
		r.Memory = int64(atoi(ad["Memory"])) << 20
		if gpus := ad["Gpus"]; gpus != "" && gpus != "0" {
			r.GPUs = gpus
		}
	}
	if ad := readClassAd(getenv("_CONDOR_JOB_AD")); ad != nil {
		if r.CPUs == 0 {
			r.CPUs = atoi(ad["RequestCpus"])
		}
		if r.Memory == 0 {
			r.Memory = int64(atoi(ad["RequestMemory"])) << 20
		}
		if gpus := ad["RequestGpus"]; r.GPUs == "" && gpus != "" && gpus != "0" {
			r.GPUs = gpus
		}
	}
	return r
}

// readClassAd reads the NAME = VALUE lines of an HTCondor ad file.
func readClassAd(path string) map[string]string {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	ad := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, found := strings.Cut(line, " = "); found {
			ad[strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ad
}

// atoi reads a leading integer, as in Slurm's "8(x2)"; 0 when there is none.
func atoi(text string) int {
	end := 0
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	number, _ := strconv.Atoi(text[:end])
	return number
}

// countList counts a comma-separated device list such as "0,1".
func countList(list string) string {
	if list == "" || list == "NoDevFiles" {
		return ""
	}
	return strconv.Itoa(len(strings.Split(list, ",")))
}
