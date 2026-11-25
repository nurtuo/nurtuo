// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nurtuo/nurtuo/core/deploy/bundle"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FetchFunc streams an artifact, stopping before more than limit bytes reach the writer.
type FetchFunc func(ctx context.Context, digest string, w io.Writer, limit int64) (int64, error)

// FetchingBundleSource verifies cached bundles and fetches a replacement when needed.
// The same verifier checks staged downloads before they become visible under their digest.
func FetchingBundleSource(dir string, fetch FetchFunc, keys []ed25519.PublicKey) BundleSource {
	return func(ctx context.Context, digest string) ([]byte, error) {
		name, err := signedBundleName(digest)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, name+".bundle")
		if content, err := readSignedBundle(path, digest, name, keys); err == nil {
			return content, nil
		}

		tmp, err := os.CreateTemp(dir, ".fetch-*")
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.Remove(tmp.Name()) }()
		defer func() { _ = tmp.Close() }()

		hash := sha256.New()
		n, err := fetch(ctx, digest, io.MultiWriter(tmp, hash), bundle.MaxBundleSize)
		if err != nil {
			return nil, fetchArtifactError(name, err)
		}
		if n > bundle.MaxBundleSize {
			return nil, fmt.Errorf("artifact larger than %d bytes", bundle.MaxBundleSize)
		}
		if !matchesBundleDigest(hash.Sum(nil), name) {
			return nil, errors.New("fetched artifact does not match digest")
		}
		if err := tmp.Sync(); err != nil {
			return nil, err
		}
		if err := tmp.Close(); err != nil {
			return nil, err
		}
		content, err := readSignedBundle(tmp.Name(), digest, name, keys)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			return nil, err
		}
		return content, nil
	}
}

func fetchArtifactError(name string, err error) error {
	st := status.Convert(err)
	switch st.Code() {
	case codes.Unimplemented:
		return fmt.Errorf("control plane does not implement FetchArtifact; place %s.bundle in bundle_dir", name)
	case codes.NotFound:
		return fmt.Errorf("artifact not available from the control plane for this runtime: %s", st.Message())
	case codes.FailedPrecondition:
		return fmt.Errorf("control plane cannot serve artifact: %s", st.Message())
	default:
		return fmt.Errorf("fetch artifact: %w", err)
	}
}

func cleanArtifactTemps(dir string, log *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warn("read bundle cache for cleanup", "err", err)
		}
		return
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), ".fetch-") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Warn("remove stale artifact download", "err", err)
			}
		}
	}
}
