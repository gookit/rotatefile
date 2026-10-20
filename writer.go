package rotatefile

import (
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gookit/goutil/errorx"
	"github.com/gookit/goutil/fsutil"
	"github.com/gookit/goutil/mathutil"
	"github.com/gookit/goutil/strutil"
	"github.com/gookit/goutil/x/stdio"
	"github.com/gookit/rotatefile/internal"
)

// Writer a flush, close, writer and support rotate file.
//
// refer https://github.com/flike/golog/blob/master/filehandler.go
type Writer struct {
	// writer instance id, use for debug
	id string
	mu sync.RWMutex
	// config of the writer
	cfg *Config

	// current opened logfile
	file *os.File
	// current opened file path. NOTE it maybe not equals Config.Filepath
	path string
	// The original file dir path for the Config.Filepath
	fileDir string
	// The original name and ext information
	fileName, onlyName, fileExt string

	// logfile max backup time. equals Config.BackupTime * time.Hour
	backupDur time.Duration
	// oldFiles []string
	cleanCh chan struct{}
	stopCh  chan struct{}
	// ensure the clean-consumer goroutine is started only once
	cleanOnce sync.Once
	// wait for the clean-consumer goroutine to exit on close
	cleanWg sync.WaitGroup
	// pathMu guards d.path. The async cleaner reads it concurrently with rotation
	// (openFile). A dedicated lock is needed because in CloseLock mode rotation
	// runs under the EXTERNAL logger lock, not d.mu, so d.mu can't synchronize it.
	pathMu sync.RWMutex

	// context use for rotating file by size
	written   uint64 // written size
	rotateNum uint   // rotate times number
	// nextSizeSync is the written size at which the counter is next refreshed
	// from the file itself (see syncWrittenSize).
	nextSizeSync uint64

	// ---- context use for rotating file by time ----

	// the rotating file name suffix format. eg: "20210102", "20210102_1500"
	suffixFormat   string
	checkInterval  int64     // check interval seconds.
	nextRotatingAt time.Time // next rotating time
}

// NewWriter create rotate write with config and init it.
func NewWriter(c *Config) (*Writer, error) {
	d := &Writer{cfg: c}

	if err := d.init(); err != nil {
		return nil, err
	}
	return d, nil
}

// NewWriterWith create a rotated writer with some settings.
func NewWriterWith(fns ...ConfigFn) (*Writer, error) {
	return NewWriter(NewConfigWith(fns...))
}

// init rotate dispatcher
func (d *Writer) init() error {
	d.id = fmt.Sprintf("%p", d)

	logfile := d.cfg.Filepath
	// dirSep := filepath.Separator
	// d.fileDir = filepath.Dir(logfile)
	d.fileDir, d.fileName = filepath.Split(d.cfg.Filepath)
	d.fileExt = filepath.Ext(d.fileName)                   // eg: .log
	d.onlyName = strings.TrimSuffix(d.fileName, d.fileExt) // eg: error
	// removes the trailing separator on the dir path
	if ln := len(d.fileDir); ln > 1 && d.fileDir[ln-1] == filepath.Separator {
		d.fileDir = d.fileDir[:ln-1]
	}

	d.backupDur = d.cfg.backupDuration()
	d.suffixFormat = d.cfg.RotateTime.TimeFormat()
	d.checkInterval = d.cfg.RotateTime.Interval()

	// calc and storage next rotating time
	if d.checkInterval > 0 {
		now := d.cfg.TimeClock.Now()
		// next rotating time
		d.nextRotatingAt = d.cfg.RotateTime.FirstCheckTime(now)
		if d.cfg.RotateMode == ModeCreate {
			// logfile = d.cfg.Filepath + "." + now.Format(d.suffixFormat)
			logfile = d.buildFilePath(now.Format(d.suffixFormat))
		}
	}

	// open the current file
	return d.openFile(logfile)
}

// Config gets the config
func (d *Writer) Config() Config {
	return *d.cfg
}

// Flush sync data to disk. alias of Sync()
func (d *Writer) Flush() error {
	return d.file.Sync()
}

// Sync data to disk.
func (d *Writer) Sync() error {
	return d.file.Sync()
}

// Close the writer. will sync data to disk, then close the file handle.
// and it will stop the async clean backups.
func (d *Writer) Close() error {
	if d.cfg.CleanOnClose {
		d.debugLog("cfg.CleanOnClose=true: start clean old files")
		internal.PrintErrln("files-clear-onClose: cleanup old files error:", d.Clean())
	}

	return d.close(true)
}

