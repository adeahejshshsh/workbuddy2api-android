package main

// 审计日志的密钥指纹来源（W5-16）。
//
// 审计行里的 key 字段是 SHA-256 前 4 字节（32 bit）。此前用 cfg.APIKey 计算，
// 而 api_key 是发给每个下游客户端的——任何拿到审计日志的人都能离线穷举/验证
// 自己手里的 key，反推出「这条操作是哪个客户端做的」，也能确认某把泄漏的 key
// 是否仍生效。审计指纹应该只来自管理员密钥（admin_api_key）；没配时才回退
// api_key 保持旧行为（此时本来也没有更强的密钥可用）。

// auditLogKey 返回用于审计指纹的密钥：admin_api_key 优先，空则回退 api_key。
func auditLogKey(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	if k := cfg.AdminAPIKey; k != "" {
		return k
	}
	return cfg.APIKey
}
