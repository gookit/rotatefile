//go:build windows

package rotatefile

import (
	"os"
	"path/filepath"
	"syscall"
)

// fileWriteEA is FILE_WRITE_EA, part of what GENERIC_WRITE grants (the syscall
// package does not export it).
const fileWriteEA = 0x00000010

// openAppendFile opens path for appending, creating it (and its directory) when
// missing - like os.OpenFile(path, DefaultFileFlags, perm) with the same access
// rights Go uses for O_APPEND - but also shares delete access.
//
// Go opens files with FILE_SHARE_READ|FILE_SHARE_WRITE only, so while one
// writer holds the log open no other writer (or process) can rename it: every
// size rotation of a file shared by several writers failed with "The process
// cannot access the file because it is being used by another process".
// FILE_SHARE_DELETE lets the rename go through; the writers that still hold the
// old handle keep writing to the renamed file, as on Unix, and switch to the new
// one on their next rotation check (pathMovedAway).
func openAppendFile(path string, perm os.FileMode) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	access := uint32(syscall.FILE_APPEND_DATA | syscall.FILE_WRITE_ATTRIBUTES | fileWriteEA |
		syscall.STANDARD_RIGHTS_WRITE | syscall.SYNCHRONIZE)
	share := uint32(syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE)
	attrs := uint32(syscall.FILE_ATTRIBUTE_NORMAL)
	if perm&0o200 == 0 {
		attrs = syscall.FILE_ATTRIBUTE_READONLY
	}
	h, err := syscall.CreateFile(name, access, share, nil, syscall.OPEN_ALWAYS, attrs, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
