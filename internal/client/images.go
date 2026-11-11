package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"codeflower-app/internal/sandbox"
)

// The page's image manager: the builds of the published sandbox image an
// engine has here, downloading one (with the Claude Code it needs, as
// `codeflower pull` does), and removing them.

// imagePull is a download the page started. It runs on when the page goes,
// and stays listed, finished or failed, until the page dismisses it.
type imagePull struct {
	ID     string `json:"id"`
	Engine string `json:"engine"`
	// Image is the published image's name:tag.
	Image   string    `json:"image"`
	Tag     string    `json:"tag"`
	Started time.Time `json:"started"`

	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	// status: pulling, done, failed or cancelled.
	status string
	err    string
	out    console
}

type imagePullView struct {
	*imagePull
	Status     string                `json:"status"`
	Error      string                `json:"error,omitempty"`
	Log        []string              `json:"log"`
	LastOutput time.Time             `json:"lastOutput,omitzero"`
	Progress   *sandbox.PullProgress `json:"progress,omitempty"`
}

func keep(line string) string { return line }

func (p *imagePull) view() imagePullView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return imagePullView{imagePull: p, Status: p.status, Error: p.err, Log: p.out.view(keep), LastOutput: p.out.last, Progress: p.out.progressView()}
}

func (p *imagePull) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out.write(data, keep)
	return len(data), nil
}

func (p *imagePull) PullProgress(progress sandbox.PullProgress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out.setProgress(progress)
}

func (p *imagePull) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status == "pulling"
}

// localImage is one the engine has, with what the page needs to know of it.
type localImage struct {
	sandbox.LocalImage
	// Default: the build the app runs when no version is chosen.
	Default bool `json:"default,omitempty"`
	// InUse: a sandbox this app started runs it.
	InUse bool `json:"inUse,omitempty"`
}

type imagesInfo struct {
	Engine string `json:"engine"`
	Label  string `json:"label"`
	// Directory is where Apptainer's image files are kept.
	Directory  string          `json:"directory,omitempty"`
	DefaultTag string          `json:"defaultTag"`
	Images     []localImage    `json:"images"`
	Pulls      []imagePullView `json:"pulls"`
	// Error says why the images could not be listed; the downloads still are.
	Error string `json:"error,omitempty"`
}

// imageEngine is the engine a request names (auto: the one sandboxes use),
// or the error to answer.
func (s *Server) imageEngine(r *http.Request, requested string) (sandbox.EngineStatus, error) {
	if s.options.ConnectOnly {
		return sandbox.EngineStatus{}, errors.New("this client only connects to sandboxes running elsewhere")
	}
	if requested == "" {
		requested = s.settings.load().Engine
	}
	return sandbox.ChooseEngine(s.detectEngines(r.Context()), requested, "")
}

func (s *Server) listImages(w http.ResponseWriter, r *http.Request) {
	engine, err := s.imageEngine(r, r.URL.Query().Get("engine"))
	if err != nil {
		writeError(w, http.StatusPreconditionFailed, err)
		return
	}
	info := imagesInfo{Engine: engine.Name, Label: engine.Label, DefaultTag: sandbox.DefaultImageTag, Images: []localImage{}, Pulls: []imagePullView{}}
	if engine.Name == sandbox.EngineApptainer {
		info.Directory = sandbox.ImageDirectory()
	}
	images, err := sandbox.ListImages(r.Context(), engine)
	if err != nil {
		info.Error = err.Error()
	}
	defaultRef, _ := sandbox.ResolveImage("", engine.Name)
	inUse := s.imagesInUse(engine.Name)
	for _, item := range images {
		info.Images = append(info.Images, localImage{LocalImage: item, Default: item.Ref == defaultRef, InUse: inUse[item.Ref]})
	}
	s.mu.Lock()
	pulls := slices.Clone(s.pullOrder)
	s.mu.Unlock()
	for _, pull := range pulls {
		if pull.Engine == engine.Name {
			info.Pulls = append(info.Pulls, pull.view())
		}
	}
	writeJSON(w, http.StatusOK, info)
}

// imagesInUse are the images of the running sandboxes this app started.
func (s *Server) imagesInUse(engine string) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	used := map[string]bool{}
	for _, item := range s.workers {
		item.mu.Lock()
		if item.Source == sourceStarted && item.engineName == engine && (item.status == "starting" || item.status == "ready" || item.status == "stopping") {
			used[item.image] = true
		}
		item.mu.Unlock()
	}
	return used
}

type pullRequest struct {
	Engine string `json:"engine"`
	// ImageTag is a release, latest or a commit's build; empty for the app's.
	ImageTag string `json:"imageTag"`
}

