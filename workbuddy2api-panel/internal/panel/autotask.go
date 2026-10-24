// autotask.go 面板「一键完成」任务的动作实现。
//
// 设计依据：上游 scripts/task_*.py 实测结论 + 2026-09-12 桌面指纹协议逆向
// （data/desktop-task-protocol.md）——
//   - first_buddy（+300 分）：report（前置解锁）→ agreement → buddy/first
//   - chat_5（+100 分）：累计 5 条 chat_request_send 上报
//   - Model_chat_GLM5.2（+100 分）：accept → 用 glm-5.2 真实对话一次 → 上报（模型字段对齐）
//   - RichMeow_Chat：桌面指纹（workbuddy-desktop）完整对话事件链，纯 API 可点亮（三账号实测）
//   - Buddy_App / Buddy_App_QQ：buddyapp 五连事件，纯 API 可点亮（两账号实测）
//   - automation_1：automated_task_create_suc 事件，纯 API 可点亮（两账号实测）
//   - Library_read：web 域 web_element_click(library_doc_intro_click)（三账号实测）
//   - template_5 / playbook_prompt / create_canvas：asar 逆向出的判据事件
//     （template_used / playbook_prompt_send / wbx_design_canvas_*），纯 API 可点亮（三账号实测）
//   - expert_5 / Expert_team_use_3：真实专家列表 + 召唤链 + 真实 chat（服务端 requestId）
//   - expert_actual_use（三账号实测）
//   - expert_use_* 系列（中国电商运营专家 / 抖音策略师 / 商家宣传页与海报设计师 / 直播电商教练）：
//     判据同 Expert_lighthouse，专家 id 取自任务 jump_url
//   - Hp_Appearance：appearance/set + appearance_skin_apply 事件（两账号实测）
//
// 仍未破解：skill_1（疑似要求真实 Skill 工具调用）。
// 不做：Expert_lighthouse（需真实连接器授权）、Expert_Philanthropy（真实捐款）。
//
// 所有动作幂等：已 claimed/已达标的任务直接跳过，不重复消耗上游配额。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// autoAction 一个可自动化的任务动作。
type autoAction struct {
	TaskCode string   // 目标任务 code
	Desc     string   // 展示用说明
	Attempt  bool     // true = 尝试型（上游未证实可脚本化，跑了可能不点亮）
	Covers   []string // 同一次上报顺带满足的其它任务码（如 Buddy_App / Buddy_App_QQ）
	run      func(p *Panel, a *auth.Auth) (string, error)
}

// autoActions 已实现的任务动作表（顺序即执行顺序：先解锁依赖项）。
// first_buddy 依赖活跃上报解锁，故 chat_5/first_buddy 的执行都自带 report 步骤。
var autoActions = []autoAction{
	{
		TaskCode: "chat_5",
		Desc:     "上报 5 条对话活跃事件（自动补足差额）",
		run:      runChat5,
	},
	{
		TaskCode: "first_buddy",
		Desc:     "上报解锁 → 同意协议 → 领取第一只 Buddy（+300 分）",
		run:      runFirstBuddy,
	},
	{
		TaskCode: "Model_chat_GLM5.2",
		Desc:     "接受任务 → glm-5.2 真实对话一次 → 对齐模型上报",
		run:      runModelChat,
	},
	{
		TaskCode: "RichMeow_Chat",
		Desc:     "桌面指纹事件链上报（已验证：纯 API 可点亮，三账号实测）",
		run:      runRichMeow,
	},
	{
		TaskCode: "Buddy_App",
		Covers:   []string{"Buddy_App_QQ"}, // 共用 runBuddyApp：一次五连事件两个任务都算
		Desc:     "上报「进入 Buddy 应用」事件链（已验证：纯 API 可点亮）",
		run:      runBuddyApp,
	},
	{
		TaskCode: "Buddy_App_QQ",
		Desc:     "上报「进入企鹅教师助手」事件链（已验证：纯 API 可点亮）",
		run:      runBuddyApp,
	},
	{
		TaskCode: "automation_1",
		Desc:     "上报「定时任务创建」事件（已验证：纯 API 可点亮）",
		run:      runAutomationCreate,
	},
	{
		TaskCode: "Library_read",
		Desc:     "上报「读资料库介绍」事件（已验证：纯 API 可点亮）",
		run:      runLibraryRead,
	},
	{
		TaskCode: "template_5",
		Desc:     "上报「使用模板创建任务」事件组 ×5（已验证：三账号点亮）",
		run:      runTemplateUse,
	},
	{
		TaskCode: "playbook_prompt",
		Desc:     "上报「灵感案例做同款发送 Prompt」事件组（已验证：三账号点亮）",
		run:      runPlaybookPrompt,
	},
	{
		TaskCode: "create_canvas",
		Desc:     "上报「设计创意画布创建」事件组（已验证：三账号点亮，+300 分）",
		run:      runCreateCanvas,
	},
	{
		TaskCode: "expert_5",
		Desc:     "真实专家召唤+使用链 ×5（专家市场列表+真实 chat，已验证：三账号点亮）",
		run:      runExpertUse,
	},
	{
		TaskCode: "Expert_team_use_3",
		Desc:     "真实专家团召唤+使用链 ×3（已验证：三账号点亮）",
		run:      runExpertTeamUse,
	},
	{
		TaskCode: "Hp_Appearance",
		Desc:     "设置主题 API + 皮肤生效事件（已验证：两账号点亮）",
		run:      runAppearance,
	},
	{
		TaskCode: "skill_1",
		Desc:     "真实对话 + skill_info 技能加载事件（已验证：人杰2 点亮）",
		run:      runSkillFresh,
	},
	{
		TaskCode: "Expert_lighthouse",
		Desc:     "真实轻量云专家召唤+使用链（chat 链带 has_expert，已验证：两账号点亮）",
		run:      runExpertLighthouse,
	},
	{
		TaskCode: "WorkBuddy_Valorant",
		Desc:     "体验「无畏契约拼豆专家」（指定专家 id，按域区分国服/国际服）：召唤链 + 真实对话 + expert_actual_use → 领奖（+100c+5e）",
		run:      runValorantExpert,
	},
	{
		TaskCode: "expert_use_maidehao",
		Desc:     "中国电商运营专家召唤+使用链（专家页搜索口径；活动截止 2026-11-20，+50c+5e）",
		run:      runExpertExperienceFor("expert_use_maidehao"),
	},
	{
		TaskCode: "expert_use_douyinyin",
		Desc:     "抖音策略师召唤+使用链（专家页搜索口径；活动截止 2026-11-20，+50c+5e）",
		run:      runExpertExperienceFor("expert_use_douyinyin"),
	},
	{
		TaskCode: "expert_use_shangjia_xuanchuan",
		Desc:     "商家宣传页与海报设计师召唤+使用链（专家页搜索口径；活动截止 2026-11-20，+50c+5e）",
		run:      runExpertExperienceFor("expert_use_shangjia_xuanchuan"),
	},
	{
		TaskCode: "expert_use_bowangwang",
		Desc:     "直播电商教练召唤+使用链（专家页搜索口径；活动截止 2026-11-20，+50c+5e）",
		run:      runExpertExperienceFor("expert_use_bowangwang"),
	},
	{
		TaskCode: "black_cat",
		Desc:     "夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示稍后再试）",
		Attempt:  true,
		run:      runBlackCat,
	},
	{
		TaskCode: "school_season",
		Desc:     "校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）",
		run:      runSchoolSeason,
	},
	{
		TaskCode: "Sequential_Tasks_1",
		Desc:     "小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）",
		run:      runSequentialChat,
	},
	{
		TaskCode: "Sequential_Tasks_2",
		Desc:     "小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）",
		run:      runMiniExpert,
	},
	{
		TaskCode: "Sequential_Tasks_3",
		Desc:     "小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）",
		run:      runSequentialChat5,
	},
	{
		TaskCode: "Sequential_Tasks_4",
		Desc:     "小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件（PC 同源）→ 领奖（判据待解锁验证）",
		Attempt:  true, // 判据未验证：跑了可能不点亮 → 默认不进待办（v1.22.0）
		run:      runSequentialAutomation,
	},
	{
		TaskCode: "Sequential_Tasks_5",
		Desc:     "小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）",
		Attempt:  true, // 同上
		run:      runSequentialModelChat,
	},
	{
		TaskCode: "Sequential_Tasks_6",
		Desc:     "小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖",
		Attempt:  true, // 同上
		run:      runSequentialChat10,
	},
	{
		TaskCode: "Sequential_Tasks_7",
		Desc:     "体验灵感功能（预留，疑 PC 口径 +500c+5e）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）",
		Attempt:  true, // 同上
		run:      runSequentialPlaybook,
	},
}

// autoActionFor 查任务对应的动作；无则返回 nil（不可自动化）。
func autoActionFor(code string) *autoAction {
	for i := range autoActions {
		// 大小写不敏感（BUG-13）：taskMapByCode 用 ToLower 建键、runAutoAll 走那张表，
		// 而这里用 == 精确比较 —— 上游一旦改大小写，队列扫描判「没有该动作」、
		// runAutoAll 却仍命中，两个入口行为分叉。
		if strings.EqualFold(autoActions[i].TaskCode, strings.TrimSpace(code)) {
			return &autoActions[i]
		}
	}
	return nil
}

// effectiveTaskCode 返回动作**实际**要读回/领奖的任务码（bug #19）。
//
// 少数任务在不同域可能用不同 code（目前只有 WorkBuddy_Valorant）：动作内部会按
// 名称兜底换码，但 accountTaskAuto 的回读与领奖判定仍用动作表里的固定 code，
// 于是回读返回 nil（progress_after="?"）、自动领奖失效，用户看到"执行成功但进度未知"。
// 这里统一解析一次，动作与调用方共用同一口径。
func (p *Panel) effectiveTaskCode(a *auth.Auth, code string) string {
	if a == nil || p.cfg.Upstream == nil || p.cfg.Pool == nil {
		return code
	}
	m, err := p.loadTaskMap(a)
	if err != nil || m == nil {
		return code
	}
	return effectiveTaskCodeIn(m, code)
}

