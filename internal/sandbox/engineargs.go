package sandbox

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-units"
	"github.com/spf13/pflag"
)

// EngineArgs are the container engine flags a user may add (--engine-args):
// a common set of `docker run` flags, which the app gives Docker through its
// API and Podman and Apptainer as their own flags. Only these are taken, so
// a project means the same on every engine, and none can undo what the app
// sets itself (the name, the network, the user, the mounts' checks).
type EngineArgs struct {
	// GPUs is "all", or the ids of the GPUs to give, comma-separated.
	GPUs     string
	Devices  []string
	ShmSize  string
	Ulimits  []string
	CapAdd   []string
	CapDrop  []string
	AddHosts []string
	DNS      []string
}

// engineArgsSupported lists the flags --engine-args takes, for its errors and help.
const engineArgsSupported = "--gpus, --device, --shm-size, --ulimit, --cap-add, --cap-drop, --add-host, --dns"

// engineArgsRefused are flags of `docker run` the app sets itself or that
// have their own codeflower flag, with what to use instead.
var engineArgsRefused = map[string]string{
	"e": "use codeflower's --env", "env": "use codeflower's --env", "env-file": "use codeflower's --env",
	"v": "use codeflower's --bind", "volume": "use codeflower's --bind", "mount": "use codeflower's --bind",
	"cpus": "use codeflower's --cpus", "m": "use codeflower's --memory", "memory": "use codeflower's --memory",
	"network": "use codeflower's --isolate-network", "net": "use codeflower's --isolate-network",
	"p": "the app publishes the worker's port itself", "publish": "the app publishes the worker's port itself",
	"name": "the app names the container", "rm": "the app removes the container",
	"entrypoint": "the image's entrypoint runs the bridge", "init": "the app runs an init process",
	"i": "the app attaches the terminal itself", "interactive": "the app attaches the terminal itself",
	"t": "the app attaches the terminal itself", "tty": "the app attaches the terminal itself",
	"u": "the sandbox runs as you", "user": "the sandbox runs as you", "userns": "the sandbox runs as you",
	"privileged":   "it would undo the sandbox's confinement",
	"security-opt": "it would undo the sandbox's confinement",
}

// ParseEngineArgs reads --engine-args: flags split as a shell splits them.
func ParseEngineArgs(value string) (EngineArgs, error) {
	var args EngineArgs
	words, err := splitWords(value)
	if err != nil {
		return args, fmt.Errorf("--engine-args: %w", err)
	}
	if len(words) == 0 {
		return args, nil
	}
	for _, word := range words {
		name := strings.TrimLeft(word, "-")
		name, _, _ = strings.Cut(name, "=")
		if !strings.HasPrefix(word, "-") || word == "-" || word == "--" {
			continue
		}
		// -it and the like are short flags run together.
		if !strings.HasPrefix(word, "--") && len(name) > 1 {
			for _, short := range name {
				if reason, refused := engineArgsRefused[string(short)]; refused {
					return args, fmt.Errorf("--engine-args cannot take -%c: %s", short, reason)
				}
			}
			continue
		}
		if reason, refused := engineArgsRefused[name]; refused {
			return args, fmt.Errorf("--engine-args cannot take %s: %s", strings.SplitN(word, "=", 2)[0], reason)
		}
	}
	flags := pflag.NewFlagSet("engine-args", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	flags.StringVar(&args.GPUs, "gpus", "", "")
	flags.StringArrayVar(&args.Devices, "device", nil, "")
	flags.StringVar(&args.ShmSize, "shm-size", "", "")
	flags.StringArrayVar(&args.Ulimits, "ulimit", nil, "")
	flags.StringArrayVar(&args.CapAdd, "cap-add", nil, "")
	flags.StringArrayVar(&args.CapDrop, "cap-drop", nil, "")
	flags.StringArrayVar(&args.AddHosts, "add-host", nil, "")
	flags.StringArrayVar(&args.DNS, "dns", nil, "")
	if err := flags.Parse(words); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			err = errors.New("unknown flag: --help")
		}
		return args, fmt.Errorf("--engine-args: %v (it takes %s)", err, engineArgsSupported)
	}
	if rest := flags.Args(); len(rest) > 0 {
		return args, fmt.Errorf("--engine-args takes only flags, not %q (it takes %s)", rest[0], engineArgsSupported)
	}
	return args, args.check()
}

