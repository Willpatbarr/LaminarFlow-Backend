// Package storage keeps the bytes of uploaded files, behind an interface small
// enough that an S3-style backend is a second file and not a refactor.
//
// Storage knows nothing about attachments, filenames, content types or who may
// read what: those are rows in Postgres (internal/attachment). It deals only in
// opaque keys, which the caller generates, so no user-controlled string ever
// reaches a backend as a path.
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound: no object is stored under the key. Open returns it; Delete does
// too, so a caller can tell "already gone" from a failure and carry on.
var ErrNotFound = errors.New("storage: not found")

// ErrInvalidKey: the key is not one this backend will map to a location. The
// local backend refuses anything that is not a plain token, which is what keeps
// a key such as "../../etc/passwd" from ever being a path.
var ErrInvalidKey = errors.New("storage: invalid key")

// Storage is the whole contract a backend implements.
type Storage interface {
	// Put stores everything r yields under key and returns how many bytes that
	// was. It is atomic: after an error, nothing is stored under the key and
	// nothing partial is left behind. An existing key is replaced.
	Put(ctx context.Context, key string, r io.Reader) (size int64, err error)

	// Open returns the object's bytes. The caller closes it. ErrNotFound when
	// there is no such object.
	Open(ctx context.Context, key string) (io.ReadCloser, error)

	// Delete removes the object. ErrNotFound when there was none.
	Delete(ctx context.Context, key string) error
}