// loadTaskMap 一次拉齐任务列表（默认口径 + mp 合并）并建 code → Task 映射（bug-2）。
//
// 为什么需要：runAutoAll 有 19 个动作，每个动作都会调 effectiveTaskCode ——
// 而它内部先 taskByCode（1 次 ListTasks）再 ListTasks（第 2 次），主流程又
// taskByCode 一次，单账号一轮 ≈ 38~57 次 GET /v2/activity/growth/tasks。
// 一次拉齐后每个动作只查本地 map。
func (p *Panel) loadTaskMap(a *auth.Auth) (map[string]*upstream.Task, error) {
	if a == nil || p.cfg.Upstream == nil {
		return nil, nil
	}
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return nil, err
	}
	// mp 列表是默认口径的超集（实测含常规任务）：合并去重，失败静默（与
	// accountTasks 同口径）。
	if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
		seen := make(map[string]bool, len(tasks)+len(mpTasks))
		for i := range tasks {
			seen[strings.ToLower(tasks[i].TaskCode)] = true
		}
		for _, t := range mpTasks {
			if k := strings.ToLower(t.TaskCode); !seen[k] {
				tasks = append(tasks, t)
				seen[k] = true
			}
		}
	}
	return taskMapByCode(tasks), nil
}

// taskMapByCode 建 code（小写）→ *Task 映射。
func taskMapByCode(tasks []upstream.Task) map[string]*upstream.Task {
	m := make(map[string]*upstream.Task, len(tasks))
	for i := range tasks {
		m[strings.ToLower(tasks[i].TaskCode)] = &tasks[i]
	}
	return m
}

// effectiveTaskCodeIn 在给定映射里解析实际任务码（纯函数，不发请求）。
func effectiveTaskCodeIn(m map[string]*upstream.Task, code string) string {
	want := strings.ToLower(code)
	if t, ok := m[want]; ok && t != nil {
		return t.TaskCode // 精确同名（大小写不敏感）：保留上游的原始大小写
	}
	// 名称特征兜底（仅对已知会换码的任务生效，避免误抓别的任务）
	// 名称特征兜底必须是**确定性**的（bug 5）：此前直接 range map，Go 的 map 迭代序
	// 是随机的 —— 账号下同时存在两个 valorant/perler 相关码（跨域变体、活动期新旧码
	// 并存）时，同一个任务点两次可能解析成不同的 code，表现为「一次成功一次报没有
	// 此任务」，且无法稳定复现。
	if strings.Contains(want, "valorant") {
		keys := make([]string, 0, len(m))
		for k := range m {
			if m[k] != nil {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		// 先按关键词的固定优先级找，同优先级内按 key 字典序取第一条
		for _, wantKey := range []string{"valorant", "perler"} {
			for _, k := range keys {
				if strings.Contains(k, wantKey) {
					return m[k].TaskCode
				}
			}
		}
	}
	return code
}

// autoActionIndex 任务在 autoActions 中的顺序（队列执行按依赖序排；未知返回大值）。
func autoActionIndex(code string) int {
	for i := range autoActions {
		// 与 autoActionFor 同口径（报告 #16）：大小写不同也要命中，否则该码恒落
		// 1<<20（排到最后），队列执行顺序与依赖序不符。
		if strings.EqualFold(autoActions[i].TaskCode, code) {
			return i
		}
	}
	return 1 << 20
}

// mpTaskCodes 小程序口径专属下发的成长任务：默认（无 mp 头）列表不出现，
// accept/claim 均要求 X-Client-Platform: miniprogram。新任务出现时在此登记。
var mpTaskCodes = map[string]bool{
	"school_season":      true, // 校园日（mini chat + activityId）
	"Sequential_Tasks_1": true, // 小程序首对话（mini chat，无 activityId）
	"Sequential_Tasks_2": true, // 小程序选中专家并完成有效对话（mp 指纹 expert_actual_use）
	"Sequential_Tasks_3": true, // 小程序完成 5 次对话（与 Tasks_1 同形状，target=5 逐条累加）
	// Tasks_4..7 存在性已实测（accept 返回 prerequisite not met 链式依赖；Tasks_8 not
	// found 封顶）。链条每日零点解锁一环（task locked until 次日），判据为 issue #42
	// 描述 + mpsrc 事件形状预置，解锁后逐个实测校正。
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

// isMPTaskCode 报告任务是否小程序口径专属（决定回读/接受/领奖走 mp 变体）。
func isMPTaskCode(code string) bool { return mpTaskCodes[code] }

// taskByCode 拉取任务列表并定位单个任务；未找到返回 nil（不视为错误）。
// 双口径：mp 专属任务在默认列表查不到，自动回落 mp 列表（仅对已登记的 mp 码，
// 未知码不多打一次上游）。
// taskDone 任务是否「已完成到不必再跑」：已领取奖励，或已达目标但还没领取。
// 单任务入口与一键全部此前口径不一致 —— 单任务只跳过 Claimed，于是「已达标未领」
// 时点一键完成会把整条动作链重跑一遍（expert_5 是 5 次真实对话 + 5 组召唤事件），
// 白烧配额与时间。两个入口共用这一个判定，避免再分叉。
func taskDone(t *upstream.Task) bool {
	if t == nil {
		return true
	}
	// Claimable 已含「进度达标」与「accept_status=completed」两种完成形态（bug-05）：
	// 上游解析侧（upstream/tasks.go）把它们统一成同一个结论，这里再用
	// Target>0 && Current>=Target 自己判一遍，就把"已完成未领"的一次性任务漏成了
	// "没做完" → 整条动作链重跑（expert_5 是 5 次真实对话 + 5 组召唤事件）。
	return t.Claimed || t.Claimable
}

// refreshCreditsAsync 领奖成功后异步刷新该号积分（bug #17）。
//
// 为什么需要：ClaimReward 只把本次到账的 credit/energy 写进响应，池内 credits 不动，
// 面板上的积分要等下一次余额刷新（默认 5 分钟）或手动点「余额」才变 —— 用户会以为
// "领了但没到账"。这里后台拉一次 UserResourceDetailedWithExpiry 并写回池内。
func (p *Panel) refreshCreditsAsync(a *auth.Auth) {
	if a == nil || p.cfg.Upstream == nil || p.cfg.Pool == nil {
		return
	}
	go func() {
		// panic 兜底（bug #1）：这是纯观测性刷新，绝不能因为它打死网关。
		defer func() {
			if r := recover(); r != nil {
				log.Printf("WARN: [panel] refreshCredits uid=%s panic: %v", a.UID, r)
			}
		}()
		remain, total, expiring, earliestAt, earliestRemaining, err :=
			p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
		if err != nil {
			return
		}
		p.cfg.Pool.SetCreditsDetailed(a.UID, remain, total, expiring, earliestAt, earliestRemaining)
	}()
}

// errTaskAborted 任务被中止（客户端断连 / HTTP 超时）。
var errTaskAborted = errors.New("task aborted: client gone or request timed out")

func (p *Panel) taskByCode(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		// 忽略大小写（报告 #15）：autoActionFor 用 EqualFold，这里精确比较会让
		// 「上游改一下大小写」时单任务入口判「没有此任务」，而队列扫描仍能命中。
		if strings.EqualFold(tasks[i].TaskCode, code) {
			return &tasks[i], nil
		}
	}
	if isMPTaskCode(code) {
		return p.taskByCodeMP(a, code)
	}
	return nil, nil
}

// claimPollAttempts / claimPollGap 达标回读的有界轮询参数。
// 背景：上游计分是**异步**的——行为事件上报后进度要数秒才刷新（实测 Model_chat
// 对话完成后立即回读仍是 0/1，约 5-8 秒后才变 1/1）。一次性回读会误判"未达标"，
// 从而跳过自动领奖。这里最多轮询 N 次、每次间隔 gap，总预算约 12 秒。
var (
	claimPollAttempts = 4
	claimPollGap      = 3 * time.Second
)

// taskByCodeWaiting 回读任务，若未达标则在有界预算内轮询等待（上游异步计分）。
// 已达标（claimable）立即返回；预算耗尽返回最后一次结果（可能仍未达标）。
func (p *Panel) taskByCodeWaiting(a *auth.Auth, code string) (*upstream.Task, error) {
	return p.taskByCodeWaitingWith(a, code, p.taskByCode)
}

// taskByCodeWaitingMP 小程序口径的同一套有界轮询（bug #6）。
//
// 为什么需要：上游计分是异步的（实测 3~8s），非 mp 任务走 taskByCodeWaiting 会
// 轮询等它落定，而 mp 分支此前只读一次 —— 于是「一键完成全部」里 mp 任务几乎总是
// claimable=false、奖励不会自动领，单任务入口反而能领到，两个入口行为不一致。
func (p *Panel) taskByCodeWaitingMP(a *auth.Auth, code string) (*upstream.Task, error) {
	return p.taskByCodeWaitingWith(a, code, p.taskByCodeMP)
}

// taskByCodeWaitingWith 回读 + 有界轮询的公共实现（fetch 决定用哪种口径读任务列表）。
func (p *Panel) taskByCodeWaitingWith(a *auth.Auth, code string, fetch func(*auth.Auth, string) (*upstream.Task, error)) (*upstream.Task, error) {
	t, err := fetch(a, code)
	if err != nil || t == nil {
		return t, err
	}
	if t.Claimable || t.Claimed {
		return t, nil
	}
	for i := 1; i < claimPollAttempts; i++ {
		if !sleepAB(a.UID, claimPollGap) {
			return nil, errTaskAborted
		}
		t2, err2 := fetch(a, code)
		if err2 != nil {
			return t, nil // 轮询期间的查询失败不覆盖已拿到的结果
		}
		if t2 != nil {
			t = t2
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}
	return t, nil
}

// taskByCodeMP 以小程序口径拉取任务列表并定位单个任务；未找到返回 nil。
// 小程序限定任务（school_season / Sequential_Tasks_1）在默认口径列表不出现。
func (p *Panel) taskByCodeMP(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasksMP(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		// 与 taskByCode 同口径（报告 #13）：精确比较时上游改一次大小写，
		// mp 专属任务在单任务/队列路径会全部变成「无此任务」。
		if strings.EqualFold(tasks[i].TaskCode, code) {
			return &tasks[i], nil
		}
	}
	return nil, nil
}

// acceptWithVerifyMP accept 并回读验证登记生效：上游存在 200+OK 但 accept 未真正
// 登记的形态（此时上报事件全部不归账，任务永远点不亮，上游 task_runner c793ae3
// 实测）——判定以回读 accept_status 为准，未生效重试一次。
func (p *Panel) acceptWithVerifyMP(a *auth.Auth, code string) bool {
	for attempt := 1; attempt <= 2; attempt++ {
		if err := p.cfg.Upstream.AcceptTasksMP(a, []string{code}); err != nil {
			log.Printf("autotask %s %s: accept 尝试%d: %v", logfmt.Label(a.UID, a.NicknameValue()), code, attempt, err)
			continue
		}
		if !sleepAB(a.UID, mpActionGap) {
			return false
		}
		t, err := p.taskByCodeMP(a, code)
		if err == nil && t != nil && t.AcceptStatus != "not_accepted" && t.AcceptStatus != "" {
			return true
		}
		log.Printf("autotask %s %s: accept 尝试%d 未登记生效（回读=%q）", logfmt.Label(a.UID, a.NicknameValue()), code, attempt, acceptStatusOr(t))
	}
	return false
}

// acceptStatusOr 安全读取任务 accept_status（nil 任务返回 "?"）。
func acceptStatusOr(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.AcceptStatus == "" {
		return "?"
	}
	return t.AcceptStatus
}

// mpActionGap mp 任务写动作间隔（accept/上报/领奖之间，防频控）。
var mpActionGap = 2 * time.Second

// mpChatEventGap mp 对话事件（chat_request_send）的真人节奏间隔。上游对
// Sequential_Tasks_3「5 次有效对话」有反作弊校验：数秒级连发的事件会先被计入
// 进度（回读 5/5、accept_status 甚至短暂转 completed），随后被判定无效整体回滚
// （进度回落、claim 返回 400 "task not completed"）——2026-09-26 实测 2s 连发
// 4 条全灭，45s 间隔逐条上报全存活且 claim +300c+5e 成功。每条上报前
// sleep gap + 0~10s 抖动；首条也等（上一轮残留进度被回滚后立即重报同样无效）。
var mpChatEventGap = 45 * time.Second

// runMPMiniChatTask growth 域小程序限定任务通用闭环：
// mp 查询 → accept（带登记回读验证）→ mini chat 事件上报（withActivityId 决定
// 是否带开学季 activityId：school_season 必带，Sequential_Tasks_1 不带——服务端按
// source=mini_program 指纹关联）→ 回读 → 达标即领奖。
func (p *Panel) runMPMiniChatTask(a *auth.Auth, code string, withActivityId bool) (string, error) {
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp 口径未下发该任务（活动可能已结束）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "accept 未登记生效（上游 200+OK 但未落账形态），待下次重试", nil
		}
		// accept 前的任务进度为 null（target 下发 0），兜底 target=1 会少报——
		// Tasks_6 首轮实测：accept 后真实 target=10，只补 1 条就误判达标去领奖
		// （claim 400 task not completed）。接受后回读一次拿真实 target/current。
		if t2, err := p.taskByCodeMP(a, code); err == nil && t2 != nil {
			t = t2
		}
	}
	// 已达标（含 completed 未领）：直接领奖。
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, already, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		// 内层就完成了领奖，外层判 claimable 的 refreshCreditsAsync 走不到（bug B13），
		// 面板积分要等 5 分钟才动 —— 这里补一次。
		if already {
			// 幂等重复领取：上游明示 already_claimed，本次没有新增奖励 —— 不能回
			// 「已领取奖励（+Nc +Ne）」，否则前端会提示有新增积分。
			return "该奖励此前已领取（本次无新增）", nil
		}
		p.refreshCreditsAsync(a)
		return fmt.Sprintf("已领取奖励（+%dc +%de）", credit, energy), nil
	}
	// 判据上报：按差额补 mini chat 事件。每条前 sleep mpChatEventGap+抖动——
	// 连发会被上游反作弊判无效（见 mpChatEventGap 注释），宁可慢不可白报。
	need := target - t.Current
	for i := int64(0); i < need; i++ {
		// 可中断（issue ISSUE-11）：这是全流程最长的等待（45~55s/条 × 最多 10 条）。
		// 客户端断连后应尽快退出，别把 8 分钟的流水线跑完还占着账号锁。
		if !sleepAB(a.UID, mpChatEventGap+time.Duration(rand.Int64N(int64(10*time.Second)))) {
			return "", errTaskAborted
		}
		conv := session.NewConversationID()
		var ev map[string]any
		if withActivityId {
			ev = upstream.SchoolSeasonChatEvent(conv)
		} else {
			ev = upstream.SchoolChatTimesEvents(conv)
		}
		if err := p.cfg.Upstream.ReportMPEvent(a, ev); err != nil {
			return fmt.Sprintf("完成 %d/%d 次上报后中断: %v", i, need, err), nil
		}
	}
	// 回读（异步计分，有界轮询复用 claimPoll 预算的紧凑版：两轮各隔 3s）。
	for i := 0; i < 2; i++ {
		if !sleepAB(a.UID, claimPollGap) {
			return "", errTaskAborted
		}
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		// accept 之后 target 可能被上游改写（报告 #14）：target 是在 accept **之前**
		// 取的，不回读就会按旧值判达标，提前领奖撞 400 task not completed。
		if t.Target > 0 {
			target = t.Target
		}
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
	}
	if t.Claimed {
		return "本轮已入账（claimed）", nil
	}
	if t.Current < target {
		return fmt.Sprintf("已上报 %d 次但进度未达 %d/%d（异步计分未归账，下次重试）", need, t.Current, target), nil
	}
	credit, energy, already, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	if !already { // 内层领奖后刷新积分（bug B13）
		p.refreshCreditsAsync(a)
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%dc +%de）", credit, energy), nil
}

// runSchoolSeason 完成 school_season「校园日」（growth 域小程序限定）。
// 判据 = mini chat_request_send + activityId=school_open_day_2026（无 activityId
// 不点亮，与 school 域开学季同活动关联；上游 task_runner e2e 实测 +100c+5e）。
func runSchoolSeason(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "school_season", true)
}

