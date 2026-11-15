package panel

// 第二轮代码审计（CODE-AUDIT-FULL.md 10.18 / H1~H16 中属于 index.html 的部分）
// 的回归用例。
//
// 只做**静态结构断言**（不依赖 app.js）：这些属性一旦被删掉不会报错、肉眼看
// 不出差别（读屏用户与浅色主题用户才会碰到），只能靠用例钉住。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func readIndexHTMLForAudit(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("读 index.html: %v", err)
	}
	return string(raw)
}

// H6：CSS 用 env(safe-area-inset-bottom) 做安全区避让，viewport 缺 viewport-fit=cover
// 时该值恒为 0 —— 刘海/手势条机型上底部导航被压住。
func TestIndexHTMLViewportFitCover(t *testing.T) {
	src := readIndexHTMLForAudit(t)
	m := regexp.MustCompile(`<meta name="viewport" content="([^"]*)"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("缺 viewport meta")
	}
	if !strings.Contains(m[1], "viewport-fit=cover") {
		t.Fatalf("viewport=%q 缺 viewport-fit=cover（safe-area-inset-* 会恒为 0）", m[1])
	}
}

// H1：--ink-3 用于 .k/.hint/.note（11.5~12px 正文），必须达到 4.5:1。
// 这里钉住具体色值：浅色 #8b93a5（2.82:1）与深色 #6b7488（4.13:1）都不达标。
func TestIndexHTMLInk3ContrastTokens(t *testing.T) {
	src := readIndexHTMLForAudit(t)
	for _, bad := range []string{"--ink-3: #6b7488", "--ink-3: #8b93a5"} {
		if strings.Contains(src, bad) {
			t.Errorf("--ink-3 仍是低对比度色值 %q（WCAG 2.2 AA 1.4.3 失败）", bad)
		}
	}
	for _, want := range []string{"--ink-3: #7b8599", "--ink-3: #666e80"} {
		if !strings.Contains(src, want) {
			t.Errorf("缺达标色值 %q", want)
		}
	}
}

// H2：<label> 里不能再嵌 <label>（HTML 非法；读屏会把两层可访问名拼在一起，
// 点说明文字会触发两次激活 = 双击切换、看起来「点了没反应」）。
func TestIndexHTMLLabelsAreNotNested(t *testing.T) {
	// 只看真实标记：<style>/<script>/注释里的示例文本（含本仓自己写的
	// `<label class="fld">` 说明）不是 DOM，不能当嵌套判据。
	src := markupOnly(readIndexHTMLForAudit(t))
	for _, line := range nestedLabelLines(src) {
		t.Fatalf("index.html:%d 存在 label 嵌套 label（HTML 非法）", line)
	}
	// 曾经的两处（思考档位开关 / 档位优先级）现在必须是 div.fld。
	if !strings.Contains(src, `<div class="fld"><span class="lb" id="lbThinkingEnabled">`) {
		t.Error("「启用网关档位策略」应改为 div.fld + aria-labelledby")
	}
	if !strings.Contains(src, `aria-labelledby="lbThinkingEnabled"`) ||
		!strings.Contains(src, `aria-labelledby="lbThinkingClientPriority"`) {
		t.Error("checkbox 缺 aria-labelledby（可访问名会被拼成「标题 + 整段说明」）")
	}
}

// H7 / H9：关键控件不能只有 placeholder / title（placeholder 输入后消失，
// title 的 AT 支持不稳定）。
func TestIndexHTMLLabelledControls(t *testing.T) {
	src := readIndexHTMLForAudit(t)
	want := map[string]string{
		`id="keyInput"`:      "访问密钥输入框",
		`id="ruleName"`:      "规则名",
		`id="ruleMode"`:      "规则模式",
		`id="bhWindow"`:      "余额趋势时间窗口",
		`id="pkSort"`:        "排序方式",
		`id="logExportDays"`: "导出区间",
	}
	for tag, what := range want {
		at := strings.Index(src, tag)
		if at < 0 {
			t.Errorf("找不到 %s", tag)
			continue
		}
		// 取该标签所在的开标签文本（到 '>' 为止）。
		end := strings.Index(src[at:], ">")
		if end < 0 {
			t.Errorf("%s 标签没闭合", tag)
			continue
		}
		tagText := src[at : at+end]
		if !strings.Contains(tagText, "aria-label=") {
			t.Errorf("%s（%s）缺 aria-label", tag, what)
		}
	}
}

// H11：运行日志是本页核心功能，新行不播报等于读屏用户完全感知不到。
func TestIndexHTMLLogBoxIsLiveRegion(t *testing.T) {
	src := readIndexHTMLForAudit(t)
	at := strings.Index(src, `id="logBox"`)
	if at < 0 {
		t.Fatal("找不到 #logBox")
	}
	end := strings.Index(src[at:], ">")
	tagText := src[at : at+end]
	if !strings.Contains(tagText, `aria-live="polite"`) {
		t.Fatalf("#logBox 缺 aria-live: %q", tagText)
	}
}

// H12：API 密钥框是 type=password，密码管理器可能把面板登录密码填进去，
// 误保存就把 config.json 的 api_key 改成了错值（面板自身鉴权随之改变）。
func TestIndexHTMLConfigKeyAutocomplete(t *testing.T) {
	src := readIndexHTMLForAudit(t)
	at := strings.Index(src, `id="cfgKey"`)
	if at < 0 {
		t.Fatal("找不到 #cfgKey")
	}
	end := strings.Index(src[at:], ">")
	tagText := src[at : at+end]
	if !strings.Contains(tagText, `autocomplete="new-password"`) {
		t.Fatalf("#cfgKey 缺 autocomplete=new-password: %q", tagText)
	}
}

// H13：连接状态变化必须能被播报（断线这种关键状态对读屏/色觉障碍用户
// 此前完全不可见：只有一个小圆点变色）。
func TestIndexHTMLNavStateIsStatus(t *testing.T) {
	src := readIndexHTMLForAudit(t)
	at := strings.Index(src, `id="navState"`)
	if at < 0 {
		t.Fatal("找不到 #navState")
	}
	// 往上找到包含它的那个 div 开标签。
	lineStart := strings.LastIndex(src[:at], "<div")
	if lineStart < 0 {
		t.Fatal("定位不到 #navState 的容器")
	}
	lineEnd := strings.Index(src[lineStart:], ">")
	tagText := src[lineStart : lineStart+lineEnd]
	if !strings.Contains(tagText, `role="status"`) {
		t.Fatalf("连接状态容器缺 role=status: %q", tagText)
	}
	// 纯颜色点不该被读屏念出来。
	if pulseAt := strings.Index(src, `id="navPulse"`); pulseAt >= 0 {
		seg := src[pulseAt : pulseAt+strings.Index(src[pulseAt:], ">")]
		if !strings.Contains(seg, `aria-hidden="true"`) {
			t.Errorf("#navPulse 是装饰性色点，应 aria-hidden: %q", seg)
		}
	}
}

// nestedLabelLines 返回「某个 <label> 内部又出现 <label>」的行号（1 基）。
//
// 用深度计数而不是正则：`<label class="fld">…<label class="chk">…` 这种兄弟
// 关系（前一个已闭合）不是嵌套，正则的惰性匹配分不清。
func nestedLabelLines(src string) []int {
	var out []int
	for i := 0; i < len(src); {
		rel := strings.Index(src[i:], "<label")
		if rel < 0 {
			break
		}
		start := i + rel
		depth, k := 0, start
		for k < len(src) {
			no := strings.Index(src[k:], "<label")
			nc := strings.Index(src[k:], "</label>")
			if nc < 0 {
				break
			}
			if no >= 0 && no < nc {
				depth++
				if depth >= 2 {
					out = append(out, strings.Count(src[:start], "\n")+1)
					break
				}
				k += no + len("<label")
				continue
			}
			depth--
			k += nc + len("</label>")
			if depth == 0 {
				break
			}
		}
		i = start + len("<label")
	}
	return out
}

// markupOnly 抹掉 <style>/<script>/HTML 注释的内容（保留换行，行号不变）。
func markupOnly(src string) string {
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?s)<style.*?</style>`),
		regexp.MustCompile(`(?s)<script.*?</script>`),
		regexp.MustCompile(`(?s)<!--.*?-->`),
	} {
		src = re.ReplaceAllStringFunc(src, func(block string) string {
			return strings.Map(func(r rune) rune {
				if r == '\n' {
					return r
				}
				return ' '
			}, block)
		})
	}
	return src
}