var gpuIDs = regexp.MustCompile(`^[A-Za-z0-9-]+(,[A-Za-z0-9-]+)*$`)

// check validates each value, and writes GPUs and the hosts as the app uses them.
func (a *EngineArgs) check() error {
	if a.GPUs != "" {
		gpus := strings.Trim(a.GPUs, `"'`)
		switch {
		case gpus == "all":
		case strings.HasPrefix(gpus, "device=") && gpuIDs.MatchString(strings.TrimPrefix(gpus, "device=")):
			gpus = strings.TrimPrefix(gpus, "device=")
		default:
			return fmt.Errorf("--engine-args: --gpus must be all or device=ID[,ID…] (e.g. device=0,1), not %q", a.GPUs)
		}
		a.GPUs = gpus
	}
	for _, device := range a.Devices {
		if _, err := deviceMapping(device); err != nil {
			return err
		}
	}
	if a.ShmSize != "" {
		if _, err := units.RAMInBytes(a.ShmSize); err != nil {
			return fmt.Errorf("--engine-args: --shm-size: %w", err)
		}
	}
	for _, ulimit := range a.Ulimits {
		if _, err := units.ParseUlimit(ulimit); err != nil {
			return fmt.Errorf("--engine-args: --ulimit: %w", err)
		}
	}
	for index, value := range a.AddHosts {
		// HOST=IP, or HOST:IP split at the first colon, as an IPv6 address has more.
		separator := ":"
		if strings.Contains(value, "=") {
			separator = "="
		}
		host, address, found := strings.Cut(value, separator)
		if !found || host == "" || (net.ParseIP(address) == nil && address != "host-gateway") {
			return fmt.Errorf("--engine-args: --add-host must be HOST:IP, not %q", value)
		}
		a.AddHosts[index] = host + ":" + address
	}
	for _, server := range a.DNS {
		if net.ParseIP(server) == nil {
			return fmt.Errorf("--engine-args: --dns must be an IP address, not %q", server)
		}
	}
	return nil
}

// deviceMapping reads HOST[:CONTAINER][:PERMISSIONS] as Docker does.
func deviceMapping(value string) (container.DeviceMapping, error) {
	parts := strings.Split(value, ":")
	mapping := container.DeviceMapping{PathOnHost: parts[0], PathInContainer: parts[0], CgroupPermissions: "rwm"}
	switch len(parts) {
	case 1:
	case 2:
		if strings.HasPrefix(parts[1], "/") {
			mapping.PathInContainer = parts[1]
		} else {
			mapping.CgroupPermissions = parts[1]
		}
	case 3:
		mapping.PathInContainer, mapping.CgroupPermissions = parts[1], parts[2]
	default:
		return mapping, fmt.Errorf("--engine-args: --device must be HOST[:CONTAINER][:PERMISSIONS], not %q", value)
	}
	if !strings.HasPrefix(mapping.PathOnHost, "/") || !strings.HasPrefix(mapping.PathInContainer, "/") ||
		strings.Trim(mapping.CgroupPermissions, "rwm") != "" {
		return mapping, fmt.Errorf("--engine-args: --device must be HOST[:CONTAINER][:PERMISSIONS], not %q", value)
	}
	return mapping, nil
}

// IsZero says whether no flag was given.
func (a EngineArgs) IsZero() bool {
	return a.GPUs == "" && len(a.Devices) == 0 && a.ShmSize == "" && len(a.Ulimits) == 0 &&
		len(a.CapAdd) == 0 && len(a.CapDrop) == 0 && len(a.AddHosts) == 0 && len(a.DNS) == 0
}