// runSequentialChat 完成 Sequential_Tasks_1「小程序内完成 1 次有效对话」。
// 判据 = mini chat_request_send（无 activityId，服务端按 source=mini_program
// 指纹关联；上游 task_runner 实测 +100c+5e）。
func runSequentialChat(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_1", false)
}

// runSequentialChat5 完成 Sequential_Tasks_3「在小程序内完成 5 次有效对话」。
// 判据与 Sequential_Tasks_1 同形状（mini 指纹 chat_request_send，无 activityId），
// 仅 target=5——服务端按上报条数累加进度，但**要求真人节奏**：连发事件先计数
// 后被反作弊回滚（claim 400 "task not completed"），由 mpChatEventGap 间隔保证
// （2026-09-26 实测：45s 间隔补满 5/5 → claim +300c+5e 成功，领后 accept_status
// =claimed 稳定不回滚）。
func runSequentialChat5(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_3", false)
}

// runSequentialChat10 完成 Sequential_Tasks_6「在小程序内完成 10 次有效对话」（预留）。
// 判据假定与 Tasks_1/3 同形状（mini chat_request_send），target 由任务自带（回读），
// runMPMiniChatTask 按差额补报——issue #42 称 target=10，以解锁后实际下发为准。
// 真人节奏间隔同样适用（mpChatEventGap）：9 条 × ~50s ≈ 8 分钟/账号，夜间队列可接受。
func runSequentialChat10(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_6", false)
}

// runSequentialEventTask Sequential 链预留任务通用骨架：mp 查询 → accept（带验证）
// → 判据事件上报（primary；未点亮且 fallback 非空时补一轮）→ 回读 → 达标领奖。
// 每日零点解锁一环：locked 期间 accept 不落账，返回等下次调度（无需人工干预）。
func (p *Panel) runSequentialEventTask(a *auth.Auth, code string, primary, fallback func() error) (string, error) {
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp 口径未下发该任务（前置任务未完成或活动未开始）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, already, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		// 内层就完成了领奖，外层判 claimable 的 refreshCreditsAsync 走不到（bug B13），
		// 面板积分要等 5 分钟才动 —— 这里补一次。
		if !already {
			p.refreshCreditsAsync(a)
		}
		if already {
			// 幂等重复领取（报告 #15）：上游明示 already_claimed，本次**没有**新增奖励。
			// 此前照样回「已领取奖励（+Nc +Ne）」，前端据此提示有新增积分。
			return "该奖励此前已领取（本次无新增）", nil
		}
		return fmt.Sprintf("已领取奖励（+%dc +%de）", credit, energy), nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "accept 未登记生效（任务可能处于每日锁定窗口，等解锁后自动重试）", nil
		}
		// accept 前 progress 为 null、target 兜底成 1；不回读就会「上报 1 条即判达标」
		// → ClaimRewardMP 撞 400 task not completed → 队列记一次「无效」，连续 3 次
		// 进跳过名单（bug 5）。与 runMPMiniChatTask 的 accept 后回读同口径。
		if t2, rerr := p.taskByCodeMP(a, code); rerr == nil && t2 != nil {
			t = t2
			if t.Target > 0 {
				target = t.Target
			}
		}
	}
	if err := primary(); err != nil {
		return fmt.Sprintf("判据上报失败: %v", err), nil
	}
	// 回读（两轮各隔 3s）；未点亮且有 fallback 时补报一轮再读。
	for round := 0; round < 2; round++ {
		if !sleepAB(a.UID, claimPollGap) {
			return "", errTaskAborted
		}
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		// accept 之后 target 可能被上游改写（报告 #14）：target 是在 accept **之前**
		// 取的，不回读就会按旧值判达标，提前领奖撞 400 task not completed。
		if t.Target > 0 {
			target = t.Target
		}
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
		if round == 0 && fallback != nil {
			if err := fallback(); err != nil {
				return fmt.Sprintf("备选判据上报失败: %v", err), nil
			}
		}
	}
	if t.Claimed {
		return "本轮已入账（claimed）", nil
	}
	if t.Current < target {
		return "已上报但进度未点亮（判据形态待解锁后校正，下次重试）", nil
	}
	credit, energy, already, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	if !already { // 内层领奖后刷新积分（bug B13）
		p.refreshCreditsAsync(a)
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%dc +%de）", credit, energy), nil
}

// runSequentialAutomation 完成 Sequential_Tasks_4「创建定时任务」（预留）。
// mp 源码无 automation 事件发射点 → 判据疑为 PC 口径：复用 automation_1 同源
// 事件（DesktopAutomationCreateEvent，PC 任务三账号实测点亮）。
func runSequentialAutomation(p *Panel, a *auth.Auth) (string, error) {
	return p.runSequentialEventTask(a, "Sequential_Tasks_4",
		func() error {
			return p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopAutomationCreateEvent("自动化"))
		}, nil)
}

// runSequentialModelChat 完成 Sequential_Tasks_5「使用 GLM5.2」（预留）。
// primary：mp 对话事件带 requestModelId/requestModelName=glm-5.2（mpsrc main 32904
// 发射点实测形状）；fallback：PC 域模型活跃上报（Model_chat_GLM5.2 同源）。
func runSequentialModelChat(p *Panel, a *auth.Auth) (string, error) {
	return p.runSequentialEventTask(a, "Sequential_Tasks_5",
		func() error {
			conv := session.NewConversationID()
			return p.cfg.Upstream.ReportMPEvent(a, upstream.MiniChatModelEvent(conv, "glm-5.2", "GLM-5.2"))
		},
		func() error {
			return p.cfg.Upstream.ReportChatActivityModel(a, session.NewConversationID(), "", "glm-5.2", "GLM-5.2")
		})
}

