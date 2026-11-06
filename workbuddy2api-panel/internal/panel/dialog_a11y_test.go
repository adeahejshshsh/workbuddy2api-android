package panel

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 弹窗（.veil）此前只有鼠标能关：Esc 无效、Tab 能跑到背景页面上、关掉后焦点落回 body。
// 这三条都不会报错，肉眼也看不出来（鼠标用户永远碰不到），所以只能靠结构断言钉住。
//
// 断言的是**契约**而不是实现：
//  1. 每个 .veil 里的 .dlg 必须自报家门（role=dialog + aria-modal + aria-labelledby，
//     且被引用的标题 id 真实存在）—— 否则读屏软件只念一堆散装按钮；
//  2. app.js 的 VEIL_CLOSERS 必须覆盖 index.html 里的每一个 .veil —— 新增弹窗却忘了
//     登记，Esc 就对它无效，而这种漏项在鼠标操作下完全看不出来；
//  3. 鉴权门 keyVeil 必须在 VEIL_ESC_LOCKED 里：按一下 Esc 就进不了面板，这不是关闭，
//     是自锁；
//  4. 打开弹窗必须走 openVeil —— 直接 classList.add('on') 会绕过「记住打开者」，
//     关闭后焦点就还回不去。
func TestPanelDialogsAccessibleAndEscapable(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	htmlSrc, jsSrc := string(html), string(js)

	veilRe := regexp.MustCompile(`<div class="veil" id="([A-Za-z0-9_]+)"`)
	veils := []string{}
	for _, m := range veilRe.FindAllStringSubmatch(htmlSrc, -1) {
		veils = append(veils, m[1])
	}
	if len(veils) == 0 {
		t.Fatal("index.html 里一个 .veil 都没找到，用例前提不成立")
	}

	// ① 每个弹窗的 .dlg 都要有 role / aria-modal / aria-labelledby，且标题 id 存在
	for _, id := range veils {
		at := strings.Index(htmlSrc, `<div class="veil" id="`+id+`"`)
		if at < 0 {
			t.Fatalf("定位不到 %s", id)
		}
		rest := htmlSrc[at:]
		if end := strings.Index(rest, `</div>`); end < 0 {
			t.Fatalf("%s 结构不完整", id)
		}
		dlgAt := strings.Index(rest, `class="dlg"`)
		if dlgAt < 0 {
			t.Fatalf("%s 里没有 .dlg", id)
		}
		lineEnd := strings.Index(rest[dlgAt:], ">")
		if lineEnd < 0 {
			t.Fatalf("%s 的 .dlg 标签没闭合", id)
		}
		dlgTag := rest[dlgAt : dlgAt+lineEnd]
		if !strings.Contains(dlgTag, `role="dialog"`) {
			t.Errorf(`%s 的 .dlg 缺 role="dialog"：读屏软件不会把它当弹窗`, id)
		}
		if !strings.Contains(dlgTag, `aria-modal="true"`) {
			t.Errorf(`%s 的 .dlg 缺 aria-modal="true"：读屏软件仍会念到背景内容`, id)
		}
		m := regexp.MustCompile(`aria-labelledby="([A-Za-z0-9_]+)"`).FindStringSubmatch(dlgTag)
		if m == nil {
			t.Errorf(`%s 的 .dlg 缺 aria-labelledby：弹窗没有名字`, id)
			continue
		}
		if !strings.Contains(htmlSrc, `id="`+m[1]+`"`) {
			t.Errorf(`%s 的 aria-labelledby="%s" 指向一个不存在的 id`, id, m[1])
		}
	}

	// ② VEIL_CLOSERS 覆盖所有 .veil
	closers := map[string]bool{}
	closerAt := strings.Index(jsSrc, "const VEIL_CLOSERS = {")
	if closerAt < 0 {
		t.Fatal("app.js 里找不到 VEIL_CLOSERS：Esc 关闭弹窗的登记表没了")
	}
	body := jsSrc[closerAt:]
	if end := strings.Index(body, "\n};"); end >= 0 {
		body = body[:end]
	}
	for _, m := range regexp.MustCompile(`(?m)^\s{2}([A-Za-z0-9_]+):`).FindAllStringSubmatch(body, -1) {
		closers[m[1]] = true
	}
	for _, id := range veils {
		if !closers[id] {
			t.Errorf("%s 没登记进 VEIL_CLOSERS：Esc 对它无效（新增弹窗必须在这里补一行）", id)
		}
	}
	for id := range closers {
		found := false
		for _, v := range veils {
			if v == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("VEIL_CLOSERS 里的 %s 在 index.html 里不存在（改名后忘了同步？）", id)
		}
	}

	// ③ 鉴权门不能被 Esc 关掉
	lockAt := strings.Index(jsSrc, "const VEIL_ESC_LOCKED = new Set([")
	if lockAt < 0 {
		t.Fatal("app.js 里找不到 VEIL_ESC_LOCKED")
	}
	lockLine := jsSrc[lockAt:]
	if end := strings.Index(lockLine, "])"); end >= 0 {
		lockLine = lockLine[:end]
	}
	if !strings.Contains(lockLine, "'keyVeil'") {
		t.Error("keyVeil 不在 VEIL_ESC_LOCKED 里：按一下 Esc 就进不了面板（自锁）")
	}

	// ④ 打开弹窗必须走 openVeil（直接 add('on') 会丢掉「打开者」，焦点还回不去）
	if strings.Contains(jsSrc, "Veil').classList.add('on')") {
		t.Error("还有弹窗直接 classList.add('on')：请改走 openVeil(id, focusSel)，否则关闭后焦点回不到原控件")
	}

	// ⑤ 关闭路径要还焦点：每个 closer 最终都得走到 restoreFocus（openVeil 记的账才有人还）
	for _, id := range veils {
		if !closers[id] {
			continue
		}
		if !strings.Contains(jsSrc, "restoreFocus('"+id+"')") && !strings.Contains(jsSrc, "restoreFocus(id)") {
			t.Errorf("%s 的关闭路径没有 restoreFocus：焦点会落回 body", id)
		}
	}

	// ⑥ 弹窗打开时锁住背景滚动：手机上手势会穿透，弹窗开着还能把背后列表滑走。
	//    锁必须在 open 与 close 两处都维护，否则关掉一个弹窗就把另一个弹窗的锁也解了。
	if !strings.Contains(htmlSrc, "body.veil-open { overflow: hidden; }") {
		t.Error("index.html 缺 body.veil-open 的滚动锁样式")
	}
	for _, fn := range []string{"function openVeil(", "function restoreFocus("} {
		at := strings.Index(jsSrc, fn)
		if at < 0 {
			t.Fatalf("app.js 里找不到 %s", fn)
		}
		body := jsSrc[at:]
		if end := strings.Index(body, "\n}\n"); end >= 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "syncVeilScroll()") {
			t.Errorf("%s 没有调用 syncVeilScroll()：背景滚动锁会漏维护", strings.TrimSuffix(fn, "("))
		}
	}

	// ⑦ toast 要有 live region：否则「已保存 / 已复制」只对看得见的人有效。
	if !strings.Contains(htmlSrc, `id="toasts" role="status" aria-live="polite"`) {
		t.Error(`#toasts 缺 role="status" aria-live="polite"：读屏软件不会播报操作结果`)
	}

	sort.Strings(veils)
	t.Logf("覆盖的弹窗：%s", strings.Join(veils, ", "))
}
