package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 面板的排版是**一套绝对 px 刻度**（10 / 10.5 / 11 / 11.5 / 12 / 12.5 / 13 / 13.5 …），
// 上游也是同一套。两类偏离会让某一处字号「突然很突兀」，而它们都不会报错：
//
//	① 相对字号（em / % / rem）：父级一变它就跟着漂，同一段文字在两张表里不一样大；
//	② 只发了 markup、没跟样式：app.js 已经在渲染某个类，index.html 里却没有对应规则，
//	   于是它按 body 的 14px 继承，而周围全是 11.5~12.5px。
//
// 这里把两条都钉住。②只钉**已经踩过坑**的那几处（通用版「所有类都必须有样式」会把
// 纯 hook 类一起判红，噪声太大）。
func TestPanelTypeScaleMatchesUpstream(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(html)
	css := src[strings.Index(src, "<style>")+len("<style>") : strings.Index(src, "</style>")]
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")

	// ① 全表只允许绝对字号：相对字号是「同一段文字在不同父级下不一样大」的唯一来源。
	rel := regexp.MustCompile(`font-size:\s*[.0-9]+(em|rem|%)\b`).FindAllString(css, -1)
	if len(rel) > 0 {
		t.Errorf("面板样式里出现了相对字号 %v：同一段文字会随父级字号漂移，改成绝对 px", rel)
	}

	// ② 已经在渲染、必须有样式的类（每条都对应一次真实缺陷）。
	for _, c := range []struct{ sel, why string }{
		{`.acc td.fl`, `fmtFlowCell 的「今日 / 本月」两行单元格`},
		{`.acc td.fl .fl-row`, `两行单元格的每一行（缺 display:block 会并成一行）`},
		{`.acc td.fl .fl-sub`, `两行单元格的副行（缺了按表格 13px 继承，主次不分）`},
		{`.uschart-hd .legend .lg-name`, `用量图例里的账号名（缺了超长名挤变形卡头）`},
		{`.acc.req td.sample`, `请求记录表的 sample 列（缺了长文本撑爆整张表）`},
	} {
		if !strings.Contains(css, c.sel+" ") && !strings.Contains(css, c.sel+"{") && !strings.Contains(css, c.sel+" {") {
			t.Errorf("样式里缺 %s（%s）", c.sel, c.why)
		}
	}

	// ③ 副行的字号必须落在刻度上，且与同类次要文字一致（表格表头 / 账号 uid 都是 11.5px）。
	m := regexp.MustCompile(`\.acc td\.fl \.fl-sub\s*\{([^}]*)\}`).FindStringSubmatch(css)
	if m == nil {
		t.Fatal("定位不到 .acc td.fl .fl-sub 的规则")
	}
	if !strings.Contains(m[1], "font-size: 11.5px") {
		t.Errorf(".fl-sub 的字号应与表格次要文字一致（11.5px），实际：%s", strings.TrimSpace(m[1]))
	}

	// ④ 域标签的行高：10px 字号上 1 与 1.1 的差别会顶到上下边框（上游是 1.1）。
	if !regexp.MustCompile(`\.realm-tag\s*\{[^}]*font:\s*500 10px/1\.1`).MatchString(css) {
		t.Error(".realm-tag 的字号行高应为 500 10px/1.1（与上游一致）")
	}

	// ⑤ 备注行的「原昵称」是跟随文字：它必须继承父级字号，不能自带相对字号。
	if regexp.MustCompile(`\.rm-ghost\s*\{[^}]*font-size`).MatchString(css) {
		t.Error(".rm-ghost 不该自带字号：它是跟随原昵称的浅色文字，字号跟父级走")
	}

	// ⑥ 兜底：app.js 里 fmtFlowCell 仍在发这套 markup —— 哪天它改成别的类名，
	//    上面 ② 的断言就会守着一个已经不存在的结构。
	if !strings.Contains(string(js), `'<td class="num fl"`) {
		t.Error("app.js 里找不到 fmtFlowCell 的 td.num.fl：②的断言已失去对象，请同步更新")
	}
}
