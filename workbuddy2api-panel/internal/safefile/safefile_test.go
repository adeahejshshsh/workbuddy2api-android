package safefile

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := Write(path, b, 0o600); err != nil {
		t.Fatalf("Write(%s): %v", path, err)
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return b
}

// 第二次写入后 .bak 是上一份内容，第三次之后仍是上一次的（不是最早的）。
func TestWriteKeepKeepsPrevious(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	write(t, p, []byte(`{"v":1}`))
	if err := WriteKeep(p, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(read(t, Bak(p)))); got != `{"v":1}` {
		t.Fatalf(".bak = %s", got)
	}
	if err := WriteKeep(p, []byte(`{"v":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(read(t, Bak(p)))); got != `{"v":2}` {
		t.Fatalf(".bak 不是上一份: %s", got)
	}
	if got := strings.TrimSpace(string(read(t, p))); got != `{"v":3}` {
		t.Fatalf("正文 = %s", got)
	}
}

// 掉电现场：文件长度正确但内容全 0 —— 读回 .bak，原文件不动。
func TestReadFallsBackToBak(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logins.json")
	good := []byte(`{"a":"token"}`)
	if err := WriteKeep(p, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeep(p, []byte(`{"a":"newer"}`), 0o600); err != nil { // .bak = good
		t.Fatal(err)
	}
	zeroed := make([]byte, len(read(t, p)))
	if err := os.WriteFile(p, zeroed, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p, nil)
	if err != nil || string(got) != string(good) {
		t.Fatalf("Read = %s, %v", got, err)
	}
	if string(read(t, p)) == string(good) {
		t.Fatal("Read 不该改原文件")
	}
}

// 文件不在是有人特意删的：不复活 .bak，错误要能被 errors.Is(fs.ErrNotExist) 认出。
func TestReadMissingIsNotExist(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := WriteKeep(p, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(p, nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}

// 两边都坏：报错说清楚路径和坏法，不返回空内容。
func TestReadBothBad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Read(p, nil)
	if err == nil || !strings.Contains(err.Error(), "全为 0") {
		t.Fatalf("err = %v", err)
	}
}

// 覆盖一个已损坏的文件：坏内容另存 .bad-*，.bak 不被污染。
func TestWriteKeepFromCorruptFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.json")
	write(t, p, []byte(`{"good":1}`))
	if err := WriteKeep(p, []byte(`{"good":2}`), 0o600); err != nil { // .bak = {"good":1}
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"trunc`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeep(p, []byte(`{"good":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(read(t, Bak(p)))); got != `{"good":1}` {
		t.Fatalf(".bak 被坏内容污染: %s", got)
	}
	bad, err := filepath.Glob(p + ".bad-*")
	if err != nil || len(bad) != 1 {
		t.Fatalf(".bad 副本 %v, %v", bad, err)
	}
	if got := string(read(t, bad[0])); got != `{"trunc` {
		t.Fatalf(".bad 内容 = %s", got)
	}
}

// 自定义校验：JSON 合法但语义不合格的文件也要走 .bak。
func TestReadCustomValidator(t *testing.T) {
	p := filepath.Join(t.TempDir(), "workbuddy-x.json")
	if err := WriteKeep(p, []byte(`{"auth":{"accessToken":"t1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeep(p, []byte(`{"auth":{"accessToken":"t2"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := func(b []byte) bool {
		var v struct {
			Auth struct {
				AccessToken string `json:"accessToken"`
			} `json:"auth"`
		}
		return json.Unmarshal(b, &v) == nil && v.Auth.AccessToken != ""
	}
	// 语义不合格（accessToken 空）的正文：JSON 合法，但必须回退 .bak。
	if err := os.WriteFile(p, []byte(`{"auth":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p, valid)
	if err != nil || !strings.Contains(string(got), "t1") {
		t.Fatalf("Read = %s, %v", got, err)
	}
}

// ── 审查 C7-089 / C7-090 / C7-091 / C7-092 / C7-094 的回归钉 ─────────────────

// TestReplaceSyncsParentDir rename 之后必须 fsync 父目录（审查 C7-089）。
//
// 目录 fsync 本身不可观测，所以用 syncDirHook 计数：此前 replace 只 f.Sync() 了
// 临时文件，rename 的目录项留在页缓存里，掉电后 rename 可能丢失（文件回到旧版或
// 不存在）。这条用例钉住「rename 成功后一定调了一次目录 fsync」。
func TestReplaceSyncsParentDir(t *testing.T) {
	var got []string
	old := syncDirHook
	syncDirHook = func(dir string) error {
		got = append(got, dir)
		return nil
	}
	t.Cleanup(func() { syncDirHook = old })

	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	write(t, p, []byte(`{"v":1}`))
	if len(got) != 1 {
		t.Fatalf("rename 后应 fsync 一次父目录，实际 %d 次: %v", len(got), got)
	}
	if got[0] != dir {
		t.Fatalf("fsync 的目录 = %q，want %q", got[0], dir)
	}
}

// TestReplaceDirSyncFailureStillSucceeds 目录 fsync 失败不改变返回值（审查 C7-089）：
// 内容已落盘、path 已指向新内容，报错只会让调用方以为写入失败而重写一遍。
func TestReplaceDirSyncFailureStillSucceeds(t *testing.T) {
	old := syncDirHook
	syncDirHook = func(string) error { return errors.New("EINVAL") }
	t.Cleanup(func() { syncDirHook = old })

	p := filepath.Join(t.TempDir(), "state.json")
	if err := Write(p, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatalf("目录 fsync 失败不该让 Write 报错: %v", err)
	}
	if got := strings.TrimSpace(string(read(t, p))); got != `{"v":2}` {
		t.Fatalf("正文 = %s", got)
	}
}

// TestPruneBadKeepsNewest 反复损坏时 .bad 副本只留最近 N 份（审查 C7-090）。
//
// 此前每写一次坏文件就新增一份、永不清理 —— 某次写盘 bug 持续时目录会被撑爆。
func TestPruneBadKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "usage.json")
	// 文件名里的时间戳字典序 = 时间序，直接造 8 份。
	for i := 1; i <= 8; i++ {
		name := p + ".bad-2026010" + strconv.Itoa(i%10) + "-00000" + strconv.Itoa(i)
		if err := os.WriteFile(name, []byte(`{"bad":`+strconv.Itoa(i)+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneBad(p, maxBadCopies)
	left, err := filepath.Glob(p + ".bad-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != maxBadCopies {
		t.Fatalf("保留 %d 份，want %d: %v", len(left), maxBadCopies, left)
	}
	sort.Strings(left)
	// 最旧的那几份必须已经被删掉。
	if strings.HasSuffix(left[0], "-000001") {
		t.Fatalf("最旧的 .bad 没被清理: %v", left)
	}
}

// TestSweepStaleTempsRemovesOnlyOld 只清「上一个进程被硬杀」留下的旧 tmp（审查 C7-091）：
// 1 小时内的（可能是并发进程正在写的）必须留着。
func TestSweepStaleTempsRemovesOnlyOld(t *testing.T) {
	dir := t.TempDir()
	base := "state.json"
	stale := filepath.Join(dir, "."+base+".stale1")
	fresh := filepath.Join(dir, "."+base+".fresh1")
	for _, f := range []string{stale, fresh} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleTempAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	sweepStaleTemps(dir, base)
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("超过 %s 的残留 tmp 应被清理: %v", staleTempAge, err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("新鲜的 tmp 不该被删（可能是并发写入）: %v", err)
	}
}

// TestLockPathSerializes 同路径互斥、不同路径不互斥（审查 C7-092）。
func TestLockPathSerializes(t *testing.T) {
	unlock := lockPath("/tmp/a.json")
	acquired := make(chan struct{})
	go func() {
		u := lockPath("/tmp/a.json")
		close(acquired)
		u()
	}()
	select {
	case <-acquired:
		t.Fatal("同一路径的两个写入应互斥")
	case <-time.After(50 * time.Millisecond):
	}
	// 不同路径不该被挡住。
	other := make(chan struct{})
	go func() {
		u := lockPath("/tmp/b.json")
		close(other)
		u()
	}()
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("不同路径不该互斥")
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("解锁后应能拿到同一路径的锁")
	}
}

// TestWriteKeepConcurrentStaysConsistent 并发 WriteKeep 同一路径后，正文与 .bak
// 都必须是一份完整 JSON（审查 C7-092）：此前「读 path → 写 .bak」与随后的 replace
// 不在同一把锁内，两个 goroutine 交错会让 .bak 与正文错位。
func TestWriteKeepConcurrentStaysConsistent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := WriteKeep(p, []byte(`{"n":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = WriteKeep(p, []byte(`{"n":`+strconv.Itoa(i*100+j)+`}`), 0o600)
			}
		}(i)
	}
	wg.Wait()
	if !json.Valid(read(t, p)) {
		t.Fatalf("正文不是合法 JSON: %q", read(t, p))
	}
	if b, err := os.ReadFile(Bak(p)); err != nil || !json.Valid(b) {
		t.Fatalf(".bak 不是合法 JSON: %q, %v", b, err)
	}
}

// TestReplaceCreatesParentDir 父目录不存在时自动创建（审查 C7-094）。
func TestReplaceCreatesParentDir(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "state.json")
	if err := Write(p, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatalf("父目录不存在时应自动创建: %v", err)
	}
	if got := strings.TrimSpace(string(read(t, p))); got != `{"v":1}` {
		t.Fatalf("正文 = %s", got)
	}
}
