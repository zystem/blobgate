package store

import (
	"context"
	"io"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
)

// unreachableFs stands for a node that could not be opened (its server was down or silent when the store was loaded: the SFTP backend of rclone connects when it is made). Every call
// answers with the error of the opening, so the node counts as one that does not take writes; the registry opens it again later and replaces the store.
type unreachableFs struct {
	name string
	err  error
}

func (u *unreachableFs) Name() string             { return u.name }
func (u *unreachableFs) Root() string             { return "" }
func (u *unreachableFs) String() string           { return u.name + " (not reachable)" }
func (u *unreachableFs) Precision() time.Duration { return time.Second }
func (u *unreachableFs) Hashes() hash.Set         { return hash.Set(hash.None) }
func (u *unreachableFs) Features() *fs.Features   { return &fs.Features{} }
func (u *unreachableFs) List(context.Context, string) (fs.DirEntries, error) {
	return nil, u.err
}
func (u *unreachableFs) NewObject(context.Context, string) (fs.Object, error) { return nil, u.err }
func (u *unreachableFs) Put(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) (fs.Object, error) {
	return nil, u.err
}
func (u *unreachableFs) Mkdir(context.Context, string) error { return u.err }
func (u *unreachableFs) Rmdir(context.Context, string) error { return u.err }
