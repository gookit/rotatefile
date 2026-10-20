package rotatefile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gookit/goutil/fsutil"
	"github.com/gookit/goutil/mathutil"
	"github.com/gookit/goutil/x/assert"
	"github.com/gookit/rotatefile"
)

// readAll concatenates every file of dir, so a test can count lines no matter
// which file a rotation moved them into.
func readAll(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	assert.NoErr(t, err)
	var b strings.Builder
	for _, f := range files {
		b.WriteString(fsutil.ReadString(f))
	}
	return b.String()
}

// ModeCreate with a time rotation must open the file of the new period; it used
// to reopen the old path, so every period kept writing into the first file.
func TestWriter_modeCreate_rotateByTime_opensNewFile(t *testing.T) {
	dir := t.TempDir()
	logfile := filepath.Join(dir, "app.log")
	mt := rotatefile.NewMockClock("2026-10-20 23:59:50")
	w, err := rotatefile.NewWriterWith(func(c *rotatefile.Config) {
		c.Filepath = logfile
		c.TimeClock = mt
		c.RotateMode = rotatefile.ModeCreate
		c.RotateTime = rotatefile.EveryDay
		c.MaxSize = 0
	})
	assert.NoErr(t, err)
	defer w.MustClose()

	_, err = w.WriteString("day one\n")
	assert.NoErr(t, err)
	mt.Add(20 * time.Second) // 2026-10-21 00:00:10
	_, err = w.WriteString("first of day two\n")
	assert.NoErr(t, err)
	_, err = w.WriteString("second of day two\n")
	assert.NoErr(t, err)

	day1 := fsutil.ReadString(filepath.Join(dir, "app.20261020.log"))
	day2 := fsutil.ReadString(filepath.Join(dir, "app.20261021.log"))
	assert.StrContains(t, day1, "day one")
	assert.NotContains(t, day1, "second of day two")
	assert.StrContains(t, day2, "second of day two")
}

// A writer that opens an existing file counts its size, so a restarted process
// rotates once the file (not only its own writes) reaches MaxSize.
func TestWriter_openExisting_countsFileSize(t *testing.T) {
	dir := t.TempDir()
	logfile := filepath.Join(dir, "app.log")
	assert.NoErr(t, os.WriteFile(logfile, []byte(strings.Repeat("x", 200)+"\n"), 0o644))

	w, err := rotatefile.NewWriterWith(func(c *rotatefile.Config) {
		c.Filepath = logfile
		c.MaxSize = 128
	})
	assert.NoErr(t, err)
	defer w.MustClose()

	_, err = w.WriteString("after restart\n")
	assert.NoErr(t, err)
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	assert.NoErr(t, err)
	assert.Len(t, files, 2, "the oversized file is rotated on the first write")
	assert.NotContains(t, fsutil.ReadString(logfile), "xxxx")
}

// Two writers sharing one file (two processes): when one has rotated the file,
// the other must follow to the new file instead of renaming it away again, and
// no line may be lost.
func TestWriter_sharedFile_followsOtherRotation(t *testing.T) {
	dir := t.TempDir()
	logfile := filepath.Join(dir, "app.log")
	newW := func() *rotatefile.Writer {
		w, err := rotatefile.NewWriterWith(func(c *rotatefile.Config) {
			c.Filepath = logfile
			c.MaxSize = 256
		})
		assert.NoErr(t, err)
		return w
	}
	a, b := newW(), newW()

	const lines = 60
	for i := 0; i < lines; i++ {
		w := a
		if i%2 == 1 {
			w = b
		}
		_, err := w.WriteString("line-" + mathutil.String(i) + " " + strings.Repeat("y", 20) + "\n")
		assert.NoErr(t, err)
	}
	assert.NoErr(t, a.Close())
	assert.NoErr(t, b.Close())

	all := readAll(t, dir)
	for i := 0; i < lines; i++ {
		assert.StrContains(t, all, "line-"+mathutil.String(i)+" ")
	}
	// The shared size counts: with both writers' lines the files stay near MaxSize
	// (one writer's own counter alone would let a file grow to twice the limit).
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	assert.NoErr(t, err)
	for _, f := range files {
		fi, err := os.Stat(f)
		assert.NoErr(t, err)
		assert.True(t, fi.Size() < 256+2*64, f+" grew to "+mathutil.String(fi.Size()))
	}
}

// When the file disappeared under the writer (rotated or removed by another
// process), the failed rename must not leave the writer closed: later lines
// still land in a file.
func TestWriter_renameFails_keepsWriting(t *testing.T) {
	dir := t.TempDir()
	logfile := filepath.Join(dir, "app.log")
	w, err := rotatefile.NewWriterWith(func(c *rotatefile.Config) {
		c.Filepath = logfile
		c.MaxSize = 64
	})
	assert.NoErr(t, err)
	defer w.MustClose()

	_, err = w.WriteString("before\n")
	assert.NoErr(t, err)
	assert.NoErr(t, os.Remove(logfile))

	_, err = w.WriteString(strings.Repeat("z", 80) + "\n") // reaches MaxSize: rotation
	assert.NoErr(t, err)
	_, err = w.WriteString("after the failed rename\n")
	assert.NoErr(t, err)
	assert.StrContains(t, readAll(t, dir), "after the failed rename")
}