// MustClose the writer. alias of Close(), but will panic if has error.
func (d *Writer) MustClose() {
	internal.PrintErrln("rotatefile: close writer -", d.Close())
}

func (d *Writer) close(closeStopCh bool) error {
	if err := d.file.Sync(); err != nil {
		return err
	}

	// stop the async clean backups, then wait for the consumer goroutine to exit.
	// NOTE: take the channel out under lock but close + Wait outside the lock,
	// otherwise the consumer's doClean (which needs RLock) would deadlock.
	if closeStopCh {
		d.mu.Lock()
		stopCh := d.stopCh
		d.stopCh = nil
		d.mu.Unlock()

		if stopCh != nil {
			d.debugLog("close stopCh for stop async clean old files")
			close(stopCh)
			d.cleanWg.Wait()
		}
	}
	return d.file.Close()
}

//
// ---------------------------------------------------------------------------
// write and rotate file
// ---------------------------------------------------------------------------
//

// WriteString implements the io.StringWriter
func (d *Writer) WriteString(s string) (n int, err error) {
	return d.Write([]byte(s))
}

// Write data to file. then check and do rotate file, then async clean backups
func (d *Writer) Write(p []byte) (n int, err error) {
	// do write data
	if n, err = d.doWrite(p); err != nil {
		return
	}

	// do rotate file
	err = d.doRotate()
	// async clean backup files.
	if err == nil && d.shouldClean(true) {
		d.asyncClean()
	}
	return
}

func (d *Writer) doWrite(p []byte) (n int, err error) {
	// if enable lock
	if !d.cfg.CloseLock {
		d.mu.Lock()
		defer d.mu.Unlock()
	}

	n, err = d.file.Write(p)
	if err == nil {
		// update size
		d.written += uint64(n)
	}
	return
}

// Rotate the file by config and async clean backups
func (d *Writer) Rotate() error {
	err := d.doRotate()

	// async clean backup files.
	if err == nil && d.shouldClean(true) {
		d.asyncClean()
	}
	return err
}

// do rotate the logfile by config
func (d *Writer) doRotate() (err error) {
	// if enable lock
	if !d.cfg.CloseLock {
		d.mu.Lock()
		defer d.mu.Unlock()
	}

	// do rotate a file by size
	if d.cfg.MaxSize > 0 {
		if d.written >= d.nextSizeSync {
			d.syncWrittenSize()
		}
		if d.written >= d.cfg.MaxSize {
			if err = d.rotatingBySize(); err != nil {
				return
			}
		}
	}

	// do rotate a file by time
	if d.checkInterval > 0 && d.written > 0 {
		err = d.rotatingByTime()
	}
	return
}

// TIP: should only call on d.checkInterval > 0
func (d *Writer) rotatingByTime() error {
	now := d.cfg.TimeClock.Now()
	if now.Before(d.nextRotatingAt) {
		return nil
	}

	// generate new file path.
	// eg: /tmp/error.log => /tmp/error.20220423_1600.log
	// file := d.cfg.Filepath + "." + d.nextRotatingAt.Format(d.suffixFormat)
	file := d.buildFilePath(d.nextRotatingAt.Format(d.suffixFormat))
	if d.cfg.RotateMode == ModeCreate {
		// ModeCreate keeps every period in its own file: open the file of the
		// period that starts now (like init does), nothing is renamed.
		file = d.buildFilePath(now.Format(d.suffixFormat))
	}
	err := d.rotatingFile(file, false)

	// calc and storage next rotating time
	d.nextRotatingAt = d.nextRotatingAt.Add(time.Duration(d.checkInterval) * time.Second)
	return err
}

