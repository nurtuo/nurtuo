// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/natsjs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/supervisor"
)

// prepareStateDir creates the state directory (private: 0700) and returns its absolute path.
// A failure is an error, never a fallback (D3).
func prepareStateDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("state_dir %q: %w", dir, err)
	}
	for _, d := range []string{abs, filepath.Join(abs, "jetstream")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", fmt.Errorf("state_dir: cannot create %s: %w", d, err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return "", fmt.Errorf("state_dir: cannot make %s private: %w", d, err)
		}
	}
	return abs, nil
}

// lockStateDir takes an exclusive lock on <stateDir>/lock so one process owns the directory (the
// embedded JetStream server does not lock its store itself). The lock is released when the
// returned function is called or the process exits.
func lockStateDir(stateDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(stateDir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state_dir %s: open lock file: %w", stateDir, err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("state_dir %s is in use by another runtime process: %w", stateDir, err)
	}
	return func() { _ = f.Close() }, nil
}

// openPersistentQueues opens the embedded JetStream backend with its data under
// <stateDir>/jetstream.
func openPersistentQueues(stateDir string) (*natsjs.Backend, error) {
	qb, err := natsjs.New(natsjs.Options{StoreDir: filepath.Join(stateDir, "jetstream")})
	if err != nil {
		return nil, fmt.Errorf("state_dir %s: open queue backend: %w", stateDir, err)
	}
	return qb, nil
}

// newSupervisor returns a supervisor whose applied revision is persisted in <stateDir>/revision
// when stateDir is set (a corrupt file is an error, D4), or an in-memory one otherwise.
func newSupervisor(dep supervisor.Deployer, stateDir string) (*supervisor.Supervisor, error) {
	if stateDir == "" {
		return supervisor.New(dep), nil
	}
	return supervisor.NewPersistent(dep, filepath.Join(stateDir, supervisor.RevisionFile))
}

func warnNoStateDir(log *slog.Logger) {
	log.Warn("state_dir is not set; queued messages and the applied revision do not survive a runtime restart")
}
