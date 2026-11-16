package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"codeflower-app/internal/sandbox"
)

// workerFlags are the flags of `codeflower sandbox` that say how the worker
// is reached, which a project's settings keep with its folders.
type workerFlags struct {
	listenHost    string
	advertiseHost string
	port          int
	inspectPort   int
	servePage     bool
	noTunnel      bool
	// continueFrom is --continue: a project name or folder.
	continueFrom string
	newKey       bool
}

// projectArgument tells a project name from a project folder: a name has
// no path separator and is a valid project name.
func projectArgument(argument string) (name, folder string) {
	if strings.ContainsAny(argument, `/\`) || strings.HasPrefix(argument, "~") || !validProjectName(argument) {
		return "", argument
	}
	return argument, ""
}

// continueProject points the options at the project to continue and reads
// its saved settings.
func (options *sandboxOptions) continueProject(argument string) (sandbox.Settings, error) {
	name, folder := projectArgument(argument)
	options.project, options.state = name, folder
	if name == "" {
		options.project = "default"
	}
	loaded, err := options.loadConfig()
	if err != nil {
		return sandbox.Settings{}, err
	}
	options.useDirectories(loaded)
	project, err := sandbox.OpenProject(options.stateDirectory(), "")
	if err != nil {
		if name != "" {
			return sandbox.Settings{}, fmt.Errorf("no project %s in %s", name, filepath.Dir(options.stateDirectory()))
		}
		return sandbox.Settings{}, err
	}
	settings, err := sandbox.ReadSettings(project.Settings())
	if errors.Is(err, sandbox.ErrNoSettings) {
		return settings, fmt.Errorf("the project %s has no saved settings (%s): start it once with "+
			"`codeflower sandbox %s …`, which saves them", project.Name, project.Settings(), projectFlag(name, project.State))
	}
	return settings, err
}

// projectFlag is the flag that names a project: --project NAME, or --state
// DIR for one outside the projects directory.
func projectFlag(name, state string) string {
	if name != "" {
		return "--project " + name
	}
	return "--state " + state
}

// applySettings takes a project's saved settings for what no flag gives,
// and adds the folders the flags give to the saved ones.
func (options *sandboxOptions) applySettings(settings sandbox.Settings, flags *workerFlags) {
	given := options.changed
	options.fromSettings = map[string]bool{}
	take := func(name string, apply func()) {
		if !given(name) {
			apply()
			options.fromSettings[name] = true
		}
	}
	if settings.Engine != "" {
		take("engine", func() { options.engine = settings.Engine })
	}
	if !given("image-tag") {
		take("image", func() { options.image, options.imageTag = settings.Image, "" })
	}
	take("isolate-network", func() { options.isolateNetwork = settings.IsolateNetwork })
	take("allowlist", func() { options.allowlist = settings.Allowlist })
	take("env-dir", func() { options.envDir, options.envDirSaved = settings.EnvDir, true })
	take("cpus", func() { options.cpus = settings.CPUs })
	take("memory", func() { options.memory = settings.Memory })
	take("time-limit", func() { options.timeLimit = settings.TimeLimit })
	take("server", func() { flags.servePage = settings.Server })
	if settings.Host != "" {
		take("host", func() { flags.listenHost = settings.Host })
	}
	take("advertise-host", func() { flags.advertiseHost = settings.AdvertiseHost })
	take("port", func() { flags.port = settings.Port })
	take("no-tunnel", func() { flags.noTunnel = settings.NoTunnel })

	options.folders.Inputs = addFolders(settings.Inputs, options.folders.Inputs)
	options.folders.Outputs = addFolders(settings.Outputs, options.folders.Outputs)
	options.folders.Code = addFolders(settings.Code, options.folders.Code)
	options.folders.Skills = addFolders(settings.Skills, options.folders.Skills)
	// Saved mounts first; a given one that is among them is not mounted twice.
	saved := map[string]bool{}
	for _, value := range settings.Binds {
		saved[absoluteBind(value)] = true
	}
	givenBinds := options.bindValues
	options.bindValues = slices.Clone(settings.Binds)
	for _, value := range givenBinds {
		if !saved[absoluteBind(value)] {
			options.bindValues = append(options.bindValues, value)
		}
	}
}

// addFolders are the saved folders, then those given that are not among them.
func addFolders(saved, given []string) []string {
	result := slices.Clone(saved)
	seen := map[string]bool{}
	for _, folder := range saved {
		seen[resolvedPath(folder)] = true
	}
	for _, folder := range given {
		if !seen[resolvedPath(folder)] {
			seen[resolvedPath(folder)] = true
			result = append(result, folder)
		}
	}
	return result
}

// settingsOf fills settings (whose key and certificate it keeps) with how
// this sandbox starts.
func (options sandboxOptions) settingsOf(settings sandbox.Settings, engine string, flags workerFlags) sandbox.Settings {
	settings.Engine = engine
	settings.Image = options.image
	if engine == sandbox.EngineApptainer && options.image != "" && !sandbox.IsPublished(options.image) {
		settings.Image = absolutePath(options.image)
	}
	settings.Inputs = absolutePaths(options.folders.Inputs)
	settings.Outputs = absolutePaths(options.folders.Outputs)
	settings.Code = absolutePaths(options.folders.Code)
	settings.Skills = nil
	for _, folder := range options.folders.Skills {
		// The config's skills come from the config each time.
		if !options.configSkills[resolvedPath(folder)] {
			settings.Skills = append(settings.Skills, absolutePath(folder))
		}
	}
	settings.Binds = nil
	for _, value := range options.bindValues {
		settings.Binds = append(settings.Binds, absoluteBind(value))
	}
	settings.IsolateNetwork = options.isolateNetwork
	settings.Allowlist = ""
	if options.allowlist != "" {
		settings.Allowlist = absolutePath(options.allowlist)
	}
	settings.EnvDir = ""
	if options.envDir != "" {
		settings.EnvDir = absolutePath(options.envDir)
	}
	settings.CPUs, settings.Memory, settings.TimeLimit = options.cpus, options.memory, options.timeLimit
	settings.Server = flags.servePage
	settings.Host = flags.listenHost
	settings.AdvertiseHost = flags.advertiseHost
	settings.Port = flags.port
	settings.NoTunnel = flags.noTunnel
	return settings
}

// absolutePath is a folder as an absolute path, ~ expanded, so the settings
// mean the same from any folder.
func absolutePath(folder string) string {
	if folder == "~" || strings.HasPrefix(folder, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			folder = home + folder[1:]
		}
	}
	if absolute, err := filepath.Abs(folder); err == nil {
		return absolute
	}
	return folder
}

func absolutePaths(folders []string) []string {
	var result []string
	for _, folder := range folders {
		result = append(result, absolutePath(folder))
	}
	return result
}

// absoluteBind is a --bind value with an absolute source, written out in
// full: SOURCE:TARGET:ro|rw.
func absoluteBind(value string) string {
	bind, err := sandbox.ParseBind(value)
	if err != nil {
		return value
	}
	source := bind.Given
	if source == "" {
		source = bind.Source
	}
	mode := "rw"
	if bind.ReadOnly {
		mode = "ro"
	}
	return source + ":" + bind.Target + ":" + mode
}

// projectKey gives settings the project's key and certificate: those saved,
// or new ones the first time or with --new-key.
func projectKey(settings *sandbox.Settings, renew bool) (sandbox.Certificate, error) {
	if renew || settings.Key == "" || settings.Certificate.Certificate == "" {
		if err := settings.Renew(); err != nil {
			return sandbox.Certificate{}, err
		}
	}
	return settings.TLS()
}
