// modelbusy.go 上游「模型繁忙度」观测（官方桌面端 POST /v2/chat/busy）。
//
// 逆向来源：syan-anan/wb-syan 的 internal/upstream/modelbusy.go（其文件头注明形状来自
// xiaofan6ya/workbuddy2api 的 admin/model_busy.py）；线上实测（2026-10-16，CN）：
//
//	POST {chatBase}/v2/chat/busy        ← 与 /v2/chat/completions 同 base，路径必须带 /v2
//	请求: {"models": ["glm-5.3-flash", "deepseek-v4-pro"]}   ← 单次上限 50 个（超出分批）
//	响应: {"code":0,"data":[{"model":"glm-5.3-flash","busy_level":"idle"}, ...]}
//
// busy_level ∈ idle | normal | busy | congested | unavailable（实测还会出 unknown）。实测发现该结果**单次请求可降级**：同一批模型重查三次分别得 idle/unknown/idle，
// n=21 的一次得 9 idle + 1 congested + 10 unknown —— unknown 是服务端当次无数据（不是模型不合法），故
// 不得把 unknown 当成「不可用」展示，也不能据此改选号。这是**观测数据**，不是
// 路由依据：真正决定选号的是 pool 的冷却/熔断状态，本文件不参与任何选号决策，只给面板看（当次取不到就不填）
// ——它恰好解释了 chat 侧偶发的 429 / 6021 queue.full（同一上游容量现象的事前面）。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	modelBusyPath = "/v2/chat/busy"
	// modelBusyBatch 单批模型数上限。上游硬限制，超限整批被拒，故必须分批。
	modelBusyBatch = 50
	// modelBusyUnsupportedCooldown 探到 404 之后的冷却：期内不再探测，到期重探一次
	// （审查 W4-U2-106）。1 小时是折中：繁忙度是纯观测数据，重探一次的成本远小于
	// 「上游补了端点却永远看不见」的代价。
	modelBusyUnsupportedCooldown = time.Hour
)

// ModelBusy 查一批模型的繁忙度，返回 model → busy_level（上游未返回的模型不在 map 里）。
// 只读、无副作用；失败原样返回错误（调用方不填即可，不得据此改变选号）。
func (c *Client) ModelBusy(a *auth.Auth, models []string) (map[string]string, error) {
	out := map[string]string{}
	// 国际版没有这条端点：2026-10-29 5.9.3 真机抓包（www.workbuddy.ai）10 次
	// POST /v2/chat/busy 全部 404，同一 token 直连复验也是 404，而国内版同路径 200。
	// 官方客户端自己也在打这条不存在的路径（服务端没部署，不是我们路径写错）。
	//
	// 探到 404 就记住，本进程后续直接返回空表：繁忙度是**纯观测**数据，拿到 404
	// 既不值得每轮刷新重试、也不该让上层把「观测失败」记成错误（面板会把 busy 失败
	// 并进目录错误列表，进而拒绝写目录缓存）。
	// 自愈口子改成「冷却后重探」（审查 W4-U2-106）：此前 404 是进程级粘滞，
	// 上游补上该端点后本进程永远不再探测，繁忙度长期空白且面板看不出原因。
	// 冷却期内仍然直接返回空表（不白打请求）；冷却到期后放行一次，成功则清位。
	if c.globalOn(a) && c.modelBusyUnsupported.Load() {
		if until := c.modelBusyRetryAt.Load(); until != 0 && time.Now().UnixNano() < until {
			return out, nil
		}
	}
	for i := 0; i < len(models); i += modelBusyBatch {
		end := i + modelBusyBatch
		if end > len(models) {
			end = len(models)
		}
		raw, err := json.Marshal(map[string]any{"models": models[i:end]})
		if err != nil {
			return out, err
		}
		req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+modelBusyPath, bytes.NewReader(raw))
		if err != nil {
			return out, err
		}
		// 8 个头，不是 chat 族 43 个（见 busyHeaders 注释）
		c.busyHeaders(req, a)
		data, err := c.doJSON(req)
		if err != nil {
			var ue *Error
			if c.globalOn(a) && errors.As(err, &ue) && ue.Kind == ErrNotFound {
				// 置位 + 记冷却截止（审查 W4-U2-106）：冷却期内不再探测，到期后重探一次。
				c.modelBusyUnsupported.Store(true)
				c.modelBusyRetryAt.Store(time.Now().Add(modelBusyUnsupportedCooldown).UnixNano())
				log.Printf("model busy: global realm 无 /v2/chat/busy（404），%s 内跳过繁忙度探测",
					modelBusyUnsupportedCooldown)
				return out, nil
			}
			return out, err
		}
		var rows []struct {
			Model string `json:"model"`
			Level string `json:"busy_level"`
		}
		if err := json.Unmarshal(data, &rows); err != nil {
			return out, err
		}
		for _, r := range rows {
			if r.Model != "" && r.Level != "" {
				out[r.Model] = r.Level
			}
		}
		// 端点可用 → 清掉「不支持」标记与冷却（审查 W4-U2-106 的自愈闭环）：
		// 上游补上端点后，第一次成功的探测就应当让后续请求恢复正常探测节奏。
		if c.modelBusyUnsupported.Load() {
			c.modelBusyUnsupported.Store(false)
			c.modelBusyRetryAt.Store(0)
			log.Printf("model busy: /v2/chat/busy 已恢复可用，解除跳过")
		}
	}
	return out, nil
}
