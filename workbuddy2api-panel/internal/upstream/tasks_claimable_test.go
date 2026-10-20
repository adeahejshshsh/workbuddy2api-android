package upstream

import (
	"encoding/json"
	"testing"
)

// bug P1-1 回归：一次性任务的完成状态只在 accept_status 上（target=0、progress=null），
// 只看 tgt>0 && cur>=tgt 会让它永远 claimable=false —— 面板不渲染「领取」按钮，
// 而 autotask 各处却按 AcceptStatus=="completed" 判完成，两侧口径不一致。
func TestParseGrowthTasksClaimableForCompletedSingleTask(t *testing.T) {
	raw := json.RawMessage(`{"tasks":[
		{"task_code":"first_buddy","title":"领养 Buddy","accept_status":"completed","target":0,"progress":null},
		{"task_code":"RichMeow_Chat","accept_status":"completed","target":5,"progress":{"current":5,"target":5}},
		{"task_code":"chat_5","accept_status":"accepted","target":5,"progress":{"current":2,"target":5}},
		{"task_code":"chat_claimed","accept_status":"claimed","target":5,"progress":{"current":5,"target":5}}
	]}`)
	tasks, err := parseGrowthTasks(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := map[string]bool{}
	for _, tk := range tasks {
		got[tk.TaskCode] = tk.Claimable
	}
	if !got["first_buddy"] {
		t.Error("accept_status=completed + target=0 应当可领取（一次性任务的主要完成标志）")
	}
	if !got["RichMeow_Chat"] {
		t.Error("进度达标应当可领取")
	}
	if got["chat_5"] {
		t.Error("进度未达标不该可领取")
	}
	if got["chat_claimed"] {
		t.Error("已领取（claimed）不该再可领取")
	}
}