// runSequentialPlaybook 完成 Sequential_Tasks_7「体验灵感功能」（预留，疑 PC 口径）。
// primary：PC 灵感事件组（DesktopPlaybookPromptSequence，playbook_prompt 三账号
// 实测点亮）；fallback：mp 指纹灵感事件组（MiniPlaybookEvents，mpsrc 形状）。
func runSequentialPlaybook(p *Panel, a *auth.Auth) (string, error) {
	return p.runSequentialEventTask(a, "Sequential_Tasks_7",
		func() error {
			conv := session.NewConversationID()
			req := session.NewMessageID()
			return p.cfg.Upstream.ReportDesktopEvent(a,
				upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")...)
		},
		func() error {
			return p.cfg.Upstream.ReportMPEvent(a, upstream.MiniPlaybookEvents("pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")...)
		})
}

// runMiniExpert 完成 Sequential_Tasks_2「在小程序内选中专家并完成有效对话」。
// 判据 = mp 指纹 expert_actual_use（**不带** activityId/conversationId、
// extVersion=2.2.8、type=send_message——小程序源码实测形状，与 school 域 expert
// 事件两套口径勿混；上游 task_runner 实测上报即 completed，claim +200c+5e）。
// 专家 id 必须是市场真实 ex_ id（空 id 服务端不入账）→ **accept 之前**先解析市场
// 列表：拉不到就整任务不动作，避免留下「已登记未上报」的半程态（上游 9a26ae7
// 的 ids 前置判定同款）。复用既有 MarketExpertList（expert_5 任务同源，实测可用）。
func runMiniExpert(p *Panel, a *auth.Auth) (string, error) {
	const code = "Sequential_Tasks_2"
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp 口径未下发该任务（活动可能已结束）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1 // 未 accept 的 mp 任务 progress 为 null，target 兜底（上游实测）
	}
	// 已达标（含 completed 未领）：直接领奖。
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, already, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		// 内层就完成了领奖，外层判 claimable 的 refreshCreditsAsync 走不到（bug B13），
		// 面板积分要等 5 分钟才动 —— 这里补一次。
		if !already {
			p.refreshCreditsAsync(a)
		}
		if already {
			// 幂等重复领取（报告 #15）：上游明示 already_claimed，本次**没有**新增奖励。
			// 此前照样回「已领取奖励（+Nc +Ne）」，前端据此提示有新增积分。
			return "该奖励此前已领取（本次无新增）", nil
		}
		return fmt.Sprintf("已领取奖励（+%dc +%de）", credit, energy), nil
	}
	// 判据载体前置（accept 之前）：市场真实专家 id。
	experts, merr := p.cfg.Upstream.MarketExpertList(a, "")
	if merr != nil || len(experts) == 0 {
		return fmt.Sprintf("专家市场不可用（%v），跳过以防半程态", merr), nil
	}
	e := experts[0]
	name := e.DisplayNameZH
	if name == "" {
		name = e.ProfessionZH
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "accept 未登记生效（上游 200+OK 但未落账形态），待下次重试", nil
		}
	}
	ev := upstream.MiniExpertUseEvent(e.ExpertID, name, e.ExpertType)
	if err := p.cfg.Upstream.ReportMPEvent(a, ev); err != nil {
		return fmt.Sprintf("上报 expert_actual_use 失败: %v", err), nil
	}
	// 回读（异步计分，两轮各隔 3s——与 runMPMiniChatTask 同预算）。
	for i := 0; i < 2; i++ {
		if !sleepAB(a.UID, claimPollGap) {
			return "", errTaskAborted
		}
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		// accept 之后 target 可能被上游改写（报告 #14）：target 是在 accept **之前**
		// 取的，不回读就会按旧值判达标，提前领奖撞 400 task not completed。
		if t.Target > 0 {
			target = t.Target
		}
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
	}
	if t.Claimed {
		return "本轮已入账（claimed）", nil
	}
	if t.Current < target {
		return "已上报但进度未归账（异步计分，下次重试）", nil
	}
	credit, energy, already, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	if !already { // 内层领奖后刷新积分（bug B13）
		p.refreshCreditsAsync(a)
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%dc +%de）", credit, energy), nil
}

// accountTaskAuto 一键完成单个任务：执行对应动作 → 回读进度 → 汇报结果。
func (p *Panel) accountTaskAuto(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUIDForGrowth(w, uid)
	if a == nil {
		return
	}
	// 国际版账号无成长任务体系（bug-01）：与 accountTasks / 调度器 D4 门控同口径。
	if a.IsGlobal() {
		writeErr(w, http.StatusNotImplemented, "国际版账号无成长任务体系（上游 growth 仅对个人 CN 用户开放）")
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	limitJSONBody(w, r) // 面板 JSON 接口请求体上限 1MB（M6）
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TaskCode == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	act := autoActionFor(body.TaskCode)
	if act == nil {
		writeErr(w, http.StatusNotImplemented,
			"该任务需要客户端内交互（无对应接口），无法自动完成；请按任务说明在官方客户端操作")
		return
	}
	// 被判"做不了"的任务不再白跑一遍（v1.22.0）：直接说清原因，避免"点了没效果"。
	if act.Attempt && !p.attemptAllowed() {
		writeErr(w, http.StatusConflict,
			"该任务是尝试型（上游未证实可点亮），默认不执行；如要试请 POST /panel/api/tasks/skip 带上 {\"allow_attempt\":true} 打开开关")
		return
	}
	// 跳过名单的 key 是 `uid|实际任务码`（bug-06）：runAutoAll 与队列路径都用
	// effectiveTaskCode 写/读，这里若用动作表里的固定码查，按域换码的任务
	// （WorkBuddy_Valorant）永远命中不了 → 被判"做不了"的任务照样重跑一遍。
	// 顺带：effectiveTaskCode 内部会 ListTasks，放在 tryLockAccount 之前也避免了
	// "拿着账号锁打上游"。
	effCode := p.effectiveTaskCode(a, act.TaskCode)
	{
		// a 由 accountByUID 保证非 nil，这里不再判空（BUG-23：原先的 if a != nil 恒真，
		// 反而让人以为这里允许 nil）
		if blocked, why := p.skipBlocked(a.UID, effCode); blocked {
			writeErr(w, http.StatusConflict, "该任务已被判定做不了并跳过："+why+
				"；如要立即重试：打开该账号的「任务」弹窗，点「清空跳过名单」")
			return
		}
	}
	// per-account 互斥：同账号的任务动作正在跑（单任务或全量）时直接 409，
	// 不并发重跑（动作幂等但 expert/skill 系含真实对话，重跑浪费配额）。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	defer p.unlockAccount(uid)
	// 断连即中止（bug #7）：单任务入口此前没有 ctx、也不打中止标记，而它调用的
	// runMPMiniChatTask 用的是 sleepAB(uid, ...) —— 标记只有 auto_all 会设，
	// 于是客户端断开后流水线照样跑满（mp 45~55s/条 × 最多 10 条 ≈ 8 分钟）并一直
	// 持着 per-account 锁，用户重试全撞 409。这里补上与 auto_all 同构的看护。
	clearAborted(uid)
	stopWatch := watchAbort(r.Context().Done(), uid)
	defer stopWatch()
	// 前置读取：已完成的任务直接跳过（幂等，不浪费上游调用）。
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	// 用**实际**任务码做前置读取（bug B6）：动作可能按域换码（WorkBuddy_Valorant），
	// 此前用固定码查不到就直接 404 返回。effCode 已在上面（跳过名单查询处）算好。
	before, err := p.taskByCode(a, effCode)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	if before == nil {
		writeErr(w, http.StatusNotFound, "该账号没有此任务")
		return
	}
	isMP := isMPTaskCode(effCode)
	if taskDone(before) {
		// 「已达标但没领」要先领一次再返回（bug #6）：runAutoAll 对同一状态会走
		// claim，单任务入口却直接回"无需再跑"，奖励挂在那儿只能去列表手动点领取。
		if !before.Claimed {
			var credit, energy int64
			var already bool
			var cerr error
			if isMP {
				credit, energy, already, cerr = p.cfg.Upstream.ClaimRewardMP(a, effCode)
			} else {
				credit, energy, already, cerr = p.cfg.Upstream.ClaimReward(a, effCode)
			}
			progress := taskProgressText(before)
			if cerr != nil {
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": true,
					"message": "该任务已达标（" + progress + "）但领奖失败：" + cerr.Error()})
				return
			}
			if already {
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": true,
					"message": "该任务已达标（" + progress + "），奖励此前已领取"})
				return
			}
			p.refreshCreditsAsync(a)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": true, "claimed": true,
				"credit": credit, "energy": energy,
				"message": "该任务已达标（" + progress + "），已自动领奖 +" +
					fmt.Sprintf("%d 分 +%d 能", credit, energy)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": true,
			"message": "该任务已领取过奖励"})
		return
	}
	msg, err := act.run(p, a)
	if err != nil {
		if errors.Is(err, errTaskAborted) {
			writeErr(w, http.StatusGatewayTimeout, "客户端已断开或等待超时，任务已中止（未继续占用账号锁）")
			return
		}
		writeErr(w, http.StatusBadGateway, "执行失败: "+err.Error())
		return
	}
	// 回读验证：上报 200 ≠ 计分（上游可能静默丢弃 + 计分异步），
	// 用有界轮询等异步计时落定，再决定是否自动领奖（mp 任务同口径回读）。
	var after *upstream.Task
	var aerr error
	if isMP {
		// mp 也要有界轮询（bug 6）：上游计分是异步的（3–8s），只读一次几乎必然
		// claimable=false → 单任务入口永远不自动领奖，而「一键完成全部」用的是
		// taskByCodeWaitingMP、能领到 —— 同一个任务两个入口行为不一致。
		after, aerr = p.taskByCodeWaitingMP(a, effCode)
	} else {
		after, aerr = p.taskByCodeWaiting(a, effCode)
	}
	// 回读被中止要如实回 504（bug B13）：此前 aerr != nil → after == nil →
	// progress_after=""、claimable=false，但最终仍 writeJSON(200, ok:true)，
	// 前端 toast 打「已执行」，用户以为成功，实际流水线已被中止。
	if errors.Is(aerr, errTaskAborted) {
		writeErr(w, http.StatusGatewayTimeout, "客户端已断开或等待超时，任务已中止（进度未回读）")
		return
	}
	progressBefore, progressAfter := taskProgressText(before), ""
	claimable := false
	if aerr == nil && after != nil {
		progressAfter = taskProgressText(after)
		claimable = after.Claimable
	}
	resp := map[string]any{
		"ok":              true,
		"message":         msg,
		"progress_before": progressBefore,
		"progress_after":  progressAfter,
		// 回读失败（报告 #12）：progress_after 是空串，前端用
		// `before !== after` 判「有进展」会把空串判成进展并弹绿色。
		// 显式标记，让前端按「进度未知」渲染。
		"progress_unknown": aerr != nil,
		"claimable":        claimable,
		"attempt":          act.Attempt,
		"verify_supported": true,
	}
	// 达标即自动领奖（Web 端 claim）：把"完成→领奖"收敛成一步，无需用户再点一次。
	if claimable {
		var credit, energy int64
		var already bool
		var cerr error
		if isMP {
			credit, energy, already, cerr = p.cfg.Upstream.ClaimRewardMP(a, effCode)
		} else {
			credit, energy, already, cerr = p.cfg.Upstream.ClaimReward(a, effCode)
		}
		if cerr == nil {
			// 成功 = 没有错误（bug #6）：道具类奖励就是 0 分 0 能，不能拿金额判失败。
			// already 只用来决定文案。
			resp["claimed"] = true
			resp["credit"] = credit
			resp["energy"] = energy
			if already {
				resp["message"] = msg + "；奖励此前已领取"
			} else {
				p.refreshCreditsAsync(a) // 领奖后刷新池内积分（bug #17）
				if credit > 0 || energy > 0 {
					resp["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
				} else {
					resp["message"] = msg + "；已领取（该奖励为道具/次数，不含积分与能量）"
				}
			}
		} else {
			resp["claim_error"] = cerr.Error()
			resp["message"] = msg + "；达标但领奖失败，可在任务列表手动点「领取」重试"
		}
	}
	log.Printf("panel: 任务动作 uid=%s code=%s progress %s -> %s claimable=%v claimed=%v",
		uid, act.TaskCode, progressBefore, progressAfter, claimable, resp["claimed"])
	writeJSON(w, http.StatusOK, resp)
}

