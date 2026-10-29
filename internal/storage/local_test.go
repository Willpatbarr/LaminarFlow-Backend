package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutOpenDelete(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	n, err := s.Put(ctx, "0b7d4a3e-6d5f-4c1e-9a53-2f8d6b1e4712", strings.NewReader("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Put = %d, %v", n, err)
	}

	rc, err := s.Open(ctx, "0b7d4a3e-6d5f-4c1e-9a53-2f8d6b1e4712")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello" {
		t.Errorf("read %q", got)
	}

	// Sharded under the root, and no scratch file left behind.
	if _, err := os.Stat(filepath.Join(root, "0b", "0b7d4a3e-6d5f-4c1e-9a53-2f8d6b1e4712")); err != nil {
		t.Errorf("not at the expected path: %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(root, tmpDir)); len(left) != 0 {
		t.Errorf("scratch directory not empty: %v", left)
	}

	if err := s.Delete(ctx, "0b7d4a3e-6d5f-4c1e-9a53-2f8d6b1e4712"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, "0b7d4a3e-6d5f-4c1e-9a53-2f8d6b1e4712"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open after delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "0b7d4a3e-6d5f-4c1e-9a53-2f8d6b1e4712"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestAKeyThatIsNotAPlainTokenIsRefused(t *testing.T) {
	s, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"", "../escape", "a/b", `a\b`, "..", ".hidden", "/abs", "a\x00b", strings.Repeat("a", 200)} {
		if _, err := s.Put(context.Background(), key, strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Put(%q) = %v, want ErrInvalidKey", key, err)
		}
		if _, err := s.Open(context.Background(), key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Open(%q) = %v, want ErrInvalidKey", key, err)
		}
		if err := s.Delete(context.Background(), key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Delete(%q) = %v, want ErrInvalidKey", key, err)
		}
	}
}

type failingReader struct{ sent bool }

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		return copy(p, bytes.Repeat([]byte("x"), 10)), nil
	}

	return 0, errors.New("boom")
}

func TestAFailedPutLeavesNothing(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Put(context.Background(), "k1", &failingReader{}); err == nil {
		t.Fatal("Put succeeded")
	}
	if _, err := s.Open(context.Background(), "k1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open = %v, want ErrNotFound", err)
	}
	var files []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}

		return nil
	})
	if len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
}

func TestACancelledPutStops(t *testing.T) {
	s, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Put(ctx, "k2", strings.NewReader("data")); !errors.Is(err, context.Canceled) {
		t.Errorf("Put = %v, want context.Canceled", err)
	}
}

func TestNewLocalRefusesAnUnwritableRoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(file, nil, 0o600)

	if _, err := NewLocal(filepath.Join(file, "sub")); err == nil {
		t.Error("NewLocal under a regular file succeeded")
	}
}
