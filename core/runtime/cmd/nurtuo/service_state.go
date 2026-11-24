// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/nurtuo/nurtuo/core/runtime/internal/runtime"
)

// defaultServiceStateDir is the OS data root used as state_dir by `service install`:
// ProgramData\Nurtuo, /Library/Application Support/Nurtuo, /var/lib/nurtuo.
func defaultServiceStateDir(goos string, getenv func(string) string) string {
	switch goos {
	case "windows":
		pd := getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return pd + `\Nurtuo`
	case "darwin":
		return "/Library/Application Support/Nurtuo"
	default:
		return "/var/lib/nurtuo"
	}
}

// ensureStateDirConfigured adds state_dir to the config file at path unless alreadySet. An
// existing value is never changed. It reports whether the file was modified.
func ensureStateDirConfigured(path, stateDir string, alreadySet bool) (bool, error) {
	if alreadySet {
		return false, nil
	}
	ext := filepath.Ext(path)
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var out []byte
	if strings.ToLower(ext) == ".json" {
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(b, &m); err != nil {
			return false, err
		}
		if m == nil {
			return false, fmt.Errorf("runtime config must be a JSON object")
		}
		if m["state_dir"], err = json.Marshal(stateDir); err != nil {
			return false, err
		}
		if out, err = json.MarshalIndent(m, "", "  "); err != nil {
			return false, err
		}
		out = append(out, '\n')
	} else {
		var doc yaml.Node
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return false, err
		}
		if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
			return false, fmt.Errorf("runtime config must be a YAML mapping")
		}
		mapping := doc.Content[0]
		value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: stateDir}
		found := false
		for i := 0; i < len(mapping.Content); i += 2 {
			if key := mapping.Content[i]; key.ShortTag() == "!!str" && key.Value == "state_dir" {
				old := mapping.Content[i+1]
				value.HeadComment, value.LineComment, value.FootComment = old.HeadComment, old.LineComment, old.FootComment
				mapping.Content[i+1] = value
				found = true
				break
			}
		}
		if !found {
			key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "state_dir"}
			mapping.Content = append(mapping.Content, key, value)
		}
		if out, err = yaml.Marshal(&doc); err != nil {
			return false, err
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	// Keep the config extension for validation and stage in the same directory for rename.
	f, err := os.CreateTemp(filepath.Dir(path), ".runtime-*"+ext)
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(out); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if _, err := runtime.LoadConfig(f.Name()); err != nil {
		return false, fmt.Errorf("validate config with state_dir: %w", err)
	}
	if err := os.Chmod(f.Name(), fi.Mode().Perm()); err != nil {
		return false, err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return false, fmt.Errorf("write state_dir to %s: %w", path, err)
	}
	return true, nil
}