// taskProgressText 任务进度的可读表示（回读对比用）。
func taskProgressText(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}
	if t.Claimed {
		return "claimed"
	}
	return t.AcceptStatus
}

// truncateStr 截断错误文本（避免把上游长响应原样透给前端）。
func truncateStr(s string, n int) string {
	// rune 边界（bug P2-2）：按字节切中文会出半个字符，进 fmt.Errorf → JSON 后变 \ufffd
	if len(s) <= n {
		return s
	}
	return logfmt.Truncate(s, n) + "…"
}

// ---------------------------------------------------------------------------
// 各任务动作实现
// ---------------------------------------------------------------------------

// reportGap 连续上报之间的间隔（对齐上游脚本实测的 1.05s 口径，避免风控）。
var reportGap = 1050 * time.Millisecond

// runChat5 补足 chat_5 的进度：按差额上报 chat_request_send。
func runChat5(p *Panel, a *auth.Auth) (string, error) {
	t, err := p.taskByCode(a, "chat_5")
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("任务不存在")
	}
	target := t.Target
	if target <= 0 {
		target = 5
	}
	need := target - t.Current
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	for i := int64(0); i < need; i++ {
		cid := session.NewConversationID()
		if err := p.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			return fmt.Sprintf("上报第 %d/%d 条失败: %v", i+1, need, err), nil
		}
		if i < need-1 {
			if !sleepAB(a.UID, reportGap) {
				return "", errTaskAborted
			}
		}
	}
	return fmt.Sprintf("已补报 %d 条对话事件", need), nil
}

// runFirstBuddy 领养：report（解锁前置）→ agreement → first。
func runFirstBuddy(p *Panel, a *auth.Auth) (string, error) {
	if err := p.cfg.Upstream.ReportChatActivity(a, session.NewConversationID(), ""); err != nil {
		return "", fmt.Errorf("前置上报: %w", err)
	}
	if !sleepAB(a.UID, reportGap) { // 给上游事件处理留时间（脚本实测口径）
		return "", errTaskAborted
	}
	if err := p.cfg.Upstream.BuddyAgreement(a); err != nil {
		return "", fmt.Errorf("同意协议: %w", err)
	}
	if err := p.cfg.Upstream.BuddyFirst(a); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			return "前置已上报，但领养门槛未过（上游要求当日活跃），请稍后重试", nil
		}
		return "", fmt.Errorf("领取 Buddy: %w", err)
	}
	return "已领取 Buddy（+300 分 +8 能量）", nil
}

// runModelChat 完成 Model_chat_GLM5.2：accept → 真实对话 → 对齐模型上报。
func runModelChat(p *Panel, a *auth.Auth) (string, error) {
	const code, modelID, modelName = "Model_chat_GLM5.2", "glm-5.2", "GLM-5.2"
	// 1. accept（报名；失败不阻塞——行为事件才是判据）
	if err := p.cfg.Upstream.AcceptTasks(a, []string{code}); err != nil {
		log.Printf("panel: accept %s: %v（继续走行为链路）", code, err)
	}
	if !sleepAB(a.UID, reportGap) {
		return "", errTaskAborted
	}
	// 2. 真实对话一次（判据的最直接证据）
	body, _ := json.Marshal(map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{"role": "user", "content": "hi，请回复一句话"},
		},
		"stream": true,
	})
	rc, status, respBody, err := p.cfg.Upstream.ChatStream(a, body, "", upstream.ChatMeta{})
	if err != nil {
		return "", fmt.Errorf("对话请求: %w", err)
	}
	if status >= 400 {
		rc.Close()
		return "", fmt.Errorf("对话失败 http=%d: %s", status, truncateStr(string(respBody), 160))
	}
	// 读干 SSE（网关对上游强制 flow：不读完会残留连接）
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	rc.Close()
	if !sleepAB(a.UID, reportGap) {
		return "", errTaskAborted
	}
	// 3. 对齐模型的上报（触发进度）
	if err := p.cfg.Upstream.ReportChatActivityModel(a, session.NewConversationID(), "", modelID, modelName); err != nil {
		return "对话已完成，但进度上报失败：" + err.Error(), nil
	}
	return "已完成 glm-5.2 对话并上报", nil
}

// runRichMeow 完成 RichMeow_Chat（桌面端对话1次）。
// 2026-09-12 三账号实测验证：以桌面指纹（extName=workbuddy-desktop）向
// copilot.tencent.com/v2/report 上报完整对话事件链（agent_task_created →
// chat_message_response isSuccessful=true 等 6 事件），纯 API 即可点亮并领奖
// （紫川/人杰2 两账号无桌面客户端登录状态下 3 秒内 0/1 → 1/1）。
// Hp_Appearance 的纯 API set 不计分（需客户端在主题下活跃），区别对待。
func runRichMeow(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := session.NewConversationID()
	req := session.NewMessageID()
	msg := fmt.Sprintf("req-%d-user", ms)
	events := upstream.DesktopChatSequence(conv, req, msg, "fast-model", "fast-model")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已按桌面端指纹上报完整对话事件链（agent_task_created→chat_response）", nil
}

// runBuddyApp 完成 Buddy_App / Buddy_App_QQ（进入 Buddy 应用）。
// 2026-09-12 两账号实测：buddyapp 五连事件（discover→show→enter→auth_confirm→
// bind_skip）以 workbuddy-desktop 指纹上报即点亮，服务端不校验真实授权。
// 用企鹅教师助手（Buddy_App_QQ 判据应用）作载体，同一组事件同时满足
// Buddy_App「进入任一应用」——两个表项共用本 run，幂等由任务状态跳过兜底。
func runBuddyApp(p *Panel, a *auth.Auth) (string, error) {
	events := upstream.DesktopBuddyAppSequence("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 buddyapp 进入五连事件（同时覆盖 Buddy_App 与 Buddy_App_QQ）", nil
}

// runAutomationCreate 完成 automation_1（设置自动化任务）。
// 2026-09-12 两账号实测：automated_task_create_suc 事件纯 API 上报即点亮，
// 无需真实创建定时任务。
func runAutomationCreate(p *Panel, a *auth.Auth) (string, error) {
	if err := p.cfg.Upstream.ReportDesktopEvent(a,
		upstream.DesktopAutomationCreateEvent("自动化")); err != nil {
		return "", err
	}
	return "已上报定时任务创建事件", nil
}

// runLibraryRead 完成 Library_read（体验资料库）。
// 2026-09-12 三账号实测：web 域 /v2/report 上报 web_element_click
// (elementId=library_doc_intro_click) 即点亮（space 的 open/WS/inlong 都不是判据）。
func runLibraryRead(p *Panel, a *auth.Auth) (string, error) {
	const docURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if err := p.cfg.Upstream.ReportWebEvent(a, "web_element_click", docURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", err
	}
	return "已上报资料库介绍阅读事件", nil
}

// runBlackCat 完成 black_cat（夜猫子，夜间 23:00–08:00 计数）。
// 判据 = 夜间窗口内 glm-5.2 真实对话 + chat 事件上报（WorkBuddy-Daily 实测口径）。
// 窗口外不做（提示等排程）；网关 blackcat_hours（默认 23 点）排程会自动补足。
func runBlackCat(p *Panel, a *auth.Auth) (string, error) {
	// 窗口按北京时间判（misakano7545 `025cc107`）：上游的 23:00–08:00 是北京时间的
	// 夜间窗口，按宿主时区判会在 UTC 主机上整体错位 8 小时。
	if !upstream.InNightWindow(cst.Now()) {
		return "当前不在 23:00–08:00 计数窗口，行为不计分；网关会在每日 23 点自动补足", nil
	}
	need, err := p.cfg.Upstream.BlackcatNeed(a)
	if err != nil {
		return "", err
	}
	if need <= 0 {
		return "进度已达标，无需补足", nil
	}
	ok, err := p.cfg.Upstream.RunNightChats(a, int(need))
	if err != nil {
		return fmt.Sprintf("完成 %d/%d 次后中断: %v", ok, need, err), nil
	}
	return fmt.Sprintf("已完成 %d 次夜间对话并上报", ok), nil
}

// runSkillFresh 完成 skill_1（尝鲜热门技能）。
// 2026-09-12 判据（紫川手动完成抓包 row 209）：`skill_info` 事件（桌面指纹）——
// {id:<技能名>, skillId, skillVersion, toolStatus:"success", fileCount,
// source:"workbuddy-desktop"} JOIN 真实会话（conversationId/requestId=服务端
// id）。此前的 skill_request_send/skill_installed/skill_action 全是错误方向。
// 人杰2 实测 0/1 → 1/1 点亮。
func runSkillFresh(p *Panel, a *auth.Auth) (string, error) {
	conv, req, err := p.cfg.Upstream.DesktopChatWithExpert(a, "")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	msgID := "msg-" + upstream.Tail8(req)
	events := upstream.DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls" // 模型发起工具调用（技能加载）语义
		}
	}
	events = append(events, upstream.DesktopEvent{
		"eventCode":      "skill_info",
		"id":             "润泽小馆·日报撰写",
		"skillId":        "skill_2097350077599879168",
		"skillVersion":   "1.0.0",
		"toolStatus":     "success",
		"fileCount":      56,
		"source":         "workbuddy-desktop",
		"conversationId": conv, "requestId": req, "messageId": msgID,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"traceId": req,
	})
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件: %w", err)
	}
	return "已上报真实对话 + skill_info 技能加载事件", nil
}

