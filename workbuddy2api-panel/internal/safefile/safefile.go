// Package safefile 原子、掉电安全地替换状态文件，并在文件损坏时回退到上一份好副本。
//
// 为什么 tmp + rename 不够：rename 只保证读者看到的是一个完整文件，不保证内容
// 已经落盘。掉电时可能留下长度正确、内容全 0 的文件（ext4/XFS 上 rename 的日志
// 先提交、数据块后刷盘）。所以临时文件先 fsync 再 rename，并把替换前的有效内容
// 留成 <path>.bak —— 坏文件被读成「空」之后，下一次写入就会把它彻底抹掉，而这
// 份 .bak 是唯一的退路。
//
// 规则同 magpie 的 internal/steady + internal/lastgood（其 issue #1505：掉电留下
// 全 0 的 logins.json，读成「没有账号」，随即被下一次写入变成定局）：
//   - 文件不在是有人特意删的，不去 .bak 里复活它；
//   - 内容坏但 .bak 好 → 用 .bak，并打 WARN；
//   - 两边都坏 → 报错说清楚路径和坏法，绝不悄悄当成空。
package safefile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	// staleTempAge 残留临时文件多久算「上一个进程被硬杀留下的」（审查 C7-091）。
	// 1 小时足够避开任何一次正常写入（毫秒级），也不会误删并发进程刚建的 tmp。
	staleTempAge = time.Hour
	// maxBadCopies <path>.bad-* 最多保留几份（审查 C7-090）。
	// 反复损坏（某次写盘 bug 持续）时此前是「每次写入新增一份、永不清理」，
	// 目录里堆成垃圾；保留最近 5 份足够事后定位，更早的没有额外信息。
	maxBadCopies = 5
)

// pathLocks 每个目标路径一把进程内互斥（审查 C7-092）。
//
// WriteKeep 的「读 path → 比较 .bak → 写 .bak」与随后的 replace 不是原子的：
// 两个 goroutine 同时 WriteKeep 同一路径时，两边都读到旧内容、各写一次 .bak，
// .bak 与 path 可能错位（回退时拿到更旧的一代，甚至拿到对方写了一半的中间态）。
// 条目数 = 进程内写过的不同路径数（状态文件/凭据文件，几十个量级），无需清理。
var pathLocks sync.Map // path → *sync.Mutex

