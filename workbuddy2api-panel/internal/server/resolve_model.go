package server

import "strings"

// modelAliases 兼容别名（与 realm 无关，只替换裸名）：
//
// 官方 DeepSeek 客户端/前端按 "deepseek-flash" 认这个模型，WorkBuddy 目录里叫
// "deepseek-v4.1-flash"。不做映射的话请求体原样转给上游，上游按未知模型拒绝
// （400/404，换号无效——根因是模型名，不是账号）。
//
// 只放**确指同一个模型**的别名，不做模糊/前缀匹配：别名属于协议面，猜错会把请求
// 送去客户端没要的模型，比报错更糟。
var modelAliases = map[string]string{
	"deepseek-flash": "deepseek-v4.1-flash",
}

// aliasModel 把裸模型名换成上游真名（无别名时原样返回）。
func aliasModel(bare string) string {
	if real, ok := modelAliases[bare]; ok {
		return real
	}
	return bare
}

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名，
// 且已过一遍 modelAliases：别名在这里一次替换，下游（选号/限流/倍率/出站重写）
// 全都吃替换后的真名，不会出现「半个请求认别名、半个不认」。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", aliasModel(model)
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", aliasModel(model)
	}
	return prefix, aliasModel(model[idx+1:])
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
