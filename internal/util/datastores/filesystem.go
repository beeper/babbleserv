package datastores

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beeper/babbleserv/internal/config"
)

var ErrPresignedURLUnsupported = errors.New("filesystem datastore does not support presigned URLs")

var _ Datastore = (*FilesystemStore)(nil)

// FilesystemStore is useful for single-node deployments and deterministic
// integration tests. Files become visible with an atomic rename after the full
// request body has been written.
type FilesystemStore struct {
	baseDatastore
	root string
}

func NewFilesystemStore() *FilesystemStore {
	return &FilesystemStore{}
}

func (s *FilesystemStore) Configure(_ config.BabbleConfig, data map[string]any) {
	s.baseDatastore.Configure(data)
	s.root = filepath.Clean(getString(data, "path"))
	if s.root == "." || !filepath.IsAbs(s.root) {
		panic("filesystem datastore path must be absolute")
	}
	if err := os.MkdirAll(s.root, 0o750); err != nil {
		panic(fmt.Errorf("create filesystem datastore: %w", err))
	}
}

func (s *FilesystemStore) objectPath(key string) (string, error) {
	path := filepath.Join(s.root, filepath.FromSlash(key))
	rel, err := filepath.Rel(s.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid datastore object key %q", key)
	}
	return path, nil
}

func (s *FilesystemStore) GetObjectPresignedURL(context.Context, string, time.Duration) (string, error) {
	return "", ErrPresignedURLUnsupported
}

func (s *FilesystemStore) PutObjectPresignedURL(context.Context, string, time.Duration) (string, error) {
	return "", ErrPresignedURLUnsupported
}

func (s *FilesystemStore) GetObject(_ context.Context, key string) (io.Reader, error) {
	path, err := s.objectPath(key)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

func (s *FilesystemStore) GetObjectInfo(_ context.Context, key string) (ObjectInfo, error) {
	path, err := s.objectPath(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Size: info.Size()}, nil
}

func (s *FilesystemStore) PutObject(ctx context.Context, key string, src io.Reader, _ ObjectInfo) error {
	path, err := s.objectPath(key)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err = io.Copy(tmp, &contextReader{ctx: ctx, reader: src}); err != nil {
		tmp.Close()
		return err
	} else if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	} else if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
