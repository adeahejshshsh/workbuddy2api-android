package server

import "strings"

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// splitRealmPrefixFold 显式识别 `cn:` / `global:` 前缀（忽略大小写）。
//
// 与 resolveModel 的差别：resolveModel 把**裸名**也归到 cn（用于路由，语义正确），
// 而黑名单/展示这类「是否带前缀」的判断必须能区分两者 —— 否则 `qwen:7b` 这种
// 名字本身带冒号的裸模型会被误判成 cn 专属。
func splitRealmPrefixFold(model string) (realm, bare string, ok bool) {
	idx := strings.IndexByte(model, ':')
	if idx <= 0 {
		return "", model, false
	}
	switch strings.ToLower(model[:idx]) {
	case "cn":
		return "cn", model[idx+1:], true
	case "global":
		return "global", model[idx+1:], true
	}
	return "", model, false
}
