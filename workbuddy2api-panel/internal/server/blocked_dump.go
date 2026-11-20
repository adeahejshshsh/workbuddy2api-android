package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// blockedDumpMaxBytes 单份快照上限。11128 的触发点是**请求体内的一段连续原文**，
// 而请求体动辄几百 KB（系统提示 + 工具定义 + 历史）；截断到 256KB 足够覆盖
// 「开头 + 最近几轮」，再大只会把磁盘写满而不增加信息量。
const blockedDumpMaxBytes = 256 << 10

// blockedDumpKeep 最多保留几份。这是个排障用的一次性证据，不是审计日志。
const blockedDumpKeep = 3

// dumpBlockedBody 在 11128 最终失败时把**出站请求体**落盘一份。
//
// 为什么需要它：这条错误是上游按「连续原文指纹」判的，而网关能做的只有猜哪一段
// 命中（先换中性提示词、再摘工具描述）。两轮都试过仍然 400 时，光看错误文案
// （"Illegal API invocation from an unapproved channel"）无法定位 —— 必须看
// 实际发出去的字节。
//
// 落盘内容默认是**已经过网关清洗的出站 body**（kind=outbound，来自 ChatMeta.OnOutbound
// 回调，与真正发出去的明文 JSON 逐字节一致）；回调不可用时退回入站原文
// （kind=inbound），并在信封里如实标注 —— 两者的差别是排障的关键：入站里还带着
// 脱敏前的指纹，照着它删段会一路删空也删不掉已经不存在的那段。
// 不含账号凭据（Authorization 是头，不在 body 里）。文件名带时间戳与模型，
// 方便对应到请求日志里那一行。
//
// 目录：<配置目录>/data/blocked/。返回写入路径（失败返回空串，调用方只管记日志）。
func (h *Handler) dumpBlockedBody(model, kind, body string, sanitize bool) string {
	dir := h.blockedDumpDir()
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	raw := []byte(body)
	truncated := false
	if len(raw) > blockedDumpMaxBytes {
		raw = raw[:blockedDumpMaxBytes]
		truncated = true
	}
	stamp := time.Now().Format("20060102-150405.000")
	name := filepath.Join(dir, "blocked-"+stamp+".json")
	// 包一层信封：原文 + 元信息。直接写裸 body 的话，事后分不清是哪个模型/是否截断。
	if kind == "" {
		kind = "outbound"
	}
	env := map[string]any{
		"captured_at": time.Now().Format(time.RFC3339),
		"model":       model,
		// sanitize：出站指纹脱敏开关的实际取值。这是 11128 排障的第一嫌疑：
		// false = 客户端指纹逐字出站，几乎必然 400（线上 2026-11-20 就是这么被
		// 误配置关掉的）。写进信封，看一眼就知道该查哪。
		"sanitize": sanitize,
		// kind：outbound = 真正发出去的（prepareBody 之后）；inbound = 客户端原文。
		// 有这张标签，才不会再出现「快照里有指纹、上游照样 400」那种自相矛盾的判断。
		"kind":      kind,
		"truncated": truncated,
		"body":      string(raw),
	}
	buf, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return ""
	}
	if err := os.WriteFile(name, buf, 0o600); err != nil {
		return ""
	}
	pruneBlockedDumps(dir)
	return name
}

// blockedDumpDir <配置目录>/data/blocked。ConfigPath 为空（测试/嵌入）时返回空 ——
// 不往进程当前目录乱写文件。
func (h *Handler) blockedDumpDir() string {
	if h == nil || h.cfg.BlockedDumpDir == "" {
		return ""
	}
	return h.cfg.BlockedDumpDir
}

// pruneBlockedDumps 只保留最近 blockedDumpKeep 份（按文件名的时间前缀排序，
// 字典序即时间序）。写失败不影响主流程。
func pruneBlockedDumps(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && len(e.Name()) > 8 && e.Name()[:8] == "blocked-" {
			names = append(names, e.Name())
		}
	}
	if len(names) <= blockedDumpKeep {
		return
	}
	// ReadDir 已按文件名升序，删最旧的 len-keep 个。
	for i := 0; i < len(names)-blockedDumpKeep; i++ {
		_ = os.Remove(filepath.Join(dir, names[i]))
	}
}