func (d *Writer) rotatingBySize() error {
	// The check below and the rename run under one process-wide lock (see
	// bakRenameMu), so two writers of this process never both rename the file.
	bakRenameMu.Lock()
	defer bakRenameMu.Unlock()

	// Several processes may append to the same file. When another one has
	// already rotated it, d.path names a new file: switch to it instead of
	// renaming that fresh file away a second time.
	if moved, err := d.pathMovedAway(); err == nil && moved {
		if err := d.close(false); err != nil {
			return err
		}
		return d.openFile(d.path)
	}

	d.rotateNum++
	now := d.cfg.TimeClock.Now()
	// up: use now minutes + seconds as rotate number
	numStr := fmt.Sprintf("%d%d%d", now.Hour(), now.Minute(), now.Second())
	numInt := strutil.IntOr(numStr, 0) + now.Nanosecond()/1000
	rotateNum := uint(numInt) + d.rotateNum

	var bakFile string
	if d.cfg.IsMode(ModeCreate) {
		// eg: /tmp/error.log => /tmp/error.894136.log
		// eg: /tmp/error.20220423_1600.log => /tmp/error.20220423_1600_894136.log
		pathNoExt := d.path[:len(d.path)-len(d.fileExt)]
		bakFile = fmt.Sprintf("%s_%d%s", pathNoExt, rotateNum, d.fileExt)
	} else if d.cfg.RenameFunc != nil {
		// rename current to new file by custom RenameFunc
		// eg: /tmp/error.log => /tmp/error.163021_894136.log
		bakFile = d.cfg.RenameFunc(d.cfg.Filepath, rotateNum)
	} else {
		// eg: /tmp/error.log => /tmp/error.25031615_894136.log
		bakFile = d.buildFilePath(fmt.Sprintf("%s_%d", now.Format("06010215"), rotateNum))
	}

	// Writers sharing one file can rotate at the same moment and build the same
	// name (the number above is not unique: "%d%d%d" has no padding and a µs
	// clock repeats across writers). os.Rename silently replaces an existing
	// target, which deletes the other writer's rotated file, so the name is
	// picked and renamed under one process-wide lock (taken above) and made
	// unique against files another process created.
	bakFile = uniqueBakPath(bakFile)

	// always rename current to a new file
	return d.rotatingFile(bakFile, true)
}

// bakRenameMu serializes picking a rotated file name and renaming to it inside
// one process, so two writers never rename onto the same name.
var bakRenameMu sync.Mutex

