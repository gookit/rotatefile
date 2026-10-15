# Rotate File

[![GoDoc](https://pkg.go.dev/badge/github.com/gookit/rotatefile.svg)](https://pkg.go.dev/github.com/gookit/rotatefile)
[![Go Report Card](https://goreportcard.com/badge/github.com/gookit/rotatefile)](https://goreportcard.com/report/github.com/gookit/rotatefile)
[![Unit-Tests](https://github.com/gookit/rotatefile/workflows/Unit-Tests/badge.svg)](https://github.com/gookit/rotatefile/actions)
[![GitHub tag](https://img.shields.io/github/tag/gookit/rotatefile)](https://github.com/gookit/rotatefile)

`rotatefile` is a lightweight Go library for **log file rotation, cleanup and gzip compression**.

> [中文说明](README.zh-CN.md) | English

`rotatefile.Writer` is a plain `io.Writer`, so it drops into the standard library
`log/slog`, the standard `log`, `zap`, [gookit/slog](https://github.com/gookit/slog) —
any logger that writes to an `io.Writer`. The Go standard library has no built-in log
rotation; this fills that gap.

> 🪄 `rotatefile` is a package split from [gookit/slog](https://github.com/gookit/slog).

## Features

- Rotate by **size** and/or **time** (every hour / day / 30min / minute …)
- Two rotate modes: `rename` (write to one file, rename on rotate) and `create`
  (write to a new dated file each period)
- **Cleanup** old files by `BackupNum` (max count) and/or `BackupTime` (max age)
- **Compress** rotated files with gzip
- Customizable: filename for size-rotation, time clock, file permission
- `FilesClear` — a standalone old-files cleaner, usable for any program's logs
  (even non-Go ones, e.g. PHP-FPM)
- [`filecleaner`](cmd/filecleaner) CLI — a JSON-configured command-line cleaner built on `FilesClear`
- Sub-package [`bufwrite`](bufwrite) — buffered writers, incl. `LineWriter` that keeps
  every write (one log line) intact
- Tiny dependency surface: only `github.com/gookit/goutil`

## Install

```bash
go get github.com/gookit/rotatefile
```

## Quick Start

### Create a rotating writer

```go
package main

import "github.com/gookit/rotatefile"

func main() {
	w, err := rotatefile.NewConfig("testdata/app.log").Create()
	if err != nil {
		panic(err)
	}
	defer w.Close() // flush + close

	_, _ = w.Write([]byte("a log message\n"))
}
```

### Common config options

```go
w, err := rotatefile.NewConfig("testdata/app.log", func(c *rotatefile.Config) {
	c.MaxSize = 100 * rotatefile.OneMByte // rotate at 100MB (0 = disable size rotate)
	c.RotateTime = rotatefile.EveryDay    // also rotate daily (0 = disable time rotate)
	c.RotateMode = rotatefile.ModeRename  // or rotatefile.ModeCreate
	c.BackupNum = 30                      // keep at most 30 old files
	c.BackupTime = 24 * 7                 // and/or keep files up to a week (hours)
	c.Compress = true                     // gzip rotated files
}).Create()
```

See [Config on GoDoc](https://pkg.go.dev/github.com/gookit/rotatefile#Config) for the full list.

### Use with the standard `log/slog` (Go 1.21+)

```go
import (
	"log/slog"

	"github.com/gookit/rotatefile"
)

w, _ := rotatefile.NewConfig("testdata/app.log", func(c *rotatefile.Config) {
	c.MaxSize = 50 * rotatefile.OneMByte
	c.RotateTime = rotatefile.EveryDay
	c.BackupNum = 7
}).Create()

logger := slog.New(slog.NewJSONHandler(w, nil))
logger.Info("log via std slog", "key", "value")
```

### Use with the standard `log` (or zap, etc.)

```go
import (
	"log"

	"github.com/gookit/rotatefile"
)

w, _ := rotatefile.NewConfig("testdata/app.log").Create()
log.SetOutput(w)
log.Println("log message")
```

Any logger that accepts an `io.Writer` works the same way (e.g. zap via `zapcore.AddSync(w)`).

### Buffered writing (`bufwrite`)

```go
import (
	"github.com/gookit/rotatefile"
	"github.com/gookit/rotatefile/bufwrite"
)

w, _ := rotatefile.NewConfig("testdata/app.log").Create()

// LineWriter keeps each Write (one log line) intact - it won't split a record
// across a flush, so an external collector never reads a half-written line.
bw := bufwrite.NewLineWriter(w)
defer bw.Close() // flush + close

_, _ = bw.Write([]byte("a complete log line\n"))
```

## Clean old files (`FilesClear`)

`FilesClear` cleans old/expired files by pattern, independent of the rotating writer.
It can also run as a background daemon.

```go
fc := rotatefile.NewFilesClear(func(c *rotatefile.CConfig) {
	c.AddPattern("/path/to/some*.log")
	c.BackupNum = 2
	c.BackupTime = 12 // 12 hours
})

// one-off clean
_ = fc.Clean()

// or run on a daemon
go fc.DaemonClean(nil)
// NOTE: stop the daemon before exit
// fc.StopDaemon()
```

See [CConfig on GoDoc](https://pkg.go.dev/github.com/gookit/rotatefile#CConfig) for clean options.

## `filecleaner` CLI

[`cmd/filecleaner`](cmd/filecleaner) is a small command-line tool built on `FilesClear`.
It cleans old/expired files by patterns, configured via a JSON file — handy for cron jobs
or cleaning logs of non-Go programs.

```bash
go install github.com/gookit/rotatefile/cmd/filecleaner@latest

filecleaner -c filecleaner.json            # one-off clean
filecleaner --dry-run -c filecleaner.json  # print what would be removed, delete nothing
filecleaner --daemon  -c filecleaner.json  # run periodically until Ctrl+C
```

Config file — a `jobs` array, one retention policy per job:

```json
{
  "jobs": [
    { "patterns": ["/var/log/app/*.log.*"], "backup_num": 20, "backup_time": 168, "time_unit": "1h" },
    { "patterns": ["/var/log/svc"], "recursive": true, "remove_empty_dir": true, "backup_time": 7, "time_unit": "24h" }
  ]
}
```

See [cmd/filecleaner/README.md](cmd/filecleaner/README.md) for all options.

## Related

- [github.com/gookit/slog](https://github.com/gookit/slog) — lightweight structured logging (uses `rotatefile`)
- [github.com/gookit/goutil](https://github.com/gookit/goutil) — Go utility library

## License

[MIT](LICENSE)
