// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// File names inside the encrypted store's directory.
const (
	EncryptedFileName = "secrets.enc"
	KeyFileName       = "master.key"
)

var encMagic = []byte("NSEC1\n")

// ErrKeychainUnavailable is returned by the OS keychain key source stub.
var ErrKeychainUnavailable = errors.New("OS keychain is not available")

// KeySource provides the 32-byte AES-256 master key of the encrypted store.
type KeySource interface {
	Key() ([]byte, error)
}

// KeychainKeySource is the OS keychain integration point (DPAPI / Keychain / libsecret). It is a
// stub that always reports ErrKeychainUnavailable so ChainKeySource falls back deterministically.
type KeychainKeySource struct{}

func (KeychainKeySource) Key() ([]byte, error) { return nil, ErrKeychainUnavailable }

// FileKeySource keeps the key in a private file (0600 / protected DACL), creating a random one on first use. Use
// ReadOnly to forbid creation (e.g. for `get` on a store that does not exist yet).
type FileKeySource struct {
	Path     string
	ReadOnly bool
}

func (f FileKeySource) Key() ([]byte, error) {
	// Check before reading, so a too-open or planted key file is refused unread.
	if fi, serr := os.Lstat(f.Path); serr == nil {
		if perr := checkPerm(f.Path, fi); perr != nil {
			return nil, perr
		}
	}
	b, err := os.ReadFile(f.Path)
	if err == nil {
		if len(b) != 32 {
			return nil, fmt.Errorf("master key %s: want 32 bytes, got %d", f.Path, len(b))
		}
		return b, nil
	}
	if !errors.Is(err, fs.ErrNotExist) || f.ReadOnly {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	// A missing parent is created private; an existing one (possibly an operator-chosen key_file
	// location) is left unchanged (D9). The key file itself is private from its creation.
	if err := mkdirPrivate(filepath.Dir(f.Path)); err != nil {
		return nil, err
	}
	// Exclusive create: if another process created it meanwhile, use theirs.
	fh, err := createPrivateExclusive(f.Path)
	if errors.Is(err, fs.ErrExist) {
		return FileKeySource{Path: f.Path, ReadOnly: true}.Key()
	}
	if err != nil {
		return nil, err
	}
	if _, err := fh.Write(k); err != nil {
		_ = fh.Close()
		return nil, err
	}
	return k, fh.Close()
}

// ChainKeySource returns the first source that yields a key.
type ChainKeySource []KeySource

func (c ChainKeySource) Key() ([]byte, error) {
	var errs []error
	for _, s := range c {
		k, err := s.Key()
		if err == nil {
			return k, nil
		}
		if !errors.Is(err, ErrKeychainUnavailable) {
			return nil, err // a real failure (bad permissions, wrong size) must not be masked
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// Encrypted is a single-file AES-256-GCM secret store: header "NSEC1\n", 12-byte nonce, then the
// ciphertext of a JSON object name->value. The header is authenticated as additional data and
// every write uses a fresh random nonce and replaces the file atomically.
type Encrypted struct {
	Path string // path of the .enc file
	Keys KeySource

	mu sync.Mutex
}

// NewEncrypted returns a store in dir using the default key chain (keychain stub, then
// <dir>/master.key, or keyFile when set).
func NewEncrypted(dir, keyFile string) *Encrypted {
	if keyFile == "" {
		keyFile = filepath.Join(dir, KeyFileName)
	}
	return &Encrypted{
		Path: filepath.Join(dir, EncryptedFileName),
		Keys: ChainKeySource{KeychainKeySource{}, FileKeySource{Path: keyFile}},
	}
}

func (e *Encrypted) aead() (cipher.AEAD, error) {
	k, err := e.Keys.Key()
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

func (e *Encrypted) load() (map[string]string, error) {
	// Check before reading (the directory too, on Windows), so a too-open store is refused unread.
	if err := checkStoreDir(filepath.Dir(e.Path)); err != nil {
		return nil, err
	}
	if fi, serr := os.Lstat(e.Path); serr == nil {
		if perr := checkPerm(e.Path, fi); perr != nil {
			return nil, perr
		}
	}
	raw, err := os.ReadFile(e.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	g, err := e.aead()
	if err != nil {
		return nil, err
	}
	hl := len(encMagic)
	if len(raw) < hl+g.NonceSize()+g.Overhead() || string(raw[:hl]) != string(encMagic) {
		return nil, errors.New("secret store is corrupt or not a Nurtuo secret store")
	}
	plain, err := g.Open(nil, raw[hl:hl+g.NonceSize()], raw[hl+g.NonceSize():], encMagic)
	if err != nil {
		return nil, errors.New("secret store cannot be decrypted (wrong master key or tampered file)")
	}
	m := map[string]string{}
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, errors.New("secret store is corrupt")
	}
	return m, nil
}

func (e *Encrypted) save(m map[string]string) error {
	g, err := e.aead()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out := append(append([]byte{}, encMagic...), nonce...)
	out = g.Seal(out, nonce, plain, encMagic)
	// Missing directory: created private. Existing: checked, never re-ACLed (D9).
	if err := ensurePrivateDirNoRepair(filepath.Dir(e.Path)); err != nil {
		return err
	}
	return writeFileAtomic(e.Path, out)
}

func (e *Encrypted) Get(_ context.Context, name string) (Secret, error) {
	if err := ValidName(name); err != nil {
		return Secret{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m, err := e.load()
	if err != nil {
		return Secret{}, err
	}
	if v, ok := m[name]; ok && v != "" {
		return New(v), nil
	}
	return Secret{}, notFound(name)
}

func (e *Encrypted) Set(_ context.Context, name string, v Secret) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if v.Reveal() == "" {
		return errors.New("secret value is empty")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m, err := e.load()
	if err != nil {
		return err
	}
	m[name] = v.Reveal()
	return e.save(m)
}

func (e *Encrypted) Delete(_ context.Context, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m, err := e.load()
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return notFound(name)
	}
	delete(m, name)
	return e.save(m)
}

func (e *Encrypted) List(context.Context) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, err := e.load()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return sorted(out), nil
}