// forRun drops what the engine cannot apply, saying so on out, so a
// project's engine arguments still start it on another engine: Apptainer
// takes only --gpus, and a sandbox without a network no hosts or DNS servers.
func (a EngineArgs) forRun(engine string, networked bool, out io.Writer) EngineArgs {
	var skipped []string
	drop := func(flag string, given bool) bool {
		if given {
			skipped = append(skipped, flag)
		}
		return given
	}
	if engine == EngineApptainer {
		if drop("--device", len(a.Devices) > 0) {
			a.Devices = nil
		}
		if drop("--shm-size", a.ShmSize != "") {
			a.ShmSize = ""
		}
		if drop("--ulimit", len(a.Ulimits) > 0) {
			a.Ulimits = nil
		}
		if drop("--cap-add", len(a.CapAdd) > 0) {
			a.CapAdd = nil
		}
		if drop("--cap-drop", len(a.CapDrop) > 0) {
			a.CapDrop = nil
		}
	}
	if engine == EngineApptainer || !networked {
		if drop("--add-host", len(a.AddHosts) > 0) {
			a.AddHosts = nil
		}
		if drop("--dns", len(a.DNS) > 0) {
			a.DNS = nil
		}
	}
	if len(skipped) > 0 && out != nil {
		why := "Apptainer has no such flags"
		if engine != EngineApptainer {
			why = "the sandbox has no network of its own"
		}
		fmt.Fprintf(out, "Not applying %s from --engine-args: %s\n", strings.Join(skipped, ", "), why)
	}
	return a
}

// docker sets the arguments in Docker's API objects.
func (a EngineArgs) docker(host *container.HostConfig) {
	if a.GPUs != "" {
		request := container.DeviceRequest{Count: -1, Capabilities: [][]string{{"gpu"}}}
		if a.GPUs != "all" {
			request.Count, request.DeviceIDs = 0, strings.Split(a.GPUs, ",")
		}
		host.DeviceRequests = append(host.DeviceRequests, request)
	}
	for _, device := range a.Devices {
		if mapping, err := deviceMapping(device); err == nil {
			host.Devices = append(host.Devices, mapping)
		}
	}
	if a.ShmSize != "" {
		host.ShmSize, _ = units.RAMInBytes(a.ShmSize)
	}
	for _, value := range a.Ulimits {
		if ulimit, err := units.ParseUlimit(value); err == nil {
			host.Ulimits = append(host.Ulimits, &container.Ulimit{Name: ulimit.Name, Soft: ulimit.Soft, Hard: ulimit.Hard})
		}
	}
	host.CapAdd = append(host.CapAdd, a.CapAdd...)
	host.CapDrop = append(host.CapDrop, a.CapDrop...)
	host.ExtraHosts = append(host.ExtraHosts, a.AddHosts...)
	host.DNS = append(host.DNS, a.DNS...)
}

// podman is the arguments as `podman run` flags. GPUs are given as CDI
// devices (nvidia-ctk cdi generate), which Podman has taken longer than --gpus.
func (a EngineArgs) podman() []string {
	var args []string
	if a.GPUs == "all" {
		args = append(args, "--device", "nvidia.com/gpu=all")
	} else if a.GPUs != "" {
		for _, id := range strings.Split(a.GPUs, ",") {
			args = append(args, "--device", "nvidia.com/gpu="+id)
		}
	}
	for _, device := range a.Devices {
		args = append(args, "--device", device)
	}
	if a.ShmSize != "" {
		args = append(args, "--shm-size", a.ShmSize)
	}
	for _, pair := range []struct {
		flag   string
		values []string
	}{{"--ulimit", a.Ulimits}, {"--cap-add", a.CapAdd}, {"--cap-drop", a.CapDrop}, {"--add-host", a.AddHosts}, {"--dns", a.DNS}} {
		for _, value := range pair.values {
			args = append(args, pair.flag, value)
		}
	}
	return args
}

