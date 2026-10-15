# Rotate File

[![GoDoc](https://pkg.go.dev/badge/github.com/gookit/rotatefile.svg)](https://pkg.go.dev/github.com/gookit/rotatefile)
[![Go Report Card](https://goreportcard.com/badge/github.com/gookit/rotatefile)](https://goreportcard.com/report/github.com/gookit/rotatefile)
[![Unit-Tests](https://github.com/gookit/rotatefile/workflows/Unit-Tests/badge.svg)](https://github.com/gookit/rotatefile/actions)
[![GitHub tag](https://img.shields.io/github/tag/gookit/rotatefile)](https://github.com/gookit/rotatefile)

`rotatefile` 是一个轻量的 Go 库,提供**日志文件轮转(切割)、清理与 gzip 压缩**。

> 中文说明 | [English](README.md)

`rotatefile.Writer` 就是一个普通的 `io.Writer`,因此可直接用于标准库 `log/slog`、
标准库 `log`、`zap`、[gookit/slog](https://github.com/gookit/slog) —— 任何写入
`io.Writer` 的日志库。Go 标准库本身没有内置日志轮转,这个库正好补上。

> 🪄 `rotatefile` 是从 [gookit/slog](https://github.com/gookit/slog) 中拆分出来的包。

## 功能特色

- 按**大小**和/或**时间**轮转(每小时 / 每天 / 30 分钟 / 每分钟 …)
- 两种轮转模式:`rename`(始终写一个文件,轮转时重命名)和 `create`(每个周期写一个带日期的新文件)
- 按 `BackupNum`(最大数量)和/或 `BackupTime`(最长保留时间)**清理**旧文件
- 用 gzip **压缩**已轮转的文件
- 可定制:按大小轮转时的文件名、时间时钟、文件权限
- `FilesClear` —— 独立的旧文件清理器,可用于任意程序的日志清理(甚至非 Go,如 PHP-FPM)
- [`filecleaner`](cmd/filecleaner) 命令行工具 —— 基于 `FilesClear`、用 JSON 配置的清理命令行
- 子包 [`bufwrite`](bufwrite) —— 缓冲写,其中 `LineWriter` 保证每次写入(一条日志)完整不被拆分
- 依赖极少:仅 `github.com/gookit/goutil`

## 安装

```bash
go get github.com/gookit/rotatefile
```

## 快速开始

### 创建一个轮转写入器

```go
package main

import "github.com/gookit/rotatefile"

func main() {
	w, err := rotatefile.NewConfig("testdata/app.log").Create()
	if err != nil {
		panic(err)
	}
	defer w.Close() // 刷新 + 关闭

	_, _ = w.Write([]byte("a log message\n"))
}
```

### 常用配置项

```go
w, err := rotatefile.NewConfig("testdata/app.log", func(c *rotatefile.Config) {
	c.MaxSize = 100 * rotatefile.OneMByte // 达到 100MB 轮转(0 = 不按大小轮转)
	c.RotateTime = rotatefile.EveryDay    // 同时每天轮转(0 = 不按时间轮转)
	c.RotateMode = rotatefile.ModeRename  // 或 rotatefile.ModeCreate
	c.BackupNum = 30                      // 最多保留 30 个旧文件
	c.BackupTime = 24 * 7                 // 和/或最多保留一周(单位:小时)
	c.Compress = true                     // gzip 压缩已轮转文件
}).Create()
```

完整配置见 [GoDoc 上的 Config](https://pkg.go.dev/github.com/gookit/rotatefile#Config)。

### 配合标准库 `log/slog`(Go 1.21+)

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

### 配合标准库 `log`(或 zap 等)

```go
import (
	"log"

	"github.com/gookit/rotatefile"
)

w, _ := rotatefile.NewConfig("testdata/app.log").Create()
log.SetOutput(w)
log.Println("log message")
```

任何接受 `io.Writer` 的日志库用法相同(例如 zap 用 `zapcore.AddSync(w)`)。

### 缓冲写(`bufwrite`)

```go
import (
	"github.com/gookit/rotatefile"
	"github.com/gookit/rotatefile/bufwrite"
)

w, _ := rotatefile.NewConfig("testdata/app.log").Create()

// LineWriter 保证每次 Write(一条日志)完整不被拆分 —— 不会把一条记录拆到两次 flush,
// 外部采集工具不会读到半行内容。
bw := bufwrite.NewLineWriter(w)
defer bw.Close() // 刷新 + 关闭

_, _ = bw.Write([]byte("a complete log line\n"))
```

## 清理旧文件(`FilesClear`)

`FilesClear` 按模式清理旧/过期文件,独立于轮转写入器,也可作为后台守护运行。

```go
fc := rotatefile.NewFilesClear(func(c *rotatefile.CConfig) {
	c.AddPattern("/path/to/some*.log")
	c.BackupNum = 2
	c.BackupTime = 12 // 12 小时
})

// 单次清理
_ = fc.Clean()

// 或作为守护运行
go fc.DaemonClean(nil)
// 注意:退出前需停止守护
// fc.StopDaemon()
```

清理选项见 [GoDoc 上的 CConfig](https://pkg.go.dev/github.com/gookit/rotatefile#CConfig)。

## `filecleaner` 命令行工具

[`cmd/filecleaner`](cmd/filecleaner) 是基于 `FilesClear` 的命令行小工具,通过 JSON 文件配置,
按 pattern 清理旧/过期文件 —— 适合放进 cron 定时任务,或清理非 Go 程序的日志。

```bash
go install github.com/gookit/rotatefile/cmd/filecleaner@latest

filecleaner -c filecleaner.json            # 一次性清理
filecleaner --dry-run -c filecleaner.json  # 预演:只打印将删除的文件,不实际删除
filecleaner --daemon  -c filecleaner.json  # 周期运行,直到 Ctrl+C
```

配置文件 —— `jobs` 数组,每个 job 一组独立保留策略:

```json
{
  "jobs": [
    { "patterns": ["/var/log/app/*.log.*"], "backup_num": 20, "backup_time": 168, "time_unit": "1h" },
    { "patterns": ["/var/log/svc"], "recursive": true, "remove_empty_dir": true, "backup_time": 7, "time_unit": "24h" }
  ]
}
```

完整选项见 [cmd/filecleaner/README.md](cmd/filecleaner/README.md)。

## 相关项目

- [github.com/gookit/slog](https://github.com/gookit/slog) —— 轻量结构化日志库(内部使用 `rotatefile`)
- [github.com/gookit/goutil](https://github.com/gookit/goutil) —— Go 工具库

## License

[MIT](LICENSE)
