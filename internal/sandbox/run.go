package sandbox

import (
	"os"
	"path/filepath"
)

// RunPromptFile is where the worker finds a run's initial prompt.
func RunPromptFile(id string) string {
	return ContainerState + "/runs/" + id + "/prompt.txt"
}

// WriteRunPrompt stores a run's initial prompt privately in the project.
func WriteRunPrompt(state, id, prompt string) error {
	directory := filepath.Join(state, "runs", id)
	if err := privateDirectory(directory); err != nil {
		return err
	}
	return writeOnce(filepath.Join(directory, "prompt.txt"), []byte(prompt+"\n"))
}

// ReadRunPrompt reads the prompt the worker will start with.
func ReadRunPrompt(state, id string) (string, error) {
	data, err := os.ReadFile(filepath.Join(state, "runs", id, "prompt.txt"))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
