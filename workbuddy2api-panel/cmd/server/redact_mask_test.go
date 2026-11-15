package main

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
)

// 面板的掩码字面量与 saveConfig 的「视为未修改」判定必须是同一个串，否则保存配置
// 会把真密钥覆盖成掩码（P0-1 次生项）。panel 包导出该常量供这里对齐。
func TestPanelRedactedValueMatches(t *testing.T) {
	if panel.RedactedValue != panelRedactedValue {
		t.Fatalf("掩码不一致：panel=%q server=%q", panel.RedactedValue, panelRedactedValue)
	}
}
