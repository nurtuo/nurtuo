// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var out []byte
	if strings.ToLower(filepath.Ext(path)) == ".json" {
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			return false, err
		}
		m["state_dir"] = stateDir
		if out, err = json.MarshalIndent(m, "", "  "); err != nil {
			return false, err
		}
		out = append(out, '\n')
	} else {
		q, err := json.Marshal(stateDir) // a JSON string is a valid YAML double-quoted scalar
		if err != nil {
			return false, err
		}
		nl := ""
		if len(b) > 0 && b[len(b)-1] != '\n' {
			nl = "\n"
		}
		out = append(b, []byte(nl+"state_dir: "+string(q)+"\n")...)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(path, out, fi.Mode().Perm()); err != nil {
		return false, fmt.Errorf("write state_dir to %s: %w", path, err)
	}
	return true, nil
}