// apptainerGPUs sets which GPUs an Apptainer sandbox (--nv, which gives it
// every one) uses: those given, or the batch job's, which --cleanenv would
// otherwise drop.
func (a EngineArgs) apptainerGPUs(values map[string]string) {
	if a.GPUs == "" || values["CUDA_VISIBLE_DEVICES"] != "" {
		return
	}
	if a.GPUs != "all" {
		values["CUDA_VISIBLE_DEVICES"] = a.GPUs
	} else if visible := os.Getenv("CUDA_VISIBLE_DEVICES"); visible != "" {
		values["CUDA_VISIBLE_DEVICES"] = visible
	}
}

// String is the arguments as flags, for the sandbox's summary.
func (a EngineArgs) String() string {
	var words []string
	if a.GPUs == "all" {
		words = append(words, "--gpus all")
	} else if a.GPUs != "" {
		words = append(words, "--gpus device="+a.GPUs)
	}
	for _, pair := range []struct {
		flag   string
		values []string
	}{{"--device", a.Devices}, {"--ulimit", a.Ulimits}, {"--cap-add", a.CapAdd}, {"--cap-drop", a.CapDrop},
		{"--add-host", a.AddHosts}, {"--dns", a.DNS}} {
		for _, value := range pair.values {
			words = append(words, pair.flag+" "+value)
		}
	}
	if a.ShmSize != "" {
		words = append(words, "--shm-size "+a.ShmSize)
	}
	return strings.Join(words, " ")
}

// splitWords splits a command line as a POSIX shell does, without expanding
// anything: words are separated by blanks, and quotes and backslashes keep
// characters together.
func splitWords(value string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	for index := 0; index < len(value); index++ {
		c := value[index]
		switch {
		case c == '\\':
			if index+1 < len(value) {
				index++
				word.WriteByte(value[index])
			}
			inWord = true
		case c == '\'':
			end := strings.IndexByte(value[index+1:], '\'')
			if end < 0 {
				return nil, errors.New("a ' is not closed")
			}
			word.WriteString(value[index+1 : index+1+end])
			index += end + 1
			inWord = true
		case c == '"':
			index++
			for ; index < len(value) && value[index] != '"'; index++ {
				if value[index] == '\\' && index+1 < len(value) && strings.IndexByte(`"\$`+"`", value[index+1]) >= 0 {
					index++
				}
				word.WriteByte(value[index])
			}
			if index >= len(value) {
				return nil, errors.New(`a " is not closed`)
			}
			inWord = true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}

// envName is what a variable's name may be.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseEnv checks an --env value, NAME or NAME=VALUE, and returns its name.
func ParseEnv(value string) (string, error) {
	name, _, _ := strings.Cut(value, "=")
	if !envName.MatchString(name) {
		return "", fmt.Errorf("--env must be NAME or NAME=VALUE, with NAME of letters, digits and _, not %q", value)
	}
	if strings.HasPrefix(name, "CODEFLOWER_") {
		return "", fmt.Errorf("--env cannot set %s: the CODEFLOWER_ variables are the app's", name)
	}
	return name, nil
}

// addEnvironment adds the user's --env variables to the sandbox's: NAME=VALUE
// as given, NAME with its value here (left out when it is not set, as Docker
// does). One the app sets itself is refused.
func addEnvironment(values map[string]string, given []string, out io.Writer) error {
	var unset []string
	for _, value := range given {
		name, err := ParseEnv(value)
		if err != nil {
			return err
		}
		if _, taken := values[name]; taken {
			return fmt.Errorf("--env cannot set %s: the app sets it", name)
		}
		if _, set, found := strings.Cut(value, "="); found {
			values[name] = set
		} else if set, found := os.LookupEnv(name); found {
			values[name] = set
		} else {
			unset = append(unset, name)
		}
	}
	if len(unset) > 0 && out != nil {
		slices.Sort(unset)
		fmt.Fprintf(out, "Not passing %s to the sandbox (--env): not set here\n", strings.Join(unset, ", "))
	}
	return nil
}
