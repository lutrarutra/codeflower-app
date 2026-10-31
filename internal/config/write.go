package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// SetTunnel writes `tunnel: {url, eu_only}` into config.yaml, keeping the
// rest of the file, its comments and its permissions as they are. An empty
// url leaves the url as it is.
func SetTunnel(directory, url string, euOnly bool) error {
	return edit(directory, func(root *yaml.Node) {
		section := mappingValue(root, "tunnel")
		if url != "" {
			setScalar(section, "url", url, "!!str")
		}
		value := "false"
		if euOnly {
			value = "true"
		}
		setScalar(section, "eu_only", value, "!!bool")
	})
}

func edit(directory string, change func(root *yaml.Node)) error {
	if directory == "" {
		return errors.New("no config directory")
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	path := filepath.Join(directory, FileName)
	mode := fs.FileMode(0o644)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	var document yaml.Node
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, &document); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if document.Kind == 0 {
		document = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: expected a mapping at the top", path)
	}
	change(root)
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), mode)
}

// mappingValue returns the mapping under key, creating it.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			value := mapping.Content[index+1]
			if value.Kind != yaml.MappingNode {
				*value = yaml.Node{Kind: yaml.MappingNode}
			}
			return value
		}
	}
	value := &yaml.Node{Kind: yaml.MappingNode}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, value)
	return value
}

func setScalar(mapping *yaml.Node, key, value, tag string) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = &yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: tag}
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: tag})
}
