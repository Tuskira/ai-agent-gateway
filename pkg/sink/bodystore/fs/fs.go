// Package fs is a sink.BodyStore on the local filesystem: each call's bodies
// become two files under a root directory. It suits single-node deployments
// and a shared volume (NFS/EFS); multi-node fleets without a shared volume
// want pkg/sink/bodystore/s3 instead.
//
// Layout: <root>/<key[0:2]>/<key>.request and <key>.response. The two-char
// fan-out keeps any one directory from holding millions of files. Refs are
// "fs://<key>" -- relative to root, so moving the directory keeps refs valid.
package fs

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

const refScheme = "fs://"

// Store writes bodies under a root directory. It implements sink.BodyStore.
type Store struct {
	root string
}

var _ sink.BodyStore = (*Store)(nil)

// New returns a Store rooted at root, creating the directory if needed.
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("bodystore(fs): root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("bodystore(fs): create root: %w", err)
	}
	return &Store{root: root}, nil
}

// Put writes both bodies (an empty body is an empty file) and returns
// "fs://<key>". Each file is written to a temp file and renamed into place,
// so a reader never sees a partial body; the response is renamed last.
func (s *Store) Put(_ context.Context, key string, req, resp []byte) (string, error) {
	base, err := s.base(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return "", fmt.Errorf("bodystore(fs): create dir: %w", err)
	}
	if err := writeAtomic(base+".request", req); err != nil {
		return "", err
	}
	if err := writeAtomic(base+".response", resp); err != nil {
		return "", err
	}
	return refScheme + key, nil
}

// Get reads both bodies back. A missing file is sink.ErrBodyNotFound.
func (s *Store) Get(_ context.Context, ref string) ([]byte, []byte, error) {
	key, ok := strings.CutPrefix(ref, refScheme)
	if !ok {
		return nil, nil, fmt.Errorf("bodystore(fs): not an fs ref: %q", ref)
	}
	base, err := s.base(key)
	if err != nil {
		return nil, nil, err
	}
	req, err := readBody(base + ".request")
	if err != nil {
		return nil, nil, err
	}
	resp, err := readBody(base + ".response")
	if err != nil {
		return nil, nil, err
	}
	return req, resp, nil
}

// Close is a no-op: the store holds no open handles.
func (s *Store) Close() error { return nil }

// base maps key to <root>/<key[0:2]>/<key>. Keys are gateway-minted request
// ids, but the store is a public seam, so a key that could escape root or
// name a hidden/relative path is rejected rather than sanitized.
func (s *Store) base(key string) (string, error) {
	if len(key) < 2 || strings.ContainsAny(key, `/\`) || strings.HasPrefix(key, ".") || strings.ContainsRune(key, 0) {
		return "", fmt.Errorf("bodystore(fs): invalid key %q", key)
	}
	return filepath.Join(s.root, key[:2], key), nil
}

func writeAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("bodystore(fs): create temp: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("bodystore(fs): write: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("bodystore(fs): close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("bodystore(fs): rename: %w", err)
	}
	return nil
}

func readBody(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, iofs.ErrNotExist) {
		return nil, sink.ErrBodyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("bodystore(fs): read: %w", err)
	}
	return b, nil
}