func (s *Server) pullImage(w http.ResponseWriter, r *http.Request) {
	var request pullRequest
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	engine, err := s.imageEngine(r, request.Engine)
	if err != nil {
		writeError(w, http.StatusPreconditionFailed, err)
		return
	}
	name, err := sandbox.PublishedImage(request.ImageTag)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tag := name[len(sandbox.ImageRepository)+1:]

	s.mu.Lock()
	for _, other := range s.pullOrder {
		if other.Engine == engine.Name && other.Image == name && other.running() {
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, other.view())
			return
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	pull := &imagePull{
		ID: sandbox.NewInstanceID()[:8], Engine: engine.Name, Image: name, Tag: tag, Started: time.Now(),
		cancel: cancel, done: make(chan struct{}), status: "pulling",
	}
	if s.pulls == nil {
		s.pulls = map[string]*imagePull{}
	}
	s.pulls[pull.ID] = pull
	s.pullOrder = append(s.pullOrder, pull)
	s.mu.Unlock()
	fmt.Fprintf(s.options.Log, "Downloading the sandbox image %s\n", name)

	go func() {
		defer close(pull.done)
		defer cancel()
		fmt.Fprintf(pull, "Downloading %s with %s…\n", name, engine.Label)
		err := sandbox.PullImage(ctx, engine, name, pull)
		pull.mu.Lock()
		pull.out.progress = nil
		pull.mu.Unlock()
		if err == nil {
			err = pullClaude(ctx, engine, name, pull)
		}
		pull.mu.Lock()
		defer pull.mu.Unlock()
		pull.out.flush(keep)
		switch {
		case ctx.Err() != nil:
			pull.status = "cancelled"
			pull.out.add("Cancelled.")
		case err != nil:
			pull.status, pull.err = "failed", err.Error()
		default:
			pull.status = "done"
			pull.out.add("The sandbox image " + name + " is ready.")
		}
	}()
	writeJSON(w, http.StatusOK, pull.view())
}

// pullClaude downloads the Claude Code the image needs, as `codeflower pull`
// does, so a later start needs no internet for it either.
func pullClaude(ctx context.Context, engine sandbox.EngineStatus, name string, out *imagePull) error {
	resolved, err := sandbox.ResolveImage(name, engine.Name)
	if err != nil {
		return err
	}
	version, err := sandbox.ImageClaudeVersion(ctx, engine, resolved)
	if err != nil || version == "" {
		return err
	}
	directory, err := sandbox.EnsureClaude(ctx, version, out)
	if err != nil {
		return fmt.Errorf("could not download Claude Code %s: %w", version, err)
	}
	fmt.Fprintf(out, "Claude Code %s is ready in %s.\n", version, directory)
	return nil
}

// dismissPull cancels a download that runs, or takes a finished one off the list.
func (s *Server) dismissPull(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	pull := s.pulls[r.PathValue("id")]
	if pull != nil && !pull.running() {
		delete(s.pulls, pull.ID)
		s.pullOrder = slices.DeleteFunc(s.pullOrder, func(other *imagePull) bool { return other == pull })
	}
	s.mu.Unlock()
	if pull == nil {
		writeError(w, http.StatusNotFound, errors.New("no such download"))
		return
	}
	pull.cancel()
	writeJSON(w, http.StatusOK, pull.view())
}

type removeImageRequest struct {
	Engine string `json:"engine"`
	Ref    string `json:"ref"`
}

func (s *Server) removeImage(w http.ResponseWriter, r *http.Request) {
	var request removeImageRequest
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	engine, err := s.imageEngine(r, request.Engine)
	if err != nil {
		writeError(w, http.StatusPreconditionFailed, err)
		return
	}
	if s.imagesInUse(engine.Name)[request.Ref] {
		writeError(w, http.StatusConflict, errors.New("a sandbox started here runs this image: stop it first"))
		return
	}
	if err := sandbox.RemoveImage(r.Context(), engine, request.Ref); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	fmt.Fprintf(s.options.Log, "Removed the sandbox image %s\n", request.Ref)
	writeJSON(w, http.StatusOK, map[string]string{"removed": request.Ref})
}

// stopPulls cancels the downloads and waits for them (at most 10 s).
func (s *Server) stopPulls() {
	s.mu.Lock()
	pulls := slices.Clone(s.pullOrder)
	s.mu.Unlock()
	deadline := time.After(10 * time.Second)
	for _, pull := range pulls {
		pull.cancel()
	}
	for _, pull := range pulls {
		select {
		case <-pull.done:
		case <-deadline:
			return
		}
	}
}
