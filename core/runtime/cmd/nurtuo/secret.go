// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/nurtuo/nurtuo/core/runtime/internal/runtime"
	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
)

func init() { register("secret", secretMain) }

const secretUsage = `Usage: nurtuo secret <set|get|list|delete> [flags] [name]

  set <name>      store a secret; the value is read from stdin or a hidden prompt
  get <name>      show whether a secret exists; --reveal prints its value
  list            list secret names (never values)
  delete <name>   remove a secret

Flags (before the name):
  --config <file>     runtime config whose "secrets:" section selects the store
  --provider <name>   env | dir | encrypted-file (overrides --config)
  --path <dir>        directory of the dir / encrypted-file provider
  --key-file <file>   master key file of the encrypted-file provider
  --reveal            (get) print the value to stdout

Without flags the standalone data directory's encrypted store is used. The value is never
accepted as a command-line argument (it would leak into shell history and process listings).
`

// Test seams.
var (
	secretStdin    io.Reader = os.Stdin
	stdinIsTTY               = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	readHiddenLine           = func() (string, error) { b, err := term.ReadPassword(int(os.Stdin.Fd())); return string(b), err }
)

const maxSecretBytes = 1 << 20

func secretMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, secretUsage)
		return 2
	}
	action := args[0]
	fs := flag.NewFlagSet("secret "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var cfgPath, provider, path, keyFile string
	var reveal bool
	fs.StringVar(&cfgPath, "config", "", "")
	fs.StringVar(&provider, "provider", "", "")
	fs.StringVar(&path, "path", "", "")
	fs.StringVar(&keyFile, "key-file", "", "")
	fs.BoolVar(&reveal, "reveal", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		_, _ = fmt.Fprintf(stderr, "nurtuo secret %s: %v\n%s", action, err, secretUsage)
		return 2
	}
	rest := fs.Args()
	wantName := action == "set" || action == "get" || action == "delete"
	switch {
	case action != "set" && action != "get" && action != "list" && action != "delete":
		_, _ = fmt.Fprintf(stderr, "nurtuo secret: unknown action %q\n%s", action, secretUsage)
		return 2
	case wantName && len(rest) != 1, !wantName && len(rest) != 0:
		_, _ = fmt.Fprintf(stderr, "nurtuo secret %s: wrong number of arguments\n%s", action, secretUsage)
		return 2
	}

	cfg, err := secretsConfig(cfgPath, provider, path, keyFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "nurtuo secret: %v\n", err)
		return 1
	}
	store, err := secrets.Open(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "nurtuo secret: %v\n", err)
		return 1
	}
	if err := runSecret(context.Background(), store, cfg, action, rest, reveal, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "nurtuo secret %s: %v\n", action, err)
		return 1
	}
	return 0
}

// secretsConfig picks the store: flags win, then --config, then the standalone data directory.
func secretsConfig(cfgPath, provider, path, keyFile string) (secrets.Config, error) {
	var cfg secrets.Config
	switch {
	case provider != "" || path != "":
		cfg = secrets.Config{Provider: provider, Path: path, KeyFile: keyFile}
		if cfg.Provider == "" {
			cfg.Provider = secrets.ProviderEncryptedFile
		}
	case cfgPath != "":
		c, err := runtime.LoadSecretsConfig(cfgPath)
		if err != nil {
			return cfg, err
		}
		cfg = c
	default:
		root, err := defaultDataRoot()
		if err != nil {
			return cfg, err
		}
		cfg = secrets.Config{Provider: secrets.ProviderEncryptedFile, Path: standaloneSecretStore(root)}
	}
	return cfg, cfg.Validate()
}

// standaloneSecretStore is the user-secret store inside a standalone data root; it is a
// sub-directory of the bootstrap secrets dir (CA, certs, admin token) so the two never mix.
func standaloneSecretStore(root string) string { return filepath.Join(root, "secrets", "store") }

func runSecret(ctx context.Context, st secrets.Store, cfg secrets.Config, action string, args []string, reveal bool, stdout, stderr io.Writer) error {
	switch action {
	case "list":
		names, err := st.List(ctx)
		if err != nil {
			return err
		}
		for _, n := range names {
			_, _ = fmt.Fprintln(stdout, n)
		}
	case "get":
		v, err := st.Get(ctx, args[0])
		if err != nil {
			return err
		}
		if reveal {
			_, _ = fmt.Fprintln(stdout, v.Reveal())
		} else {
			_, _ = fmt.Fprintf(stdout, "%s: set (%d bytes); use --reveal to print the value\n", args[0], len(v.Reveal()))
		}
	case "delete":
		if err := st.Delete(ctx, args[0]); err != nil {
			return readOnlyHint(err, cfg, args[0])
		}
		_, _ = fmt.Fprintf(stdout, "deleted %s\n", args[0])
	case "set":
		if err := secrets.ValidName(args[0]); err != nil {
			return err
		}
		if cfg.Provider == "" || cfg.Provider == secrets.ProviderEnv {
			return readOnlyHint(secrets.ErrReadOnly, cfg, args[0])
		}
		v, err := readSecretValue(args[0], stderr)
		if err != nil {
			return err
		}
		if err := st.Set(ctx, args[0], secrets.New(v)); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "stored %s\n", args[0])
	}
	return nil
}

func readOnlyHint(err error, cfg secrets.Config, name string) error {
	if errors.Is(err, secrets.ErrReadOnly) {
		return fmt.Errorf("the env provider is read-only: export %s in the runtime's environment, or select another provider with --provider/--config", secrets.EnvVar(name))
	}
	return err
}

// readSecretValue reads the value from a hidden prompt (terminal) or from stdin (pipe). One
// trailing newline is trimmed; the value is never echoed or logged.
func readSecretValue(name string, stderr io.Writer) (string, error) {
	var v string
	if stdinIsTTY() {
		_, _ = fmt.Fprintf(stderr, "Value for %s (input hidden): ", name)
		first, err := readHiddenLine()
		_, _ = fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprint(stderr, "Repeat value: ")
		second, err := readHiddenLine()
		_, _ = fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		if first != second {
			return "", errors.New("values do not match")
		}
		v = first
	} else {
		b, err := io.ReadAll(io.LimitReader(bufio.NewReader(secretStdin), maxSecretBytes+1))
		if err != nil {
			return "", err
		}
		if len(b) > maxSecretBytes {
			return "", fmt.Errorf("secret value exceeds %d bytes", maxSecretBytes)
		}
		v = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	if v == "" {
		return "", errors.New("empty value; nothing stored")
	}
	return v, nil
}