// lockPath 取路径锁，返回解锁函数。用法：defer lockPath(p)()
func lockPath(path string) func() {
	v, _ := pathLocks.LoadOrStore(path, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// syncDirHook 目录 fsync 的间接层：默认就是 syncDir，用例换成计数器以钉住
// 「rename 之后确实 sync 了目录」（审查 C7-089）——目录 fsync 本身不可观测。
var syncDirHook = syncDir

// syncDir 尽力 fsync 目录，让 rename 的**目录项**落盘（审查 C7-089）。
//
// 此前只 f.Sync() 了临时文件：内容落盘了，但「path 指向新 inode」这个目录项还
// 在页缓存里。掉电后 rename 可能丢失 —— 文件回到旧版甚至不存在（ext4
// data=writeback / f2fs 上可复现），正是包注释引用的 magpie issue #1505 那类现场。
//
// 与 auth.ReplaceAtomic 的 syncDir 同口径：失败**不**改变返回值。有些文件系统/
// Android 分区对目录 fsync 返回 EINVAL，此时退回「内容已 fsync」的既有保证；
// 报错反而会让调用方以为整次写入失败而重写一遍。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// Bak 返回 path 的上一份好副本路径。
func Bak(path string) string { return path + ".bak" }

// Write 用 b 原子替换 path：同目录临时文件写完并 fsync 后才 rename，
// 任何一步失败都删掉临时文件、path 保持原样。
func Write(path string, b []byte, perm fs.FileMode) error {
	defer lockPath(path)() // 同路径串行（审查 C7-092）
	return replace(path, b, perm)
}

// WriteKeep 同 Write，并在替换前把 path 现有的有效内容留成 <path>.bak。
// 现有内容本身已损坏时另存 <path>.bad-<时间>，不拿坏内容污染 .bak。
func WriteKeep(path string, b []byte, perm fs.FileMode) error {
	// 整段（读旧内容 → 写 .bak → 替换正文）在同一把路径锁内（审查 C7-092）：
	// 只锁 replace 是不够的，.bak 的读改写同样会交错。
	defer lockPath(path)()
	if err := keep(path, perm); err != nil {
		return err
	}
	return replace(path, b, perm)
}

// Read 读 path；内容通不过 valid（nil 表示 json.Valid）时回退到 <path>.bak。
// path 不存在返回 fs.ErrNotExist（调用方据此区分「没配置」与「配置坏了」），
// 此时不看 .bak。
func Read(path string, valid func([]byte) bool) ([]byte, error) {
	if valid == nil {
		valid = json.Valid
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if valid(b) {
		return b, nil
	}
	why := describe(b)
	if bak, bakErr := os.ReadFile(Bak(path)); bakErr == nil && valid(bak) {
		log.Printf("WARN: safefile: %s %s，已回退到 %s（原文件保持不动，下次写入前会另存 .bad）", path, why, Bak(path))
		return bak, nil
	}
	return nil, fmt.Errorf("%s %s，且没有可用的 %s", path, why, filepath.Base(Bak(path)))
}

// keep 把 path 现有的有效内容留成 .bak（与 .bak 相同则跳过）。
func keep(path string, perm fs.FileMode) error {
	prev, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !json.Valid(prev) {
		bad := path + ".bad-" + time.Now().Format("20060102-150405")
		if err := replace(bad, prev, perm); err != nil {
			return err
		}
		log.Printf("WARN: safefile: %s %s，已另存 %s", path, describe(prev), bad)
		pruneBad(path, maxBadCopies) // 保留最近 N 份（审查 C7-090）
		return nil
	}
	if old, err := os.ReadFile(Bak(path)); err == nil && bytes.Equal(old, prev) {
		return nil
	}
	return replace(Bak(path), prev, perm)
}

// replace 写临时文件 → fsync → rename → fsync 目录。
func replace(path string, b []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	// 父目录不存在时先建（审查 C7-094）：CreateTemp 不会替你建目录，首次部署
	// （或刚清过目录）时落盘会直接失败，而调用方多半只记一行日志。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// 顺手清掉上一个进程被硬杀留下的临时文件（审查 C7-091）：defer os.Remove
	// 只在正常返回路径执行，kill -9 每次留一个 `.<base>.XXXX`，长期运行会堆满目录。
	sweepStaleTemps(dir, filepath.Base(path))
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = ""
	// rename 之后必须 fsync 父目录（审查 C7-089），否则掉电时目录项可能丢。
	if err := syncDirHook(dir); err != nil {
		// 尽力而为：内容已经 fsync 过，path 此刻确实指向新内容；目录 fsync 失败
		// （部分 FS 返回 EINVAL）不该让调用方以为写入失败而重写一遍。
		log.Printf("WARN: safefile: %s 目录 fsync 失败（内容已落盘，rename 的目录项未确认）: %v", dir, err)
	}
	return nil
}

// sweepStaleTemps 清理 dir 下与某目标文件同前缀、且超过 staleTempAge 的残留临时文件
// （审查 C7-091）。只动 `.` 开头、名字匹配 `.<base>.` 前缀的文件。
func sweepStaleTemps(dir, base string) {
	matches, err := filepath.Glob(filepath.Join(dir, "."+base+".*"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleTempAge)
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(m)
	}
}

// pruneBad 只保留最近 keep 份 <path>.bad-*（审查 C7-090）。
// 文件名里的时间戳是 20060102-150405，字典序即时间序，直接排序后删最旧的。
func pruneBad(path string, keep int) {
	matches, err := filepath.Glob(path + ".bad-*")
	if err != nil || len(matches) <= keep {
		return
	}
	sort.Strings(matches)
	for _, m := range matches[:len(matches)-keep] {
		if err := os.Remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("WARN: safefile: 清理旧 .bad 副本 %s 失败: %v", filepath.Base(m), err)
		}
	}
}

// describe 说清楚坏在哪，供日志和报错使用。
func describe(b []byte) string {
	switch {
	case len(b) == 0:
		return "是空文件"
	case len(bytes.Trim(b, "\x00")) == 0:
		return strconv.Itoa(len(b)) + " 字节全为 0"
	case len(bytes.TrimSpace(b)) == 0:
		return "只有空白字符"
	}
	return "不是合法 JSON"
}