// runExpertLighthouse 完成 Expert_lighthouse（体验「腾讯轻量云」专家）。
// 2026-09-12 判据（真实样本 Sunny row 868）：与 expert_5 同构，但两处差异——
// chat 链的 agent_task_created 需带 has_expert:true + expert_id（我们默认 false），
// expert_actual_use 的 mode 为 "LOCAL"（非 craft）。id 固定为轻量云专家
// ex_2cvvUZQhDyeJ；requestId 必须是真实 chat 的服务端 id。紫川/人杰2 实测点亮。
func runExpertLighthouse(p *Panel, a *auth.Auth) (string, error) {
	const lhID = "ex_2cvvUZQhDyeJ"
	lh := upstream.MarketExpert{
		ExpertID: lhID, ExpertType: "agent",
		DisplayNameZH: "腾讯轻量云专家", ProfessionZH: "腾讯轻量云专家", Version: "1.0.2",
	}
	// 市场列表若命中真实条目则用其信息（version 等以服务端为准）。
	if experts, err := p.cfg.Upstream.MarketExpertList(a, "agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == lhID {
				lh = e
				break
			}
		}
	}
	if err := p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	conv, req, err := p.cfg.Upstream.DesktopChatWithExpert(a, lhID)
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+upstream.Tail8(req), "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	events = append(events, upstream.DesktopExpertActualUseLocal(lh, conv, req))
	// 对齐真实样本细节：轻量云专家 actual_use 的 type 为空、cost=0。
	events[len(events)-1]["type"] = ""
	events[len(events)-1]["cost"] = 0
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("使用事件: %w", err)
	}
	return "已上报轻量云专家召唤+使用链（真实对话 requestId）", nil
}

// WorkBuddy_Valorant：体验「无畏契约拼豆专家」（+100c+5e，截止 11-15）。
//
// 与 expert_5 的区别：那个是「任意 5 个专家各用一次」，这个是**指定某一个专家**
// （拼豆/无畏契约主题）。判据同样是 expert_actual_use，但要求：
//   - 专家 id 必须是该域市场里真实存在的 ex_ id（编造/跨域 id 不计数）；
//   - requestId 必须是真实 chat 的服务端 id（自造 UUID 不计数）。
//
// 国服/国际服必须分开：两套专家市场（webBase 按账号 realm 切），id 与 version 都可能
// 不同。已知国服 id 见 valorantExpertIDs，国际服按名称在各自市场里匹配 —— 匹配不到
// 就明确报错，绝不拿国服 id 去打国际服（那样只会白跑一次且不留痕迹）。
var valorantExpertIDs = map[string]string{
	"cn":     "ex_xQHHD1u5Jkua", // agent_name=valorant-perler-expert, version 1.2.1
	"global": "",                // 国际服 id 未实测：留空 → 走名称匹配
}

// valorantNameKeys 市场列表的名称兜底匹配关键词（小写比较，命中任一即可）。
var valorantNameKeys = []string{"无畏契约", "valorant", "perler", "拼豆"}

// resolveValorantExpert 解析本域应使用的专家条目。
// 优先：本域已知 id 在市场列表里命中（用服务端下发的 version/名称）；
// 其次：按名称关键词匹配（跨域 id 不同时靠它兜底）；
// 最后：市场拉不到时退回「已知 id + 硬编码元数据」（与 Expert_lighthouse 同策略）。
func resolveValorantExpert(p *Panel, a *auth.Auth) (upstream.MarketExpert, error) {
	realm := a.Realm()
	wantID := valorantExpertIDs[realm]
	fallback := upstream.MarketExpert{
		ExpertID: wantID, ExpertType: "agent",
		DisplayNameZH: "无畏契约拼豆专家", ProfessionZH: "无畏契约拼豆专家", Version: "1.2.1",
	}
	experts, err := p.cfg.Upstream.MarketExpertList(a, "agent")
	if err != nil || len(experts) == 0 {
		if wantID == "" {
			return fallback, fmt.Errorf("realm=%s 专家市场不可用（%v）且没有已知专家 id", realm, err)
		}
		return fallback, nil // 市场拉不到：按已知 id 硬上（信息仍以本地兜底为准）
	}
	if e, ok := matchValorantExpert(experts, wantID); ok {
		return e, nil
	}
	return fallback, fmt.Errorf("realm=%s 的专家市场里没找到「无畏契约拼豆专家」（期望 id=%q）：国服/国际服 id 不通用，需在对应域确认", realm, wantID)
}

// matchValorantExpert 在给定市场列表里定位拼豆专家：先按 id 精确命中，
// 再按名称关键词兜底（跨域 id 不同时靠它）。纯函数，便于单测。
func matchValorantExpert(experts []upstream.MarketExpert, wantID string) (upstream.MarketExpert, bool) {
	if wantID != "" {
		for _, e := range experts {
			if e.ExpertID == wantID {
				return e, true
			}
		}
	}
	for _, e := range experts {
		hay := strings.ToLower(e.DisplayNameZH + " " + e.ProfessionZH)
		for _, k := range valorantNameKeys {
			if strings.Contains(hay, k) {
				return e, true
			}
		}
	}
	return upstream.MarketExpert{}, false
}

// runValorantExpert 完成 WorkBuddy_Valorant。
func runValorantExpert(p *Panel, a *auth.Auth) (string, error) {
	// 国服/国际服的任务码可能不同：统一走 effectiveTaskCode（与调用方回读同口径）。
	code := p.effectiveTaskCode(a, "WorkBuddy_Valorant")
	t, err := p.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "该账号未下发此任务（活动未开放 / 已结束 / 该域没有）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1 // 未 accept 的任务 progress 可能为 null，target 兜底
	}
	// 已达标（含 completed 未领）：直接领奖。
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, already, cerr := p.cfg.Upstream.ClaimReward(a, code)
		if cerr != nil {
			return "已达标但领奖失败: " + cerr.Error(), nil
		}
		if already {
			return "奖励此前已领取", nil
		}
		if credit == 0 && energy == 0 {
			return "已领取（该奖励为道具/次数，不含积分与能量）", nil
		}
		// already 已在上面提前 return（报告 #7），此处只剩「确实新领到」一条路。
		return fmt.Sprintf("已领取奖励（+%dc +%de）", credit, energy), nil
	}
	// 先 accept（PC 口径任务：未报名时上报事件不计数）。
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if aerr := p.cfg.Upstream.AcceptTasks(a, []string{code}); aerr != nil {
			return "accept 失败: " + aerr.Error(), nil
		}
		if !sleepAB(a.UID, reportGap) {
			return "", errTaskAborted
		}
	}
	e, rerr := resolveValorantExpert(p, a)
	if rerr != nil {
		return rerr.Error(), nil
	}
	// 召唤链（web_element_click + summon_click + summoned）。
	if rerr = p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(e)...); rerr != nil {
		return "召唤链上报失败: " + rerr.Error(), nil
	}
	if !sleepAB(a.UID, expertSummonGap) {
		return "", errTaskAborted
	}
	// 真实 chat（带 X-Expert-Id）→ 服务端 requestId。
	conv, req, cerr := p.cfg.Upstream.DesktopChatWithExpert(a, e.ExpertID)
	if cerr != nil {
		return "真实对话失败: " + cerr.Error(), nil
	}
	if len(req) < 8 {
		return "服务端未返回 requestId，判据不成立（跳过上报以免白跑）", nil
	}
	// 使用事件（JOIN 服务端 requestId）+ chat 链。
	events := append(upstream.DesktopChatSequence(conv, req, "msg-"+upstream.Tail8(req), "fast-model", "fast-model"),
		upstream.DesktopExpertActualUseEvent(e, conv, req))
	if rerr = p.cfg.Upstream.ReportDesktopEvent(a, events...); rerr != nil {
		return "使用事件上报失败: " + rerr.Error(), nil
	}
	// 回读（异步计分）：两轮各隔 3s，达标即领奖。
	for i := 0; i < 3; i++ {
		if !sleepAB(a.UID, claimPollGap) {
			return "", errTaskAborted
		}
		t2, err2 := p.taskByCode(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || (t.Target > 0 && t.Current >= t.Target) {
			break
		}
	}
	if t.Claimed {
		return "任务已完成并领取（此前已领）", nil
	}
	if t.Claimable || (t.Target > 0 && t.Current >= t.Target) {
		credit, energy, already, cerr := p.cfg.Upstream.ClaimReward(a, code)
		if cerr != nil {
			return "已点亮但领奖失败（可稍后重试）: " + cerr.Error(), nil
		}
		if already {
			return "任务已点亮，奖励此前已领取", nil
		}
		return fmt.Sprintf("任务点亮并领取奖励（+%dc +%de，专家=%s realm=%s）", credit, energy, e.ExpertID, a.Realm()), nil
	}
	return fmt.Sprintf("已上报召唤+使用链（专家=%s realm=%s），进度 %s（计分异步，稍后再看）",
		e.ExpertID, a.Realm(), taskProgressText(t)), nil
}

// expertExperienceTasks 「在专家页搜索并召唤「X」，完成一次对话」类活动的固定参数。
//
// 专家 id 全部取自任务自身的 jump_url（PC 的 deeplink `experts?expertId=ex_…`、小程序的
// `expert-center/detail?id=ex_…`），与专家市场真实条目一一对应（2026-10-23 实测：
// keyword 搜索命中的 expert_id 与 jump_url 完全一致）。DisplayNameZH 是专家的**昵称**
// （卖得好 / 斗音音 / 播旺旺），ProfessionZH 才是任务标题里的职业名——判据事件里
// name 取昵称、expertTitle 取职业名，两者都参与服务端校验，不能互换。
var expertExperienceTasks = map[string]upstream.MarketExpert{
	"expert_use_maidehao": {
		ExpertID: "ex_GDl31WahyIH0", ExpertType: "agent",
		DisplayNameZH: "卖得好", ProfessionZH: "中国电商运营专家", Version: "1.0.1",
	},
	"expert_use_douyinyin": {
		ExpertID: "ex_FbY5nlWTqDod", ExpertType: "agent",
		DisplayNameZH: "斗音音", ProfessionZH: "抖音策略师", Version: "1.0.2",
	},
	"expert_use_shangjia_xuanchuan": {
		ExpertID: "ex_Weo3x2EZuSDm", ExpertType: "agent",
		DisplayNameZH: "商家宣传设计师", ProfessionZH: "商家宣传页与海报设计师", Version: "1.6.0",
	},
	"expert_use_bowangwang": {
		ExpertID: "ex_WZY136wV5aMt", ExpertType: "agent",
		DisplayNameZH: "播旺旺", ProfessionZH: "直播电商教练", Version: "1.0.2",
	},
}

