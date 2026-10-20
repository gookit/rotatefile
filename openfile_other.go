//go:build !windows

package rotatefile

import (
	"os"

	"github.com/gookit/goutil/fsutil"
)

// openAppendFile opens path for appending, creating it (and its directory) when
// missing. Unix lets a file be renamed while others hold it open, so the plain
// open flags are enough (see the Windows variant).
func openAppendFile(path string, perm os.FileMode) (*os.File, error) {
	return fsutil.OpenFile(path, DefaultFileFlags, perm)
}