// uniqueBakPath returns path, or path with "_1", "_2"... before the extension
// when a file of that name already exists.
func uniqueBakPath(path string) string {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// rotateFile closes the syncBuffer's file and starts a new one.
func (d *Writer) rotatingFile(bakFile string, rename bool) error {
	// close the current file
	if err := d.close(false); err != nil {
		return err
	}

	// record old files for clean.
	// d.oldFiles = append(d.oldFiles, bakFile)

	// rename current to a new file.
	if rename || d.cfg.RotateMode == ModeRename {
		if err := os.Rename(d.path, bakFile); err != nil {
			// The current file is already closed: reopen it, or every later
			// write fails and the lines are lost. A file that no longer exists
			// was rotated by another process sharing it - not an error, the
			// reopen starts the new file.
			if oerr := d.openFile(d.path); oerr != nil {
				return errorx.Withf(oerr, "reopen %s after rename failed (%v)", d.path, err)
			}
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
	}

	// filepath for reopening: ModeRename always writes Config.Filepath; ModeCreate
	// reopens the same path after a size rotation (renamed away above) and opens
	// bakFile, the new period's file, on a time rotation.
	logfile := d.path
	if d.cfg.RotateMode == ModeRename {
		logfile = d.cfg.Filepath
	} else if !rename {
		logfile = bakFile
	}

	// reopen log file (openFile sets written from its size)
	return d.openFile(logfile)
}

// syncWrittenSize refreshes the written counter from the open file, so the
// lines other processes appended to the same file count toward MaxSize too. It
// runs every tenth of MaxSize written, not on every write.
func (d *Writer) syncWrittenSize() {
	if fi, err := d.file.Stat(); err == nil && uint64(fi.Size()) > d.written {
		d.written = uint64(fi.Size())
	}
	step := d.cfg.MaxSize / 10
	if step == 0 {
		step = 1
	}
	d.nextSizeSync = d.written + step
}

// pathMovedAway reports whether d.path now names another file than the one
// this writer holds open (another process rotated it).
func (d *Writer) pathMovedAway() (bool, error) {
	held, err := d.file.Stat()
	if err != nil {
		return false, err
	}
	cur, err := os.Stat(d.path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !os.SameFile(held, cur), nil
}

//
// ---------------------------------------------------------------------------
// clean backup files
// ---------------------------------------------------------------------------
//

// check should clean old files by config
func (d *Writer) shouldClean(withRand bool) bool {
	cfgIsYes := d.cfg.BackupNum > 0 || d.cfg.BackupTime > 0
	if !withRand {
		return cfgIsYes
	}

	// 20% probability trigger clean
	return cfgIsYes && rand.Intn(100) < 20
}

// async clean old files by config.
func (d *Writer) asyncClean() {
	if !d.shouldClean(false) {
		return
	}

	// lazily start the single clean-consumer goroutine.
	//
	// NOTE: use sync.Once + full lock. The previous version used RLock (a shared
	// lock) to guard the lazy init, which provides no mutual exclusion, so two
	// concurrent Write() calls could both create channels and start goroutines.
	d.cleanOnce.Do(func() {
		d.mu.Lock()
		d.debugLog("INIT clean and stop channels for clean old files")
		// buffered(1) so a needed clean isn't lost while the consumer is busy.
		d.cleanCh = make(chan struct{}, 1)
		d.stopCh = make(chan struct{})
		// capture channels locally so close()/nil-ing fields can't affect the goroutine
		cleanCh, stopCh := d.cleanCh, d.stopCh
		d.mu.Unlock()

		d.cleanWg.Add(1)
		go d.cleanConsumer(cleanCh, stopCh)
	})

	d.notifyClean()
}

// cleanConsumer consumes clean signals until stopCh is closed.
func (d *Writer) cleanConsumer(cleanCh, stopCh chan struct{}) {
	defer d.cleanWg.Done()
	d.debugLog("START a goroutine consumer for clean old files")
	for {
		select {
		case <-cleanCh:
			d.debugLog("receive signal - clean old files handling")
			internal.PrintErrln("rotatefile: clean old files error:", d.doClean())
		case <-stopCh:
			d.debugLog("STOP consumer for clean old files")
			return // stop clean
		}
	}
}

func (d *Writer) notifyClean() {
	d.mu.RLock()
	ch := d.cleanCh
	d.mu.RUnlock()
	if ch == nil {
		return
	}

	select {
	case ch <- struct{}{}: // notify clean old files
		d.debugLog("sent signal - start clean old files...")
	default: // skip on blocking
		d.debugLog("clean old files signal blocked, SKIP")
	}
}

// Clean old files by config
func (d *Writer) Clean() (err error) {
	if d.cfg.BackupNum == 0 && d.cfg.BackupTime == 0 {
		return errorx.Err("clean: backupNum and backupTime are both 0")
	}

	// up: 单独运行清理，不需要设置 skipSeconds
	return d.doClean(0)
}

// do clean old files by config
//
// - skipSeconds: skip find files that are within the specified seconds
func (d *Writer) doClean(skipSeconds ...int) (err error) {
	// oldFiles: xx.log.yy files, no gz file
	var oldFiles, gzFiles []fsutil.FileInfo
	// fileDir/fileName are immutable after init(); d.path is changed on rotate,
	// so snapshot it under pathMu to avoid racing with openFile.
	fileDir, fileName := d.fileDir, d.fileName
	d.pathMu.RLock()
	curFileName := filepath.Base(d.path)
	d.pathMu.RUnlock()

	// FIX: do not process recent changes to avoid conflicts
	skipSec := 30
	if len(skipSeconds) > 0 {
		skipSec = skipSeconds[0]
	}
	limitTime := d.cfg.TimeClock.Now().Add(-time.Second * time.Duration(skipSec))

	// find and clean old files
	d.debugLog("clean - find old files, match name:", fileName, ", in dir:", fileDir)
	err = fsutil.FindInDir(fileDir, func(fPath string, ent fs.DirEntry) error {
		fi, err := ent.Info()
		if err != nil {
			return err
		}

		// fix: exclude the current file
		if ent.Name() == curFileName {
			return nil
		}

		if strings.HasSuffix(ent.Name(), internal.CompressSuffix) {
			gzFiles = append(gzFiles, fsutil.NewFileInfo(fPath, fi))
		} else if fi.ModTime().Before(limitTime) {
			oldFiles = append(oldFiles, fsutil.NewFileInfo(fPath, fi))
		}
		return nil
	}, d.buildFilterFns(fileName)...)

	gzNum := len(gzFiles)
	oldNum := len(oldFiles)
	remNum := mathutil.Max(gzNum+oldNum-int(d.cfg.BackupNum), 0)
	d.debugLog("clean old files, gzNum:", gzNum, "oldNum:", oldNum, "remNum:", remNum)

	if remNum > 0 && d.cfg.BackupNum > 0 {
		// remove old gz files
		if gzNum > 0 {
			remNum, err = d.removeOldGzFiles(remNum, gzFiles)
			if err != nil {
				return err
			}
		}

		// remove old log files
		if remNum > 0 && oldNum > 0 {
			oldFiles, err = d.removeOldFiles(remNum, oldFiles)
			if err != nil {
				return err
			}
		}
	}

	if d.cfg.Compress && len(oldFiles) > 0 {
		d.debugLog("compress old normal files to gz files")
		err = d.compressFiles(oldFiles)
	}
	return
}

// remove old gz files
func (d *Writer) removeOldGzFiles(remNum int, gzFiles []fsutil.FileInfo) (rn int, err error) {
	gzNum := len(gzFiles)
	sort.Sort(fsutil.FileInfos(gzFiles)) // sort by mod-time
	d.debugLog("remove old gz files ...")

	for idx := 0; idx < gzNum; idx++ {
		d.debugLog("remove old gz file:", gzFiles[idx].Path())
		if err = os.Remove(gzFiles[idx].Path()); err != nil {
			break
		}

		remNum--
		if remNum == 0 {
			break
		}
	}

	if err != nil {
		return remNum, errorx.Wrap(err, "remove old gz file error")
	}
	return remNum, nil
}

// remove old log files
func (d *Writer) removeOldFiles(remNum int, oldFiles []fsutil.FileInfo) (files []fsutil.FileInfo, err error) {
	// sort by mod-time, oldest at first.
	sort.Sort(fsutil.FileInfos(oldFiles))
	d.debugLog("remove old normal files ...")

	var idx int
	oldNum := len(oldFiles)

	for idx = 0; idx < oldNum; idx++ {
		d.debugLog("remove old file:", oldFiles[idx].Path())
		if err = os.Remove(oldFiles[idx].Path()); err != nil {
			break
		}

		remNum--
		if remNum == 0 {
			break
		}
	}

	oldFiles = oldFiles[idx+1:]
	if err != nil {
		return nil, errorx.Wrap(err, "remove old file error")
	}
	return oldFiles, nil
}

//
// ---------------------------------------------------------------------------
// helper methods
// ---------------------------------------------------------------------------
//

// open the current file. and set the d.file, d.path
func (d *Writer) openFile(logfile string) error {
	file, err := fsutil.OpenFile(logfile, DefaultFileFlags, d.cfg.FilePerm)
	if err != nil {
		return err
	}

	d.pathMu.Lock()
	d.path = logfile
	d.pathMu.Unlock()
	d.file = file

	// An existing file is appended to: count what it already holds, so a
	// restarted process (or one of several sharing the file) does not grow it
	// past MaxSize.
	d.written = 0
	if fi, err := file.Stat(); err == nil {
		d.written = uint64(fi.Size())
	}
	d.nextSizeSync = 0
	return nil
}

// return eg. logs/error.20220423_1600.log
func (d *Writer) buildFilePath(suffix string) string {
	fileName := d.onlyName + "." + suffix + d.fileExt
	return fmt.Sprintf("%s/%s", d.fileDir, fileName)
}

func (d *Writer) buildFilterFns(fileName string) []fsutil.FilterFunc {
	onlyName := d.onlyName
	filterFns := []fsutil.FilterFunc{
		fsutil.OnlyFindFile,
		// filter by name. match pattern like: error.log.* eg: error.log.xx, error.log.xx.gz
		func(fPath string, ent fs.DirEntry) bool {
			// ok, _ := path.Match(fileName+".*", ent.Name())
			if !strings.HasPrefix(ent.Name(), fileName) {
				// 自定义文件名 eg: error.log -> error.20220423_02.log
				return strings.HasPrefix(ent.Name(), onlyName)
			}
			return true
		},
	}

	// filter by mod-time, clear expired files
	if d.cfg.BackupTime > 0 {
		cutTime := d.cfg.TimeClock.Now().Add(-d.backupDur)
		filterFns = append(filterFns, func(fPath string, ent fs.DirEntry) bool {
			fi, err := ent.Info()
			if err != nil {
				return false // skip, not handle
			}

			// collect un-expired
			if fi.ModTime().After(cutTime) {
				return true
			}

			// remove expired files
			d.debugLog("remove expired file:", fPath)
			internal.PrintErrln("rotatefile: remove expired file error:", os.Remove(fPath))
			return false
		})
	}

	return filterFns
}

func (d *Writer) compressFiles(oldFiles []fsutil.FileInfo) error {
	for _, fi := range oldFiles {
		err := internal.CompressFile(fi.Path(), fi.Path()+internal.CompressSuffix)
		if err != nil {
			return errorx.Wrap(err, "compress old file error")
		}

		// remove an old log file
		d.debugLog("compress and rm old file:", fi.Path())
		if err = os.Remove(fi.Path()); err != nil {
			return errorx.Wrap(err, "remove file error after compress")
		}
	}
	return nil
}

// Debug print debug message on development
func (d *Writer) debugLog(vs ...any) {
	if d.cfg.DebugMode {
		stdio.WriteString("[rotatefile.DEBUG] ID:" + d.id + " | " + fmt.Sprintln(vs...))
	}
}
