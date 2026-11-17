package sandbox

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

func TestSplitWords(t *testing.T) {
	for _, test := range []struct {
		line string
		want []string
	}{
		{"", nil},
		{"  --gpus all  ", []string{"--gpus", "all"}},
		{`--gpus '"device=0,1"'`, []string{"--gpus", `"device=0,1"`}},
		{`--add-host "my host:1.2.3.4" a\ b`, []string{"--add-host", "my host:1.2.3.4", "a b"}},
		{`--dns=1.1.1.1 ''`, []string{"--dns=1.1.1.1", ""}},
	} {
		got, err := splitWords(test.line)
		if err != nil || !slices.Equal(got, test.want) {
			t.Errorf("splitWords(%q) = %q, %v; want %q", test.line, got, err, test.want)
		}
	}
	for _, line := range []string{`--gpus 'all`, `--gpus "all`} {
		if _, err := splitWords(line); err == nil {
			t.Errorf("splitWords(%q) should fail", line)
		}
	}
}

func TestParseEngineArgs(t *testing.T) {
	args, err := ParseEngineArgs(`--gpus '"device=0,1"' --device /dev/fuse --shm-size=8g --ulimit nofile=1024:2048 ` +
		`--cap-add SYS_PTRACE --cap-drop NET_RAW --add-host db=10.0.0.2 --add-host v6:::1 --dns 1.1.1.1`)
	if err != nil {
		t.Fatal(err)
	}
	want := EngineArgs{GPUs: "0,1", Devices: []string{"/dev/fuse"}, ShmSize: "8g", Ulimits: []string{"nofile=1024:2048"},
		CapAdd: []string{"SYS_PTRACE"}, CapDrop: []string{"NET_RAW"}, AddHosts: []string{"db:10.0.0.2", "v6:::1"}, DNS: []string{"1.1.1.1"}}
	if args.String() != want.String() {
		t.Errorf("got %s\nwant %s", args, want)
	}
	if empty, err := ParseEngineArgs("  "); err != nil || !empty.IsZero() {
		t.Errorf("empty: %v, %v", empty, err)
	}

	for line, message := range map[string]string{
		"-v /tmp:/temp":           "--bind",
		"--volume=/tmp:/temp":     "--bind",
		"-e A=1":                  "--env",
		"-it":                     "-i",
		"--network host":          "--isolate-network",
		"--privileged":            "confinement",
		"--memory 4g":             "--memory",
		"--pids-limit 10":         "unknown flag",
		"all":                     "only flags",
		"--gpus 2":                "all or device=",
		"--shm-size lots":         "--shm-size",
		"--ulimit nofile":         "--ulimit",
		"--device fuse":           "--device",
		"--device /dev/a:/b:xyz":  "--device",
		"--add-host nohost":       "--add-host",
		"--add-host db:not-an-ip": "--add-host",
		"--dns example.org":       "--dns",
	} {
		_, err := ParseEngineArgs(line)
		if err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("ParseEngineArgs(%q) = %v, want an error about %q", line, err, message)
		}
	}
}

func TestEngineArgsForRun(t *testing.T) {
	args := EngineArgs{GPUs: "all", ShmSize: "1g", Devices: []string{"/dev/fuse"}, DNS: []string{"1.1.1.1"}}
	var out bytes.Buffer
	if got := args.forRun(EngineDocker, true, &out); got.String() != args.String() || out.Len() > 0 {
		t.Errorf("docker with a network: %s, %q", got, out.String())
	}
	got := args.forRun(EngineDocker, false, &out)
	if len(got.DNS) != 0 || got.ShmSize != "1g" || !strings.Contains(out.String(), "--dns") {
		t.Errorf("docker without a network: %s, %q", got, out.String())
	}
	out.Reset()
	got = args.forRun(EngineApptainer, true, &out)
	if got.String() != "--gpus all" || !strings.Contains(out.String(), "--device, --shm-size, --dns") {
		t.Errorf("apptainer: %s, %q", got, out.String())
	}
}

