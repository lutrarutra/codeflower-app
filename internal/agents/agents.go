// Package agents describes the agents a sandbox can run, from the descriptors
// the sandbox's bridge keeps in dsh-plugins/acp-ws/src/agents/<id>/agent.json.
// descriptors/ holds copies of them, made by `make agents` from the sandbox
// version the app works with, so the app needs no sandbox checkout to build.
package agents

import (
	"cmp"
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
)

//go:embed descriptors/*.json
var files embed.FS

// Descriptor is what the app needs to know about one agent.
type Descriptor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Description says whose service it is and how to sign in, for choosing one.
	Description string `json:"description"`
	// Color is a palette name of the chat (orange, teal, …) or a CSS color.
	Color string `json:"color"`
	Order int    `json:"order"`
	// Home is where the agent keeps its logins and settings in the project:
	// the variable that names it in the sandbox, and its path in the project.
	Home struct {
		Variable string `json:"variable"`
		Path     string `json:"path"`
	} `json:"home"`
	// ProviderHosts are the services the agent signs in to and works with; a
	// network-isolated sandbox can always reach them.
	ProviderHosts []string `json:"providerHosts"`
	// Files are written into the agent's home once, so the user's edits are
	// kept: text as given, anything else as indented JSON.
	Files map[string]json.RawMessage `json:"files"`
}

var all = load()

func load() []Descriptor {
	entries, err := files.ReadDir("descriptors")
	if err != nil {
		panic(err)
	}
	var result []Descriptor
	for _, entry := range entries {
		data, err := files.ReadFile(path.Join("descriptors", entry.Name()))
		if err != nil {
			panic(err)
		}
		var descriptor Descriptor
		if err := json.Unmarshal(data, &descriptor); err != nil {
			panic(fmt.Sprintf("agents/descriptors/%s: %v", entry.Name(), err))
		}
		result = append(result, descriptor)
	}
	// The first is the default where the user does not choose.
	slices.SortStableFunc(result, func(a, b Descriptor) int {
		return cmp.Or(cmp.Compare(a.Order, b.Order), strings.Compare(a.ID, b.ID))
	})
	return result
}

// All are the agents, the default first.
func All() []Descriptor {
	return slices.Clone(all)
}

// IDs are the agents' ids, the default first.
func IDs() []string {
	ids := make([]string, len(all))
	for index, descriptor := range all {
		ids[index] = descriptor.ID
	}
	return ids
}

// Get returns the agent with this id.
func Get(id string) (Descriptor, bool) {
	index := slices.IndexFunc(all, func(descriptor Descriptor) bool { return descriptor.ID == id })
	if index < 0 {
		return Descriptor{}, false
	}
	return all[index], true
}

// Label is the agent's product name, or its id for an unknown agent.
func Label(id string) string {
	if descriptor, ok := Get(id); ok && descriptor.Name != "" {
		return descriptor.Name
	}
	return id
}

// FileContent is a file to write once into the agent's home.
func FileContent(raw json.RawMessage) ([]byte, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []byte(text), nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