// runExpertExperienceFor 把任务码绑进 autoAction.run 的函数签名（run 只收 Panel/Auth）。
func runExpertExperienceFor(code string) func(*Panel, *auth.Auth) (string, error) {
	return func(p *Panel, a *auth.Auth) (string, error) {
		return p.runExpertExperience(a, code)
	}
}

// runExpertExperience 完成「体验「X」专家」类任务：专家页搜索 → 召唤链 → 真实对话
// （服务端 requestId）→ expert_actual_use(mode=LOCAL)。
//
// 判据与 Expert_lighthouse / WorkBuddy_Valorant 同源（同一句任务文案、同一份
// jump_url 形态）；差异只在专家本身，故复用同一条事件链，仅按任务码换专家参数。
// 市场搜索只用于校正 version/名称（服务端以市场条目为准），搜不到就退回表内静态值
// ——召唤链本身不依赖搜索成功。
func (p *Panel) runExpertExperience(a *auth.Auth, code string) (string, error) {
	spec, ok := expertExperienceTasks[code]
	if !ok {
		return "", fmt.Errorf("未登记的专家体验任务: %s", code)
	}
	e := spec
	if list, err := p.cfg.Upstream.MarketExpertSearch(a, spec.ProfessionZH); err == nil {
		for _, m := range list {
			// 关键词会命中多个同名专家（display_name_zh 与 profession_zh 都参与匹配），
			// 必须按 jump_url 里的 expert_id 精确落位。
			if m.ExpertID == spec.ExpertID {
				e = m
				break
			}
		}
	}
	if e.ExpertType == "" {
		e.ExpertType = "agent"
	}
	if err := p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(e)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	conv, req, err := p.cfg.Upstream.DesktopChatWithExpert(a, e.ExpertID)
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	msgID := "msg-" + req
	if len(req) > 8 {
		msgID = "msg-" + req[len(req)-8:]
	}
	events := upstream.DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = e.ExpertID
			ev["expert_name"] = e.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	events = append(events, upstream.DesktopExpertActualUseLocal(e, conv, req))
	// 对齐真实样本细节：专家页直连召唤的 actual_use 的 type 为空、cost=0。
	events[len(events)-1]["type"] = ""
	events[len(events)-1]["cost"] = 0
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("使用事件: %w", err)
	}
	return fmt.Sprintf("已上报「%s」召唤+使用链（真实对话 requestId）", e.ProfessionZH), nil
}

// runAppearance 完成 Hp_Appearance（换主题）。
// 2026-09-12 紫川/人杰2 实测（desktopverify12）：判据是 appearance_skin_apply
// 事件 {action:"apply",source:"settings_close",id:<resourceKey>,...}（客户端在
// 主题生效状态下离开设置页时上报）——早期"纯 API set 不计分"的结论不准确，
// 真相是当时只调了 appearance/set 没发事件。组合：set API 留痕 + 事件上报。
func runAppearance(p *Panel, a *auth.Auth) (string, error) {
	const themeKey = "theme-tkmw7j" // 和平精英激战金秋（Hp_Appearance 判据主题）
	if err := p.cfg.Upstream.SetAppearanceTheme(a, themeKey); err != nil {
		return "", fmt.Errorf("设置主题: %w", err)
	}
	if !sleepAB(a.UID, 2*time.Second) {
		return "", errTaskAborted
	}
	if err := p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": themeKey, "vipLevel": 0, "series": "", "type": "unknown",
	}); err != nil {
		return "", err
	}
	return "已设置主题并上报皮肤生效事件", nil
}

// runTemplateUse 完成 template_5（使用 5 个模板创建任务）。
// 2026-09-12 三账号实测：agent_task_created_with_template + template_used 事件组
// （JOIN chat 链）一次上报 5 组即 5/5 点亮。template_id 服务端不校验真实性。
func runTemplateUse(p *Panel, a *auth.Auth) (string, error) {
	templates := [][2]string{{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"}, {"4", "活动策划"}, {"5", "代码评审"}}
	for i, tp := range templates {
		conv := session.NewConversationID()
		req := session.NewMessageID()
		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
			return fmt.Sprintf("第 %d 组模板事件上报失败: %v", i+1, err), nil
		}
		if !sleepAB(a.UID, 300*time.Millisecond) {
			return "", errTaskAborted
		}
	}
	return "已上报 template_used ×5", nil
}

// runPlaybookPrompt 完成 playbook_prompt（灵感案例 Dialog 中发送 Prompt）。
// 判据是 playbook_prompt_send（Dialog 发送）而非卡片曝光/点击——asar 逆向确认。
func runPlaybookPrompt(p *Panel, a *auth.Auth) (string, error) {
	conv := session.NewConversationID()
	req := session.NewMessageID()
	events := upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 playbook_cta_click + playbook_prompt_send", nil
}

// runCreateCanvas 完成 create_canvas（设计创意模式创建画布，+300 分）。
// 判据是 wbx_design_canvas_task_create/open（Ardot create_design 工具完成遥测）。
func runCreateCanvas(p *Panel, a *auth.Auth) (string, error) {
	conv := session.NewConversationID()
	req := session.NewMessageID()
	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 wbx_design_canvas_task_create/open", nil
}

// expertSummonGap 专家召唤链的间隔（真实使用节奏，v11 实测 8s 成功率 100%）。
const expertSummonGap = 6 * time.Second

// runExpertUse 完成 expert_5（使用 5 个平台专家）。
// 2026-09-12 三账号实测公式：真实专家列表（id 必须真实存在）→ 召唤链
// （summon_click/summoned）→ 真实 chat 拿服务端 requestId → expert_actual_use。
// 自造专家 id 或自造 requestId 均不计数。
func runExpertUse(p *Panel, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "agent", 5)
}

// runExpertTeamUse 完成 Expert_team_use_3（使用 3 个专家团，expertType=team）。
func runExpertTeamUse(p *Panel, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "team", 3)
}

// runExpertBatch 专家召唤+使用的公共实现。失败逐个继续，返回汇总信息。
func runExpertBatch(p *Panel, a *auth.Auth, expertType string, count int) (string, error) {
	experts, err := p.cfg.Upstream.MarketExpertList(a, expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空")
	}
	ok, fail := 0, 0
	for i, e := range experts {
		if ok >= count {
			break
		}
		// 召唤链（web_element_click + summon_click + summoned）。
		summonEvents := upstream.DesktopExpertSummonSequence(e)
		if err := p.cfg.Upstream.ReportDesktopEvent(a, summonEvents...); err != nil {
			fail++
			continue
		}
		// 真实 chat（带 X-Expert-Id）→ 服务端 requestId。
		conv, req, cerr := p.cfg.Upstream.DesktopChatWithExpert(a, e.ExpertID)
		if cerr != nil {
			fail++
			continue
		}
		// 使用事件（JOIN 服务端 requestId）+ chat 链。
		events := append(upstream.DesktopChatSequence(conv, req, "msg-"+upstream.Tail8(req), "fast-model", "fast-model"),
			upstream.DesktopExpertActualUseEvent(e, conv, req))
		if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
			fail++
			continue
		}
		ok++
		if i < len(experts)-1 {
			if !sleepAB(a.UID, expertSummonGap) {
				return "", errTaskAborted
			}
		}
	}
	// 失败数要写进返回值（bug #18）：此前 `_ = fail` 丢掉，用户看到"已对 5 位完成"
	// 却不知道其中 3 个是失败的（ok>=count 才停，失败的那些白跑）。
	if fail > 0 {
		return fmt.Sprintf("已对 %d 位真实专家完成召唤+使用链（类型 %s，另有 %d 位失败）", ok, expertType, fail), nil
	}
	return fmt.Sprintf("已对 %d 位真实专家完成召唤+使用链（类型 %s）", ok, expertType), nil
}

// ---------------------------------------------------------------------------
// 全量自动完成
// ---------------------------------------------------------------------------

