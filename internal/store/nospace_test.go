package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestIsNoSpace(t *testing.T) {
	yes := []error{
		&os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC},
		fmt.Errorf("update stor: %w", &os.PathError{Op: "write", Path: "/x", Err: syscall.EDQUOT}),
		errors.New("update stor: 552 Requested file action aborted. Exceeded storage allocation"),
		errors.New("Put \"http://h/x\": 507 Insufficient Storage"),
		errors.New("sftp: \"Failure\" (SSH_FX_FAILURE): disk quota exceeded"),
		errors.New("write: No space left on device"),
	}
	for _, e := range yes {
		if !IsNoSpace(e) {
			t.Errorf("not recognised: %v", e)
		}
	}
	no := []error{nil, errors.New("connection refused"), errors.New("550 permission denied"), errors.New("timeout"), errors.New("object not found")}
	for _, e := range no {
		if IsNoSpace(e) {
			t.Errorf("taken for no space: %v", e)
		}
	}
}
