//go:build ignore

// crossacct 判定上游前缀缓存是否跨账号共享（issue #277 后续 / 用户提问「跨号缓存」）。
//
// 2026-11-20 实测（CN 域 deepseek-v4.1-flash，前缀 22,755 token）：
//
//	A#1 冷启动   hit%= 0.0     A#2 复打   hit%= 99.0
//	B#1 跨号试探 hit%= 0.0     B#2 复打   hit%= 99.0     A#3 回头 hit%= 99.0
//
// ⇒ 前缀缓存**按账号隔离**：换号等于从零重吃。网关上不存在「共享别号缓存」的做法，
// 唯一的杠杆是把同一会话钉在同一账号（internal/session 粘性路由，现网 ttl=30m）。
//
// 判定：同一段长前缀依次发到 A、A、B、B。
//
//	A#2 命中   → 同一账号内缓存有效（对照）
//	B#1 命中   → 缓存跨账号共享 → 网关上做「跨号缓存」无意义也无需做
//	B#1 未命中  → 缓存按账号隔离 → 只能靠会话粘性把同一会话钉在同号
//
// 只打印状态码与 usage 缓存字段，不打印 token 与正文。
//
//	go run internal/probe/crossacct.go -auths ./auths -a 11f49997 -b <uid2>
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type res struct {
	status int
	err    string
	prompt int64
	hit    int64
	miss   int64
}

func (r res) String() string {
	if r.err != "" {
		return fmt.Sprintf("status=%d ERR=%s", r.status, truncate(r.err, 160))
	}
	tot := r.hit + r.miss
	pct := 0.0
	if tot > 0 {
		pct = 100 * float64(r.hit) / float64(tot)
	}
	return fmt.Sprintf("status=%d prompt=%6d hit=%6d miss=%6d hit%%=%5.1f", r.status, r.prompt, r.hit, r.miss, pct)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func main() {
	dir := flag.String("auths", "", "auth 目录")
	aUID := flag.String("a", "", "账号 A 的 uid 前缀")
	bUID := flag.String("b", "", "账号 B 的 uid 前缀")
	model := flag.String("model", "deepseek-v4.1-flash", "模型裸名")
	kb := flag.Int("kb", 40, "长前缀大约多少千 token")
	flag.Parse()
	if *dir == "" || *aUID == "" || *bUID == "" {
		fmt.Println("必须给 -auths -a -b")
		os.Exit(2)
	}
	A := load(*dir, *aUID)
	B := load(*dir, *bUID)
	if A == nil || B == nil {
		fmt.Printf("找不到账号: A=%v B=%v\n", A != nil, B != nil)
		os.Exit(1)
	}
	fmt.Printf("A=%s… realm=%s\nB=%s… realm=%s\nmodel=%s 前缀≈%dk token\n\n",
		A.UID[:8], A.Realm(), B.UID[:8], B.Realm(), *model, *kb)

	msgs := longMessages(*kb)
	c := upstream.New()
	for _, a := range []*auth.Auth{A, B} {
		if err := c.RefreshTokenIfNeeded(a, 10*time.Minute); err != nil {
			fmt.Printf("刷新 token 失败 uid=%s: %v\n", a.UID[:8], err)
			os.Exit(1)
		}
	}
	steps := []struct {
		label string
		acct  *auth.Auth
	}{
		{"A#1 冷启动", A},
		{"A#2 应命中", A},
		{"B#1 跨号试探", B},
		{"B#2 应命中", B},
		{"A#3 回头再打", A},
	}
	var got []res
	for i, s := range steps {
		if i > 0 {
			time.Sleep(1500 * time.Millisecond)
		}
		r := send(c, s.acct, *model, msgs)
		got = append(got, r)
		fmt.Printf("  %-16s %s\n", s.label, r)
	}
	pct := func(r res) float64 {
		if t := r.hit + r.miss; t > 0 {
			return 100 * float64(r.hit) / float64(t)
		}
		return 0
	}
	fmt.Printf("\n结论：同号复打 %.1f%% / 跨号首打 %.1f%% / 跨号复打 %.1f%% / A 回头 %.1f%%\n",
		pct(got[1]), pct(got[2]), pct(got[3]), pct(got[4]))
	switch {
	case pct(got[1]) < 50:
		fmt.Println("→ 同号都没命中，本判定无效（前缀太小、模型不返回缓存字段，或该模型不缓存）")
	case pct(got[2]) >= 50:
		fmt.Println("→ 跨号首打就命中：上游按内容前缀共享缓存，账号无关。跨号缓存已存在，网关无需另做。")
	default:
		fmt.Println("→ 跨号首打未命中：前缀缓存按账号隔离。要提命中率只能靠会话粘性（现网已默认开，TTL 30m）。")
	}
}

func load(dir, prefix string) *auth.Auth {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Println("read auths:", err)
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".bak") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil || a == nil || a.AccessToken == "" {
			continue
		}
		if strings.HasPrefix(a.UID, prefix) || strings.HasPrefix(e.Name(), prefix) {
			a.FilePath = filepath.Join(dir, e.Name())
			return a
		}
	}
	return nil
}

func longMessages(kb int) string {
	var b strings.Builder
	b.WriteString(`[{"role":"system","content":"You are a helpful assistant. 下面是一段无关资料，只作前缀填充用。\n`)
	for b.Len() < kb*3000 {
		b.WriteString("资料条目：网关前缀缓存测试段落，编号无关紧要，仅用于构造足够长的稳定前缀。")
	}
	b.WriteString(`"},{"role":"user","content":"只回复两个字：收到"}]`)
	return b.String()
}

func send(c *upstream.Client, a *auth.Auth, model, msgs string) res {
	body := fmt.Sprintf(`{"model":%q,"stream":true,"messages":%s}`, model, msgs)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	rc, status, respBody, err := c.ChatStreamContext(ctx, a, []byte(body), "", upstream.ChatMeta{}, nil)
	r := res{status: status}
	if err != nil {
		r.err = err.Error()
		if len(respBody) > 0 {
			r.err += " body=" + string(respBody)
		}
		return r
	}
	defer rc.Close()
	raw, _ := io.ReadAll(io.LimitReader(rc, 4<<20))
	var last map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &obj) != nil {
			continue
		}
		if u, ok := obj["usage"].(map[string]any); ok && u != nil {
			last = u
		}
	}
	if last == nil {
		return r
	}
	num := func(v any) int64 {
		switch x := v.(type) {
		case float64:
			return int64(x)
		case json.Number:
			n, _ := x.Int64()
			return n
		}
		return 0
	}
	r.prompt = num(last["prompt_tokens"])
	details, _ := last["prompt_tokens_details"].(map[string]any)
	if details != nil {
		r.hit = num(details["cached_tokens"])
	}
	for _, k := range []string{"cache_hit_tokens", "cached_tokens"} {
		if v := num(last[k]); v > r.hit {
			r.hit = v
		}
	}
	if v := num(last["cache_miss_tokens"]); v > 0 {
		r.miss = v
	} else if r.prompt > r.hit {
		r.miss = r.prompt - r.hit
	}
	return r
}
