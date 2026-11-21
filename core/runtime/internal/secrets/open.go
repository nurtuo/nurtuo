// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import "fmt"

// Provider names of Config.Provider.
const (
	ProviderEnv           = "env"
	ProviderDir           = "dir"
	ProviderEncryptedFile = "encrypted-file"
)

// Config selects and configures a provider (runtime config key `secrets`).
type Config struct {
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"` // env (default) | dir | encrypted-file
	Path     string `yaml:"path,omitempty" json:"path,omitempty"`         // directory for dir / encrypted-file
	KeyFile  string `yaml:"key_file,omitempty" json:"key_file,omitempty"` // encrypted-file master key; default <path>/master.key
}

// Validate checks the provider name and its required fields.
func (c Config) Validate() error {
	switch c.Provider {
	case "", ProviderEnv:
		return nil
	case ProviderDir, ProviderEncryptedFile:
		if c.Path == "" {
			return fmt.Errorf("secrets: provider %q requires path", c.Provider)
		}
		return nil
	}
	return fmt.Errorf("secrets: unknown provider %q (want env, dir or encrypted-file)", c.Provider)
}

// Open returns the configured store. The env provider is read-only (Set/Delete fail).
func Open(c Config) (Store, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	switch c.Provider {
	case ProviderDir:
		return NewDir(c.Path), nil
	case ProviderEncryptedFile:
		return NewEncrypted(c.Path, c.KeyFile), nil
	}
	return NewEnv(), nil
}
