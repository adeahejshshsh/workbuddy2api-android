package panel

import (
	"os"
	"strings"
	"testing"
)

// TestAppJSGatewayEffortCell 模型行内「网关档位」下拉的形状：
// 选项原样取该模型 supported_efforts（可关思考的模型再补 off），不做「最高档（x）」
// 之类的合成文案；默认选中支持的最高档（= 缺省 default_effort=max 的语义）；
// 显示当前实际生效档；策略关闭时置灰；上游固定档（没有 supported_efforts）也给一个
// 单选项表示网关跟随该档（不可调）；出图与完全没有档位声明的模型不渲染下拉。
func TestAppJSGatewayEffortCell(t *testing.T) {
	nodeBin(t)
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function mdRowHtml');
const end = src.indexOf('function renderModels');
if (start < 0 || end < 0) throw new Error('mdRowHtml region not found');
const ctx = {
  esc: s => String(s),
  rateCell: () => 'RATE',
  outCell: () => '<td>OUT</td>',
  mdAvailabilityTag: () => '',
  // 本仓 mdRowHtml 额外带了 1017 版本徽标与工具/视觉/思考常开标签的判据，沙箱补齐桩。
  is1017Model: () => false,
  BUSY_WORDS: { busy: '繁忙', congested: '拥堵', unavailable: '不可用' },
};
vm.createContext(ctx);
vm.runInContext("'use strict';\n" + src.slice(start, end) + '\nthis.mdRowHtml=mdRowHtml;', ctx);
const base = { id: 'cn:glm-5.2', name: 'GLM', supported_efforts: ['xhigh', 'high', 'medium'], default_effort: 'high', can_disable_thinking: true, supports_reasoning: true, context_length: 200000 };
const html = m => ctx.mdRowHtml(Object.assign({}, base, m), null);
const sel = html({ gateway_effort: 'xhigh', gateway_policy_enabled: true });
const offSel = html({ gateway_effort: 'off', gateway_policy_enabled: true });
const fixed = html({ supported_efforts: [], default_effort: 'medium', can_disable_thinking: false });
process.stdout.write(JSON.stringify({
  hasSelect: sel.includes('data-effort-id="cn:glm-5.2"'),
  topOption: sel.includes('<option value="xhigh" selected>xhigh</option>'),
  offOption: sel.includes('<option value="off">off</option>'),
  offSelected: offSel.includes('<option value="off" selected>off</option>'),
  tiers: (sel.match(/<option value="/g) || []).length,
  noSynthetic: !sel.includes('最高档') && !sel.includes('max（最高）'),
  disabled: html({ gateway_effort: 'high', gateway_policy_enabled: false }).includes('disabled'),
  fixedCell: fixed.includes('<option value="medium" selected>medium</option>') && fixed.includes('disabled'),
  noImage: html({ image_generation: true }).includes('data-effort-id'),
  noTier: html({ supported_efforts: [], supports_reasoning: true, default_effort: '' }).includes('data-effort-id'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "gateway-effort-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := nodeCmd(f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("gateway effort node test failed: %v\n%s", err, out)
	}
	const want = `{"hasSelect":true,"topOption":true,"offOption":true,"offSelected":true,"tiers":4,"noSynthetic":true,"disabled":true,"fixedCell":true,"noImage":false,"noTier":false}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("gateway effort=%s want %s", strings.TrimSpace(string(out)), want)
	}
}