// runAutoAll 对单账号依次执行所有可自动化任务，返回逐项结果。
// 供「一键完成全部可自动任务」使用；单项失败不影响后续项。
//
// 流程：先把所有未接受的任务批量 accept（规范状态机；上游脚本建议"先 accept"），
// 再逐项执行行为链路。accept 不是进度产生的必要条件，但让后续状态流转规范。
func (p *Panel) runAutoAll(a *auth.Auth) []map[string]any {
	var out []map[string]any
	// 企业上下文不做行为类任务（上游 PR #162）：/v2/report 返回 200 却静默丢弃，
	// 跑了也记不上账，只会白烧配额。
	if a.IsEnterpriseSession() {
		return append(out, map[string]any{
			"task_code": "(pipeline)", "status": "skipped",
			"message": "企业上下文（token_source=enterprise_switch）：上游静默丢弃行为上报，已跳过",
		})
	}

	// 阶段 0：批量接受尚未接受的任务（失败不阻塞——行为事件才是进度唯一判据）。
	if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
		var codes []string
		for _, t := range tasks {
			if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
				codes = append(codes, t.TaskCode)
			}
		}
		if len(codes) > 0 {
			// 按 20 条分片（BUG-11）：上游对 task_codes 数组长度无公开上限，
			// 面板单任务入口（tasks.go）早就这么做了，一键完成这边整批发，
			// 任务多时第一步整批被拒（只记一条 error，后续动作照跑）。
			var acceptErr error
			for i := 0; i < len(codes); i += 20 {
				end := i + 20
				if end > len(codes) {
					end = len(codes)
				}
				if err := p.cfg.Upstream.AcceptTasks(a, codes[i:end]); err != nil {
					acceptErr = err
					break
				}
			}
			if err := acceptErr; err != nil {
				out = append(out, map[string]any{
					"task_code": "(批量接受)", "status": "error",
					"message": "接受任务失败（不阻塞后续）: " + err.Error(),
				})
			} else {
				out = append(out, map[string]any{
					"task_code": "(批量接受)", "status": "done",
					"message": fmt.Sprintf("已接受 %d 个任务", len(codes)),
				})
				if !sleepAB(a.UID, reportGap) {
					return out
				}
			}
		}
	}

	// 阶段 0b：小程序口径任务单独接受（默认列表不含 mp 码；失败不阻塞）。
	if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
		var mpCodes []string
		for _, t := range mpTasks {
			if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
				mpCodes = append(mpCodes, t.TaskCode)
			}
		}
		if len(mpCodes) > 0 {
			// 同样按 20 条分片（报告 #10）：与上面的 CN 侧一致，整批下发会被上游拒。
			var mpAccErr error
			for i := 0; i < len(mpCodes); i += 20 {
				end := i + 20
				if end > len(mpCodes) {
					end = len(mpCodes)
				}
				if e := p.cfg.Upstream.AcceptTasksMP(a, mpCodes[i:end]); e != nil {
					mpAccErr = e
					break
				}
			}
			if err := mpAccErr; err != nil {
				out = append(out, map[string]any{
					"task_code": "(批量接受-mp)", "status": "error",
					"message": "接受小程序任务失败（不阻塞后续）: " + err.Error(),
				})
			} else {
				out = append(out, map[string]any{
					"task_code": "(批量接受-mp)", "status": "done",
					"message": fmt.Sprintf("已接受 %d 个小程序任务", len(mpCodes)),
				})
				if !sleepAB(a.UID, reportGap) {
					return out
				}
			}
		}
	}

	// 一次拉齐任务列表（bug-2）：此前 19 个动作各调一次 effectiveTaskCode
	// （内部 2 次 ListTasks）+ 主流程再 taskByCode 一次 ≈ 38~57 次列表请求。
	// 拉失败就退回逐次查询（行为与旧版一致，只是慢）。
	snap, snapErr := p.loadTaskMap(a)
	// covered 记录「本次上报已经覆盖过的任务码」（BUG-15）：此前是把 A 的 *Task
	// 直接写进 B 的键 —— after 的 TaskCode/Current/Target 都是 A 的，B 拿到 A 的
	// 状态后：A 已达标就误判 B 已完成而跳过，A 没推进就让 B 再跑一遍（重复上报）。
	covered := map[string]bool{}
	for _, act := range autoActions {
		item := map[string]any{"task_code": act.TaskCode, "desc": act.Desc}
		// 用**实际**任务码（bug #7）：动作可能按域换码（WorkBuddy_Valorant 国际服用
		// 不同 code），而这里此前全程用 act.TaskCode —— 国际服账号在"一键完成全部"里
		// before==nil → 记一条"该账号无此任务"，同一任务用单任务按钮却能跑通。
		var effCode string
		var before *upstream.Task
		var err error
		if snapErr == nil && snap != nil {
			effCode = effectiveTaskCodeIn(snap, act.TaskCode)
			before = snap[strings.ToLower(effCode)]
		} else {
			effCode = p.effectiveTaskCode(a, act.TaskCode)
			before, err = p.taskByCode(a, effCode)
		}
		if err != nil {
			item["status"] = "error"
			item["message"] = "查询失败: " + err.Error()
			out = append(out, item)
			continue
		}
		if before == nil {
			item["status"] = "skipped"
			item["message"] = "该账号无此任务"
			out = append(out, item)
			continue
		}
		if taskDone(before) {
			item["status"] = "skipped"
			item["message"] = "已完成（" + taskProgressText(before) + "）"
			out = append(out, item)
			continue
		}
		if covered[strings.ToLower(effCode)] {
			// 同一次上报已经满足过这个码（如 Buddy_App 覆盖 Buddy_App_QQ）：直接跳过，
			// 不再重跑一遍动作链（BUG-15）
			item["status"] = "skipped"
			item["message"] = "同一次上报已覆盖（无需重复执行）"
			out = append(out, item)
			continue
		}
		// 跳过名单门控（v1.22.0）：尝试型默认不跑；反复无效的也不跑 —— 省下的
		// 上游额度比"再试一次"值钱。
		if act.Attempt && !p.attemptAllowed() {
			item["status"] = "skipped"
			item["message"] = "尝试型任务（上游未证实可点亮），默认不执行"
			out = append(out, item)
			continue
		}
		if blocked, why := p.skipBlocked(a.UID, effCode); blocked { // 同口径用实际码（bug #7）
			item["status"] = "skipped"
			item["message"] = "已跳过（判定做不了）：" + why
			out = append(out, item)
			continue
		}
		msg, err := act.run(p, a)
		if err != nil {
			item["status"] = "error"
			item["message"] = err.Error()
			out = append(out, item)
			continue
		}
		var after *upstream.Task
		var rerr error
		if isMPTaskCode(effCode) {
			after, rerr = p.taskByCodeWaitingMP(a, effCode) // 与 Web 口径同等待（bug #6）
		} else {
			after, rerr = p.taskByCodeWaiting(a, effCode)
		}
		item["status"] = "done"
		item["message"] = msg
		// 只要动作跑过，就要登记「这次上报覆盖了哪些码」（BUG-22）：此前只在
		// 回读成功时登记，回读失败后紧接着的 Buddy_App_QQ 会把同一组事件再报一遍。
		for _, c := range act.Covers {
			covered[strings.ToLower(c)] = true
		}
		if rerr != nil {
			// 回读失败（bug A4）：动作已执行，但拿不到执行后的进度。
			// 不能写成 progress_after="" 让前端以为"进度没动"；after 为 nil 时
			// 下面的领奖分支本来也进不去，这里只把原因如实报给前端。
			item["progress_error"] = rerr.Error()
		} else if after != nil {
			// 把回读结果写回快照（bug 4）：快照只拉一次，不回写的话循环里
			// `before = snap[...]` 永远看启动时的状态 —— 任何「前一个动作顺手
			// 推进了后一个任务」的组合都会按旧状态把整条动作链再跑一遍
			// （Buddy_App / Buddy_App_QQ 共用一次上报，最典型）。
			if snap != nil {
				snap[strings.ToLower(effCode)] = after
			}
			// Covers 只标记「这次上报覆盖了它」，**不写 A 的任务对象**（BUG-15）。
			// 登记不能依赖 snap != nil（BUG-12）：loadTaskMap 失败退化为逐次查询时，
			// 共用一次上报的两个码会各跑一遍动作链。
			for _, c := range act.Covers {
				covered[strings.ToLower(c)] = true
			}
		}
		if rerr == nil {
			item["progress_after"] = taskProgressText(after)
		}
		// 进度达标即自动领奖（mp 任务走 chat 域 mp 口径，其余 Web 端接口）。
		// 领奖失败不掩盖主流程结果：status 仍为 done，附加 claim_error 供前端提示。
		if after != nil && after.Claimable {
			item["claimable"] = true
			var credit, energy int64
			var already bool
			var cerr error
			if isMPTaskCode(effCode) {
				credit, energy, already, cerr = p.cfg.Upstream.ClaimRewardMP(a, effCode)
			} else {
				credit, energy, already, cerr = p.cfg.Upstream.ClaimReward(a, effCode)
			}
			if cerr == nil {
				// 成功 = 无错误（bug #6）；already 只决定文案。
				item["claimed"] = true
				item["credit"] = credit
				item["energy"] = energy
				if already {
					item["message"] = msg + "；奖励此前已领取"
				} else {
					p.refreshCreditsAsync(a) // 领奖后刷新池内积分（bug #17）
					if credit > 0 || energy > 0 {
						item["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
					} else {
						item["message"] = msg + "；已领取（该奖励为道具/次数，不含积分与能量）"
					}
				}
			} else {
				item["claim_error"] = cerr.Error()
				item["message"] = msg + "；达标但领奖失败（可在列表手动重试）"
			}
		}
		out = append(out, item)
		// 项间节流改成可中断（bug #9）：客户端断开后不再把整条流水线跑完。
		if !sleepAB(a.UID, reportGap) {
			return out
		}
	}
	return out
}

// accountTaskAutoAll 一键完成该账号全部可自动任务。
func (p *Panel) accountTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUIDForGrowth(w, uid)
	if a == nil {
		return
	}
	// 国际版账号无成长任务体系（bug-04）：与 accountTaskAuto / accountTasks 同口径。
	// 此前这里漏了门控，"一键完成全部"会对 global 号白跑整条流水线（19 个动作各打一次
	// 上游，全是 403 growth system is only available for personal users），
	// 还长期占着 per-account 锁。
	if a.IsGlobal() {
		writeErr(w, http.StatusNotImplemented, "国际版账号无成长任务体系（上游 growth 仅对个人 CN 用户开放）")
		return
	}
	// per-account 互斥（与单任务动作共用一把锁）：重复点击 409。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	// 用 context 兜底超时（多项任务串联 + 每项含真实对话，可能耗时较长）。
	//
	// 预算必须覆盖流水线真实耗时，否则必然 504：mp 待办每条 sleep(mpChatEventGap=45s
	// + 0..10s 抖动)，9 条就 ≈ 8 分钟/账号；再叠 expert_5 的 5×6s、每项 3 次 3s
	// 回读与 reportGap 节流。原来的 5 分钟远小于这个量级 —— 返回 504 的同时后台
	// goroutine 还持着 per-account 锁，用户重试全撞 409「有任务动作正在执行中」。
	// 15 分钟给足余量；真要更长就走队列+轮询（本函数返回值里已有逐项进度）。
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	done := make(chan []map[string]any, 1)
	// 客户端断连/超时 → 给该账号打中止标记，长等待（mp 节流 45s/条）会尽快退出，
	// 而不是把整条流水线跑完再放锁（issue ISSUE-11）。
	clearAborted(uid)
	stopWatch := watchAbort(ctx.Done(), uid)
	go func() {
		defer p.unlockAccount(uid) // 流水线真正结束（而非 HTTP 超时返回）才放锁
		defer stopWatch()
		// panic 兜底（bug #1）：非 http 起的 goroutine 不会有人 recover，
		// 任务链里大量对上游响应的弱类型解析，一旦 panic 死的是整个网关。
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERR: [panel] auto_all uid=%s panic: %v", uid, r)
				select {
				case done <- []map[string]any{{"task_code": "(pipeline)", "status": "error",
					"message": fmt.Sprintf("内部错误：%v", r)}}:
				default:
				}
			}
		}()
		done <- p.runAutoAll(a)
	}()
	select {
	case results := <-done:
		log.Printf("panel: 一键完成可自动任务 uid=%s 共 %d 项", uid, len(results))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
	case <-ctx.Done():
		// HTTP 侧超时返回，但后台流水线仍在跑——锁在流水线 goroutine 内释放，
		// 期间重复点击会被 409 挡住，不会出现两轮并发。
		// 文案必须与行为一致（BUG-13）：ctx 来自 r.Context()，handler 返回后
		// watchAbort 立刻给账号打中止标记，后台流水线在下一个节流点就退出 ——
		// 说「仍在后台继续」会让用户干等一个已经停掉的流水线。
		writeErr(w, http.StatusGatewayTimeout, "执行超时，流水线已中止（可在任务列表查看已完成项）")
	}
}
