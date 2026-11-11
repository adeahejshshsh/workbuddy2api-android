package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 单选框不能被全局的 input{width:100%} 拉成整行（issue #202）。
//
// 背景：index.html 给表单控件统一设了 `input, select { width: 100% }`。单选框一旦
// 落在 `display: inline-flex` 的容器里，作为 flex item 的它同样拿到 100%——于是
// 圆圈被拉到整行宽，文字被挤到下一行（Safari 直接把 radio 压在文字上）。这两处都
// 在容器里放了 radio：`.radios`（配置页三选一）与 `.realm-opt`（添加账号弹窗的
// 站点选择）。前者早就写了 `width:auto; flex:none` 规避，后者漏了，正是本 issue。
//
// 这条测试挡的是「新加一处 radio 容器时又忘写规避」——纯 CSS 的问题 Go 侧和 JS
// 语法检查都发现不了，只能靠盯着规则本身。
//
// 变异判据：把 `.realm-opt input` 里的 width:auto 或 flex:none 删掉，本用例红。
func TestRadioInputsNotStretchedByGlobalWidth(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)

	// 先确认「全局 100% 宽」这条前提还在——它一旦被改掉，下面几条断言的动机就变了，
	// 那时应该重新审视这组规则而不是让测试默默继续通过。
	globalRe := regexp.MustCompile(`(?s)input, select \{[^}]*width: 100%`)
	if !globalRe.MatchString(css) {
		t.Fatal("index.html 的 `input, select` 全局规则里没有 width:100% 了；" +
			"若确实去掉了，请连带删掉本测试与两处单选框的规避规则")
	}

	// 每一处「容器里有 radio」的选择器都必须显式收宽。新加容器时在这里补一行。
	containers := []struct {
		selector string
		why      string
	}{
		{".radios input", "配置页三选一（thinking_mode 等）"},
		{".realm-opt input", "添加账号弹窗的站点选择（issue #202 现场）"},
	}
	for _, c := range containers {
		rule, ok := cssRule(css, c.selector)
		if !ok {
			t.Fatalf("index.html 里找不到规则 %s（%s）", c.selector, c.why)
		}
		if !strings.Contains(rule, "width: auto") {
			t.Errorf("%s 缺少 width:auto —— 会被全局 100%% 撑到整行（%s）：%s",
				c.selector, c.why, rule)
		}
		if !strings.Contains(rule, "flex: none") {
			t.Errorf("%s 缺少 flex:none —— 作为 flex 项仍会被拉伸（%s）：%s",
				c.selector, c.why, rule)
		}
	}

	// 反向覆盖：面板里所有 `<input type="radio">` 都必须**确实**落在上面某个容器内部，
	// 否则它没有任何宽度规避，会以同样的形态错位。
	//
	// 判据是「最近的那个容器开标签之后、到本 radio 之前没出现过对应的闭合标签」。
	// 不能用「前后 N 个字符内出现过 class」近似：一个 .radios 里放三个 radio 时，
	// 第三个距离开标签已经很远了。
	radioRe := regexp.MustCompile(`<input[^>]*type="radio"[^>]*>`)
	for _, m := range radioRe.FindAllStringIndex(css, -1) {
		tag := css[m[0]:m[1]]
		divA := strings.LastIndex(css[:m[0]], `class="radios"`)
		optA := strings.LastIndex(css[:m[0]], `class="realm-opt"`)
		switch {
		case divA < 0 && optA < 0:
			t.Errorf("radio 不在任何收宽容器里，会被全局 100%% 撑开：%s", tag)
		case divA > optA:
			if strings.Contains(css[divA:m[0]], "</div>") {
				t.Errorf("radio 在 .radios 闭合之后，等于没有容器收宽：%s", tag)
			}
		default:
			if strings.Contains(css[optA:m[0]], "</label>") {
				t.Errorf("radio 在 .realm-opt 闭合之后，等于没有容器收宽：%s", tag)
			}
		}
	}
}

// cssRule 取出某个选择器的规则体（`selector { ... }`）。
func cssRule(css, selector string) (string, bool) {
	idx := strings.Index(css, selector+" {")
	if idx < 0 {
		return "", false
	}
	end := strings.Index(css[idx:], "}")
	if end < 0 {
		return "", false
	}
	return css[idx : idx+end+1], true
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
