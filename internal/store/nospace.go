package store

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
)

// ErrNoSpace is a write that failed because a node is out of room. Many nodes cannot say beforehand how much room they have (FTP, most WebDAV servers), so the module cannot refuse an upload
// early with the figures (docs/storage-module.md, "Room in the store"); it can still say, in words, what happened, instead of a bare "store_error".
type ErrNoSpace struct {
	Store  string
	Nodes  []string // labels of the nodes that said so
	Detail string   // what the node answered
}

func (e *ErrNoSpace) Error() string {
	return fmt.Sprintf("the store %s could not take the block: %s reports that it is out of space (%s); this kind of node cannot say beforehand how much room it has, so the upload was refused only when it "+
		"did not fit; delete caches or artifacts, shorten their retention (cache.retention, artifacts.retention), or give the node a bigger volume", e.Store, strings.Join(e.Nodes, ", "), e.Detail)
}

// noSpaceWords are what servers say when they have no room: the text of ENOSPC and EDQUOT, FTP 552 and 452, WebDAV 507 Insufficient Storage, the quota messages of SFTP servers and of S3 stores.
var noSpaceWords = []string{"no space left", "disk full", "device full", "insufficient storage", "insufficient space", "not enough space", "not enough storage", "out of space", "out of disk",
	"quota exceeded", "quota is exceeded", "exceeded storage allocation", "storage allocation exceeded", "disk quota", "507 ", "552 ", "452 ", "status code 507"}

// IsNoSpace says whether an error from a node means that the node has no room.
func IsNoSpace(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return true
	}
	low := strings.ToLower(err.Error())
	for _, w := range noSpaceWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// Label is a short name of a node for a person: the backend and the path, never an option (an option may be a secret).
func (n *Node) Label() string { return n.Spec.Backend + ":" + n.Spec.Path }
