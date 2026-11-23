package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// resolved is a folder with its links resolved, as the project code keeps
// paths (/var is /private/var on macOS).
func resolved(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The config's workspace_dir chooses for new projects only, and the choice
// is saved, so a project keeps its workspace whatever the config says later.
func TestTheConfigsWorkspaceDirIsForNewProjects(t *testing.T) {
	dir := t.TempDir()
	workspaces := filepath.Join(dir, "workspaces")
	other := filepath.Join(dir, "other")

	fresh, err := PrepareState(filepath.Join(dir, "projects", "fresh"))
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := ChooseWorkspace(fresh, true, workspaces, "")
	if err != nil || chosen != workspaces {
		t.Fatalf("new project: %q %v", chosen, err)
	}
	if saved, _ := SavedWorkspaceDir(fresh); saved != workspaces {
		t.Fatalf("not saved: %q", saved)
	}
	if chosen, _ := ChooseWorkspace(fresh, false, other, ""); chosen != workspaces {
		t.Fatalf("a changed config moved the workspace to %q", chosen)
	}

	old, err := PrepareState(filepath.Join(dir, "projects", "old"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(old, "workspace", "analysis.R"), "x")
	if chosen, err := ChooseWorkspace(old, false, workspaces, ""); err != nil || chosen != "" {
		t.Fatalf("existing project: %q %v", chosen, err)
	}
	if _, err := os.Stat(SettingsFile(old)); !os.IsNotExist(err) {
		t.Fatalf("settings written for a project that keeps its workspace: %v", err)
	}
}

func TestWorkspaceDirFlagCannotHideAWorkspace(t *testing.T) {
	dir := t.TempDir()
	workspaces := filepath.Join(dir, "workspaces")
	state, err := PrepareState(filepath.Join(dir, "projects", "study"))
	if err != nil {
		t.Fatal(err)
	}
	// Still empty: the flag chooses.
	if chosen, err := ChooseWorkspace(state, false, "", workspaces); err != nil || chosen != workspaces {
		t.Fatalf("empty workspace: %q %v", chosen, err)
	}
	if _, err := PrepareWorkspace(WorkspaceFolder(workspaces, state), state); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(WorkspaceFolder(workspaces, state), "data.csv"), "1")
	if chosen, err := ChooseWorkspace(state, false, "", workspaces); err != nil || chosen != workspaces {
		t.Fatalf("the saved folder again: %q %v", chosen, err)
	}
	_, err = ChooseWorkspace(state, false, "", filepath.Join(dir, "elsewhere"))
	if err == nil || !strings.Contains(err.Error(), "move-workspace") {
		t.Fatalf("moved a full workspace by flag: %v", err)
	}
}

func TestMovingAWorkspaceOutAndBack(t *testing.T) {
	project, _, _ := testProject(t)
	before, _ := ReadSettings(project.Settings())
	writeFile(t, filepath.Join(project.State, "workspace", "results", "table.csv"), "a,b")
	workspaces := filepath.Join(resolved(t, t.TempDir()), "workspaces")

	moved, err := MoveWorkspace(context.Background(), project, workspaces, nil)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Workspace != WorkspaceFolder(workspaces, project.State) {
		t.Fatalf("workspace %q", moved.Workspace)
	}
	if readFile(t, filepath.Join(moved.Workspace, "results", "table.csv")) != "a,b" {
		t.Fatal("the files did not move")
	}
	// The mount point stays, empty.
	if !emptyFolder(filepath.Join(project.State, "workspace")) || !isDirectory(filepath.Join(project.State, "workspace")) {
		t.Fatal("the project's workspace is not an empty mount point")
	}
	after, err := ReadSettings(project.Settings())
	if err != nil || after.WorkspaceDir != workspaces || after.Key != before.Key || after.Port != before.Port {
		t.Fatalf("settings %+v %v", after, err)
	}
	reopened, err := OpenProject(project.State, "")
	if err != nil || reopened.Workspace != moved.Workspace {
		t.Fatalf("reopened %+v %v", reopened, err)
	}
	if _, err := MoveWorkspace(context.Background(), reopened, workspaces, nil); err == nil {
		t.Fatal("moved the workspace onto itself")
	}

	back, err := MoveWorkspace(context.Background(), reopened, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if back.Workspace != "" || readFile(t, filepath.Join(project.State, "workspace", "results", "table.csv")) != "a,b" {
		t.Fatalf("not back: %+v", back)
	}
	if exists(moved.Workspace) {
		t.Fatal("the workspace folder is still there")
	}
	if saved, _ := SavedWorkspaceDir(project.State); saved != "" {
		t.Fatalf("settings still name %q", saved)
	}
}

func TestAMovingWorkspaceDoesNotOverwrite(t *testing.T) {
	project, _, _ := testProject(t)
	workspaces := t.TempDir()
	writeFile(t, filepath.Join(WorkspaceFolder(workspaces, project.State), "theirs.txt"), "x")
	if _, err := MoveWorkspace(context.Background(), project, workspaces, nil); err == nil {
		t.Fatal("moved into a folder that has files")
	}
	if _, err := MoveWorkspace(context.Background(), project, filepath.Join(project.State, "inner"), nil); err == nil {
		t.Fatal("moved into the project")
	}
}

// projectWithWorkspace is testProject with its workspace in a workspace
// directory.
func projectWithWorkspace(t *testing.T) (Project, string, string) {
	t.Helper()
	project, projects, _ := testProject(t)
	workspaces := filepath.Join(filepath.Dir(projects), "workspaces")
	writeFile(t, filepath.Join(project.State, "workspace", "notes.md"), "notes")
	moved, err := MoveWorkspace(context.Background(), project, workspaces, nil)
	if err != nil {
		t.Fatal(err)
	}
	return moved, projects, resolved(t, workspaces)
}

func TestProjectsTakeTheirWorkspaceFolderAlong(t *testing.T) {
	project, projects, workspaces := projectWithWorkspace(t)

	copied, err := CopyProject(context.Background(), project, filepath.Join(projects, "copied"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if copied.Workspace != WorkspaceFolder(workspaces, copied.State) || readFile(t, filepath.Join(copied.Workspace, "notes.md")) != "notes" {
		t.Fatalf("copy %+v", copied)
	}
	if _, err := CopyProject(context.Background(), project, filepath.Join(t.TempDir(), "study42"), "", nil); err == nil ||
		!strings.Contains(err.Error(), "would share") {
		t.Fatalf("a same-named copy shares the workspace: %v", err)
	}

	renamed, err := MoveProject(context.Background(), project, filepath.Join(projects, "renamed"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Workspace != WorkspaceFolder(workspaces, renamed.State) || exists(project.Workspace) {
		t.Fatalf("renamed %+v", renamed)
	}
	reopened, err := OpenProject(renamed.State, "")
	if err != nil || reopened.Workspace != renamed.Workspace {
		t.Fatalf("reopened %+v %v", reopened, err)
	}

	// Elsewhere under the same name, the workspace folder stays put.
	elsewhere, err := MoveProject(context.Background(), reopened, filepath.Join(t.TempDir(), "renamed"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if elsewhere.Workspace != renamed.Workspace || readFile(t, filepath.Join(elsewhere.Workspace, "notes.md")) != "notes" {
		t.Fatalf("elsewhere %+v", elsewhere)
	}

	if err := RemoveProject(context.Background(), elsewhere, nil); err != nil {
		t.Fatal(err)
	}
	if exists(elsewhere.Workspace) || exists(RemovingPath(elsewhere.Workspace)) {
		t.Fatal("the workspace folder outlived its project")
	}
}

func TestTheWorkspaceFolderIsMountedAndReported(t *testing.T) {
	binds := ProjectBinds("", "/groups/lab/ws/study", "/home/me/p/study")
	if len(binds) != 1 || binds[0].Target != ContainerWorkspace || binds[0].Source != "/groups/lab/ws/study" || binds[0].ReadOnly {
		t.Fatalf("binds %+v", binds)
	}
	values := WorkerEnvironment(WorkerSettings{HostState: "/home/me/p/study", HostWorkspace: "/groups/lab/ws/study"})
	if values["CODEFLOWER_HOST_WORKSPACE"] != "/groups/lab/ws/study" {
		t.Fatalf("CODEFLOWER_HOST_WORKSPACE %q", values["CODEFLOWER_HOST_WORKSPACE"])
	}
	if _, set := WorkerEnvironment(WorkerSettings{HostState: "/p"})["CODEFLOWER_HOST_WORKSPACE"]; set {
		t.Fatal("set for a workspace in the project")
	}
}
