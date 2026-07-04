package ship

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// fileRemote — бэкенд «директория как объектный стор». Для NFS/rclone-mount и
// тестов. Put атомарен (tmp+rename) — читатель никогда не видит полуфайл,
// это важно для MANIFEST.json, который перезаписывается на каждой публикации.
type fileRemote struct {
	root string
}

func newFileRemote(root string) (*fileRemote, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("ship: mkdir %s: %w", root, err)
	}
	return &fileRemote{root: root}, nil
}

func (f *fileRemote) path(key string) string {
	return filepath.Join(f.root, filepath.FromSlash(key))
}

func (f *fileRemote) Put(_ context.Context, key string, data []byte) error {
	p := f.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("ship: mkdir for %s: %w", key, err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("ship: write %s: %w", key, err)
	}
	// fsync файла и каталога: file://-бэкенд — это и есть «удалённая» копия,
	// его durability не слабее локальной (та же дисциплина, что у snapshot.wal).
	if err := syncFile(tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ship: sync %s: %w", key, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ship: rename %s: %w", key, err)
	}
	return syncDir(filepath.Dir(p))
}

func (f *fileRemote) Get(_ context.Context, key string) ([]byte, error) {
	b, err := os.ReadFile(f.path(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return nil, fmt.Errorf("ship: get %s: %w", key, err)
	}
	return b, nil
}

func (f *fileRemote) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	base := f.root
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Гонка с Delete во время обхода — пропускаем исчезнувшее.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasSuffix(p, ".tmp") {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ship: list %s: %w", prefix, err)
	}
	return keys, nil
}

func (f *fileRemote) Delete(_ context.Context, key string) error {
	if err := os.Remove(f.path(key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("ship: delete %s: %w", key, err)
	}
	return nil
}

func syncFile(path string) error {
	fd, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fd.Close()
	return fd.Sync()
}

func syncDir(dir string) error {
	fd, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer fd.Close()
	return fd.Sync()
}
