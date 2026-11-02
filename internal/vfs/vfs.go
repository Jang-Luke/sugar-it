// Package vfs defines the filesystem contract shared by local shares and peers.
package vfs

import (
	"context"
	"io"
	"time"
)

type EntryType string

const (
	TypeFile    EntryType = "file"
	TypeDir     EntryType = "dir"
	TypeSymlink EntryType = "symlink"
	TypeOther   EntryType = "other"
)

type Entry struct {
	Name    string
	Path    string
	Type    EntryType
	Size    int64
	ModTime time.Time
	Mode    uint32
	ETag    string
}

type PutOptions struct {
	Overwrite bool
	ModTime   time.Time
	SHA256    string
}

type FS interface {
	List(ctx context.Context, p string) ([]Entry, error)
	Stat(ctx context.Context, p string) (Entry, error)
	Open(ctx context.Context, p string, offset int64, etag string) (io.ReadCloser, Entry, error)
	Hash(ctx context.Context, p string) (string, error)
	Put(ctx context.Context, p string, r io.Reader, o PutOptions) (Entry, error)
	Mkdir(ctx context.Context, p string) error
	Remove(ctx context.Context, p string, recursive bool) error
	Rename(ctx context.Context, from, to string) error
}
