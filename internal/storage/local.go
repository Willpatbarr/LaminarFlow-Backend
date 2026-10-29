package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// keyPattern is what the local backend accepts as a key: one token, starting
// with an alphanumeric, with no separators. The service generates uuids; this
// is the backstop for the day something else does not.
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// tmpDir holds uploads while they are being written. Inside the root, so the
// final rename never crosses a filesystem and stays atomic.
const tmpDir = ".tmp"

// Local keeps objects as files under one directory.
//
// Layout: <root>/<first two characters of the key>/<key>. The fan-out keeps a
// directory listing usable once there are thousands of files. Nothing in the
// path comes from a filename.
type Local struct {
	root string
}

// NewLocal creates root (and the scratch directory inside it) if needed and
// checks it can be written to, so a read-only or mis-mounted volume stops the
// server at startup instead of failing the first upload.
func NewLocal(root string) (*Local, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve %q: %w", root, err)
	}
	if err := os.MkdirAll(filepath.Join(abs, tmpDir), 0o750); err != nil {
		return nil, fmt.Errorf("storage: create %q: %w", abs, err)
	}

	probe, err := os.CreateTemp(filepath.Join(abs, tmpDir), "probe-*")
	if err != nil {
		return nil, fmt.Errorf("storage: %q is not writable: %w", abs, err)
	}
	probe.Close()
	os.Remove(probe.Name())

	return &Local{root: abs}, nil
}

// Root is the absolute folder in use, for the startup log.
func (l *Local) Root() string { return l.root }

func (l *Local) path(key string) (string, error) {
	if !keyPattern.MatchString(key) {
		return "", ErrInvalidKey
	}
	// Two characters are always there: the pattern demands at least one, and a
	// one-character key is padded rather than special-cased.
	shard := (key + "_")[:2]

	return filepath.Join(l.root, shard, key), nil
}

func (l *Local) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	dst, err := l.path(key)
	if err != nil {
		return 0, err
	}

	tmp, err := os.CreateTemp(filepath.Join(l.root, tmpDir), "upload-*")
	if err != nil {
		return 0, fmt.Errorf("storage: temp file: %w", err)
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	n, err := io.Copy(tmp, &ctxReader{ctx: ctx, r: r})
	if err != nil {
		// Not wrapped with %w beyond the cause: the caller matches its own
		// sentinel (the size limit) with errors.Is.
		return 0, fmt.Errorf("storage: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return 0, fmt.Errorf("storage: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("storage: close: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, fmt.Errorf("storage: shard directory: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return 0, fmt.Errorf("storage: chmod: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return 0, fmt.Errorf("storage: rename: %w", err)
	}
	done = true

	return n, nil
}

func (l *Local) Open(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: open: %w", err)
	}

	return f, nil
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}

	err = os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("storage: delete: %w", err)
	}

	return nil
}

// ctxReader stops a copy when the request is cancelled, so a client that hangs
// up mid-upload does not leave the server draining it into a temp file.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.r.Read(p)
}
