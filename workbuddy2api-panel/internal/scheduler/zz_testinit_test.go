package scheduler

// 测试里把 keepalive 的账号间节流关掉（默认 800ms × 账号数会把用例拖慢几分钟）。
func init() { keepaliveAccountDelay = 0 }
