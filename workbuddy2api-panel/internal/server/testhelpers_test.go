package server

import "time"

// resetDynamicModelsCache 清空动态模型目录缓存。
//
// 上游把它放在 hidden_models_test.go 里；本仓没有那个文件（相关用例并入了
// allowed_models_test.go），但 videos_test 需要它，故单独成文件。
// 任何「先造目录、再断言 /v1/models」的用例都必须先调它，否则会读到上一条用例
// 留下的缓存。
func resetDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}
