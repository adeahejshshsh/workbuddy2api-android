package upstream

import (
	"strings"
	"testing"
)

// BUG-1: cc_entrypoint 必须在特征表里（此前被上一行的注释吞掉）。
func TestSanitizeCcEntrypointFeature(t *testing.T) {
	key := "cc_" + "entrypoint"
	in := "build " + key + "=cli"
	if !hasFingerprint(in) {
		t.Fatal("特征表里缺这一项：只带它的文本不会进净化")
	}
	if out := sanitizeText(in); strings.Contains(out, key) {
		t.Fatalf("没被剥离: %q", out)
	}
}

// BUG-8: progress 逐字段覆盖，target=0 不许覆盖平铺字段的 target。
func TestParseGrowthTasksProgressKeepsFlatTarget(t *testing.T) {
	raw := []byte(`{"tasks":[{"task_code":"t","current":0,"target":10,"progress":{"current":3,"target":0}}]}`)
	tasks, err := parseGrowthTasks(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("解析出 %d 条", len(tasks))
	}
	if tasks[0].Target != 10 {
		t.Fatalf("target = %d，期望保留平铺字段的 10", tasks[0].Target)
	}
	if tasks[0].Current != 3 {
		t.Fatalf("current = %d，期望 3", tasks[0].Current)
	}
}