func TestEngineArgsDocker(t *testing.T) {
	_, host, err := DockerConfig(DockerRun{Image: "image", State: t.TempDir(), EngineArgs: EngineArgs{
		GPUs: "0,1", Devices: []string{"/dev/fuse:/dev/fuse2:r"}, ShmSize: "1k", Ulimits: []string{"nofile=10:20"},
		CapAdd: []string{"SYS_PTRACE"}, AddHosts: []string{"db:10.0.0.2"}, DNS: []string{"1.1.1.1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(host.DeviceRequests) != 1 || !slices.Equal(host.DeviceRequests[0].DeviceIDs, []string{"0", "1"}) ||
		host.DeviceRequests[0].Capabilities[0][0] != "gpu" {
		t.Errorf("device requests: %+v", host.DeviceRequests)
	}
	if len(host.Devices) != 1 || host.Devices[0] != (container.DeviceMapping{PathOnHost: "/dev/fuse", PathInContainer: "/dev/fuse2", CgroupPermissions: "r"}) {
		t.Errorf("devices: %+v", host.Devices)
	}
	if host.ShmSize != 1024 || len(host.Ulimits) != 1 || host.Ulimits[0].Hard != 20 || host.CapAdd[0] != "SYS_PTRACE" ||
		host.ExtraHosts[0] != "db:10.0.0.2" || host.DNS[0] != "1.1.1.1" {
		t.Errorf("host config: %+v", host)
	}
	_, host, _ = DockerConfig(DockerRun{Image: "image", State: t.TempDir(), EngineArgs: EngineArgs{GPUs: "all"}})
	if host.DeviceRequests[0].Count != -1 {
		t.Errorf("all GPUs: %+v", host.DeviceRequests)
	}
}

func TestEngineArgsPodmanAndApptainer(t *testing.T) {
	args, err := PodmanArgs("podman", PodmanRun{Name: "n", Image: "image", State: t.TempDir(),
		EngineArgs: EngineArgs{GPUs: "0,1", ShmSize: "8g", CapAdd: []string{"SYS_PTRACE"}}})
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(args, " ")
	if !strings.Contains(line, "--device nvidia.com/gpu=0 --device nvidia.com/gpu=1 --shm-size 8g --cap-add SYS_PTRACE") ||
		!strings.HasSuffix(line, " image") {
		t.Errorf("podman: %s", line)
	}

	dir := t.TempDir()
	command, err := ApptainerCommandWith(ApptainerOptions{Executable: "apptainer", Image: "x.sif", State: dir, Scratch: dir, GPUs: true})
	if err != nil || !slices.Contains(command, "--nv") {
		t.Errorf("apptainer: %v, %v", command, err)
	}
	values := map[string]string{}
	EngineArgs{GPUs: "1"}.apptainerGPUs(values)
	if values["CUDA_VISIBLE_DEVICES"] != "1" {
		t.Errorf("apptainer GPU ids: %v", values)
	}
	t.Setenv("CUDA_VISIBLE_DEVICES", "2,3")
	values = map[string]string{}
	EngineArgs{GPUs: "all"}.apptainerGPUs(values)
	if values["CUDA_VISIBLE_DEVICES"] != "2,3" {
		t.Errorf("the job's GPUs: %v", values)
	}
	values = map[string]string{"CUDA_VISIBLE_DEVICES": "0"}
	EngineArgs{GPUs: "all"}.apptainerGPUs(values)
	if values["CUDA_VISIBLE_DEVICES"] != "0" {
		t.Errorf("the user's own: %v", values)
	}
}

func TestAddEnvironment(t *testing.T) {
	t.Setenv("CF_TEST_HERE", "from-host")
	values := map[string]string{"HTTP_PROXY": "proxy"}
	var out bytes.Buffer
	if err := addEnvironment(values, []string{"A=1", "B=", "CF_TEST_HERE", "CF_TEST_UNSET_XYZ"}, &out); err != nil {
		t.Fatal(err)
	}
	if values["A"] != "1" || values["B"] != "" || values["CF_TEST_HERE"] != "from-host" {
		t.Errorf("values: %v", values)
	}
	if _, found := values["CF_TEST_UNSET_XYZ"]; found || !strings.Contains(out.String(), "CF_TEST_UNSET_XYZ") {
		t.Errorf("unset: %v, %q", values, out.String())
	}
	if err := addEnvironment(values, []string{"HTTP_PROXY=x"}, nil); err == nil {
		t.Error("a variable the app sets should be refused")
	}
	for _, value := range []string{"CODEFLOWER_X=1", "1A=2", "A-B", ""} {
		if _, err := ParseEnv(value); err == nil {
			t.Errorf("ParseEnv(%q) should fail", value)
		}
	}
}
