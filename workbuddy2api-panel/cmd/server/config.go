// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Panel struct {
		// PackageDetailLimit 积分构成页单账号默认展示的最近到期包数；<=0 回落 5。
		PackageDetailLimit int `json:"package_detail_limit"`
	} `json:"panel"`

	Logging struct {
		// RequestArchiveEnabled 请求元数据 JSONL 归档开关，缺省 true。
		RequestArchiveEnabled bool `json:"request_archive_enabled"`
		// RequestRetentionDays 归档保留天数，缺省 7；<=0 回落默认。
		RequestRetentionDays int `json:"request_retention_days"`
		// RequestArchiveMaxMB 归档总上限（MiB），缺省 100；<=0 回落默认。
		RequestArchiveMaxMB int `json:"request_archive_max_mb"`
		// RequestClientInfo 是否在请求日志（归档事件 + stdout 流水行 + 面板运行
		// 日志）里记录调用来源：客户端 IP 与 User-Agent。缺省 true。
		//
		// 为什么做成开关而不是恒开：来源信息是排查"谁在打网关"的第一手线索，
		// 但它比 token 计数敏感（IP 属个人信息），共享部署/多租户场景可能需要
		// 关掉。关闭后 Event.ClientIP/UserAgent 保持为空，归档里不出现该字段。
		// 热生效（经 livecfg 快照），无需重启。
		RequestClientInfo bool `json:"request_client_info"`
	} `json:"logging"`

	Server struct {
		// ReadTimeout 入站请求读取（含 body 上传）总时长上限（issue #100）。
		// http.Server 的 ReadTimeout 覆盖整个请求读取：大上下文/文件块请求经
		// 反代链转发时上传可超过旧固定值 60s，被掐后客户端拿到
		// 400 "read body: ... i/o timeout"。缺省 "300s"；"0" = 不限制
		//（慢速 body 可无限占用连接，自担风险）；改动需重启进程。
		ReadTimeout string `json:"read_timeout"` // "300s"；"0" = 不限制
		// MaxBodyMB /v1/chat/completions 请求体上限（MB，review P1-10）。
		// 三态（issue ISSUE-07）：null/缺省 = 默认 64（远超真实长上下文，只挡内存炸弹）；
		// 0 = **不限制**（保留历史上「对齐上游不设限」的语义）；负值 = 报错。
		// 用指针而不是 int：0 在 Go 里与"没填"不可区分，此前注释承诺 0=不限制、
		// 实现却把 0 当默认值，用户按注释填 0 仍然被 64MB 挡住。
		MaxBodyMB *int `json:"max_body_mb"`
	} `json:"server"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
		// SoftResetMax 上游**权威重置墙钟**（429 文案「将在 … 重置」/ Retry-After 头）
		// 的冷却上限，默认 "24h"。与 soft_rate_max 分工不同，别混用：
		//   - soft_rate_max：不知道上游何时恢复时的**指数退避**封顶（默认 2h）；
		//   - soft_reset_max：上游明确给了恢复时刻时的**合理性上限**，只用来挡明显
		//     荒谬的解析结果（脏缓存文案 / 时钟错乱），不参与退避。
		// 历史坑：这两个语义曾共用一个 soft_rate_max —— 上游文案说「16:26 重置」
		// （10 小时后），我们 2 小时就把号放回池子，冷却一过照旧 6004，客户端一直 429。
		SoftResetMax string `json:"soft_reset_max"` // "24h"
	} `json:"cooldown"`

	Schedule struct {
		// AccountSpreadSeconds 账号遍历型任务的**账号间最小间隔**（秒），
		// 0 = 关闭（同一秒打全池）。默认 0 保持既有行为。
		// 一批任务同一秒打几十个号，上游看到的是「同一瞬间几十个客户端」；
		// 按账号错开能显著降低相关性风控（misakano7545 `3dfe46c3` issue #45）。
		AccountSpreadSeconds int `json:"account_spread_seconds"`
		// AccountSpreadMinutes / AccountSpreadMinGapSeconds 账号级摊开（issue #45 方案 A）：
		// 每号在窗口内的确定性偏移（按「任务|uid|日」哈希），而不是固定节拍。
		// 0 = 关闭（回落 account_spread_seconds 的串行延迟）。
		AccountSpreadMinutes       int   `json:"account_spread_minutes"`
		AccountSpreadMinGapSeconds int   `json:"account_spread_min_gap_seconds"`
		CheckinHours               []int `json:"checkin_hours"`   // [9,21]
		TravelHours                []int `json:"travel_hours"`    // [9,21]
		ActivityHours              []int `json:"activity_hours"`  // [10]
		KeepaliveHours             []int `json:"keepalive_hours"` // [22]
		BlackcatHours              []int `json:"blackcat_hours"`  // [23] 夜猫子窗口（23:00–08:00 计数）
		GrowthHours                []int `json:"growth_hours"`    // [1] 成长任务队列（Sequential 族每日零点解锁，01:00 自动扫描执行）
		// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/BlackcatEnabled 显式禁用开关（缺省 true）。
		//
		// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
		//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
		//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
		//     则对老配置零影响，向后完全兼容。
		//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
		//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
		// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
		CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
		TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
		ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
		KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
		BlackcatEnabled  bool `json:"blackcat_enabled"`  // 缺省 true；false = 关夜猫子
		GrowthEnabled    bool `json:"growth_enabled"`    // 缺省 true；false = 关成长任务自动排程

		// IncludeDisabledInTasks 让「保号类」定时任务（签到 / 活跃上报 / token 保活 /
		// 余额刷新）对**已禁用（disabled）**的账号也执行。
		//
		// 为什么需要它：面板「禁用」的语义是「不再参与选号」（见面板确认文案），但这四类
		// 任务此前一律 `if st.Disabled { continue }`，等于把「停用流量」放大成「停止一切
		// 上游保号行为」——被禁用的号拿不到签到积分、不续 token、余额也不再刷新；而
		// ReenableIfCredits 明确不复活 disabled 账号（见 pool.state.go），于是签到这条唯一
		// 的自动回血路径也断了，账号只能靠人工「解冻」回来。
		//
		// 对「一次只放开一个号、用禁用做流量开关」的轮换用法（同 IP 多号防风控），闲置
		// 待命的号恰恰是最需要签到的那批——本开关即为该用法提供出口。
		//
		// 缺省 false = 保持既有行为，对老配置零影响。打开后禁用号仍会签到 / 保活，但
		// **依旧不参与选号**：pool 选号侧的 disabled 过滤不受本开关影响。
		IncludeDisabledInTasks bool `json:"include_disabled_in_tasks"`
		// KeepaliveRefreshWindow 周期保活刷新的临期窗口（PR #244）：排程路径只刷新
		// 剩余有效期 <= 该值的账号；空/非法见 normalize 的回落与 fail-fast。
		KeepaliveRefreshWindow string `json:"keepalive_refresh_window"`

		// 余额后台周期刷新：两次签到时点之间 credits 也能保持新鲜（面板/状态观测用）。
		// 解冻语义同签到（余额 > 0 的冷却账号自动解冻），但不做签到不刷 token。
		BalanceRefreshEnabled bool `json:"balance_refresh_enabled"` // 缺省 true；false = 关闭
		BalanceRefreshMinutes int  `json:"balance_refresh_minutes"` // 缺省 5；<=0 回落 5
	} `json:"schedule"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	// Models 模型目录（上游 PR #225）。
	// Budget 日预算与两道快速闸（移植 58262f31）。三项均 0 = 关闭，老配置零影响。
	Budget struct {
		// DailyBudget 日预算上限（credit）；0 = 不限。
		DailyBudget float64 `json:"daily_budget"`
		// BurstCreditLimit 5 分钟窗口内累计 credit 达此值即拒绝（0 = 关闭）。
		BurstCreditLimit float64 `json:"burst_credit_limit"`
		// CacheLowRate 连续 5 轮缓存命中率低于此百分比即冷却 60s（0 = 关闭）。
		CacheLowRate float64 `json:"cache_low_rate"`
	} `json:"budget"`

	Models struct {
		// Hidden 不对外暴露的模型黑名单：裸名=两域都藏；"cn:x"/"global:x"=只藏该域。
		// 生效点：/v1/models 列表过滤 + chat 点名拒绝（400 model_unavailable）。改动需重启。
		Hidden []string `json:"hidden"`
		// Aliases 模型名映射（issue #30）："客户端名=真实模型名"。
		// 客户端只能填固定模型名的 agent 靠它落到真实模型。
		// 键大小写不敏感；裸名键两域生效，cn:/global: 前缀键只在该域生效。
		Aliases []string `json:"aliases"`
	} `json:"models"`

	// Cn 国内版上游 base 覆盖（上游 PR #220）：空 = 回落内置默认
	// https://copilot.tencent.com。base 在启动时注入，改动需重启。
	Cn struct {
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"cn"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// 默认值已对齐官方 WorkBuddy 桌面形态（三段式），用户仍可配完全自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与归属头
		// X-IDE-Version）。空 = 内置默认（对齐官方 5.5.4 分发包）。
		ClientVersion string `json:"client_version"`
		// ClientVersionGlobal global realm 专用版本段（空 = 内置默认 5.8.4）。
		// CN 与 intl 是两条独立发版线，单值会让一侧 UA 与真实安装包不符。
		ClientVersionGlobal string `json:"client_version_global"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 内置默认（官方内置 CLI 2.137.1）。
		CliVersion string `json:"cli_version"`
		// CliVersionGlobal global realm 专用 CLI 版本段（空 = 内置默认 2.161.0）。
		CliVersionGlobal string `json:"cli_version_global"`
		// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
		// 空 = 旧行为 X-Product="SaaS" 不设 X-IDE-*；配 "WorkBuddy" 则四头跟随。
		ClientName string `json:"client_name"`
		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底；空 = 不注入。
		// 每号 auth 文件的 device_token 键优先于本项。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile device token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
		DeviceTokenFile string `json:"device_token_file"`
		// PassthroughIP 是否透传客户端 IP 给上游（默认 false，反代安全边界）。
		PassthroughIP bool `json:"passthrough_ip"`
		// DNSServers 出站域名解析用的 DNS 服务器（上游 PR #144）：空 = 系统默认。
		// 例：["1.1.1.1", "223.5.5.5"]。改动需重启（Dialer 在启动时构造）。
		DNSServers []string `json:"dns_servers"`
		// DumpDir 非空时把 chat 请求的出站报文 + 上游原始响应落盘（诊断用，见
		// internal/upstream/dump.go）。默认空 = 不落盘：正文含用户提示词。
		DumpDir string `json:"dump_dir"`
		// DumpMaxBytes 落盘文件里「出站请求体」片段的上限，默认 262144（256 KB）。
		// 长会话把 tools 定义排在几百条 messages 之后，256 KB 常在中途截断，
		// 排障时看不到工具声明（issue #44 的反馈）。0 = 用默认值。
		DumpMaxBytes int `json:"dump_max_bytes"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（默认）= 透传客户端原始 system（降级重试仍会切到 Degraded）；
		// custom = 网关用自有系统提示词替换客户端 system/developer；
		// append = 两者并用：开头连续 system/developer 块后插网关 system，既有消息逐字不动（issue #129）。
		Mode string `json:"mode"` // "passthrough" / "custom" / "append"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
		// IdentityOverrideEnabled 仅在 passthrough/append 模式替换客户端 harness 的
		// 身份首句（吸收 qcgzxw `a7f10ad1` / 上游 PR #2）：只改开头连续
		// system/developer 里以 "You are " 开头的**第一句**，其余内容逐字保留。
		// 缺省关闭，不改变既有行为。需重启。
		IdentityOverrideEnabled bool `json:"identity_override_enabled"`
		// IdentityOverrideText 替换后的首句；空值回落到 prompt.DefaultHarnessIdentity。
		IdentityOverrideText string `json:"identity_override_text"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom/append 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int    `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		MaxInFlightGlobal  int    `json:"max_in_flight_global"` // global 域单账号在途上限（WAF 风控紧域压低并发），0 = 回落默认 2
		BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 连败降权（issue #114）：ErrClient/传输层这类「不罚号」失败连续计数，达阈
		// 临时出池。与冷却/熔断并存取更长者不叠加。默认 5 次 / 10m。
		DegradeThreshold   int     `json:"degrade_threshold"`    // 连败次数触发降权，默认 5
		DegradeCooldown    string  `json:"degrade_cooldown"`     // 降权时长（固定，非指数退避），默认 "10m"
		DegradeCooldownMax string  `json:"degrade_cooldown_max"` // 降权时长的上限钳制，默认 "2h"（仅当 cooldown 超该值才钳制）
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// PreferExpiring 快过期积分加权开关，默认 true。开启且 expiring_soon 窗口内
		// 存在有效批次时，该账号选号权重 ×3（虚拟实例，见 pool 路由加权）；
		// 不按到期时间排序、与批次金额无关（issue #101 对齐实现口径）。
		// 关闭后完全不使用到期信息选号。
		PreferExpiring bool `json:"prefer_expiring"`
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到/余额刷新时，到期时间在
		// 此窗口内的批次令账号命中上述 ×3 加权；窗口开大 → 命中账号变多、
		// 偏好被稀释。空/0 = 禁用该加权门槛。
		ExpiringSoon string `json:"expiring_soon"`
		// CostExploreInterval costTier 条件探索窗口（issue #136 方案 a′）：tier 0
		// 垄断层存在且 tier 1 有成员时，距上次探索 ≥ 窗口则本次 pick 生效层切
		// tier 1-only（探索=搭车改道，零新增上游请求；成功即毕业，失败走既有
		// 错误策略）。默认 "30m"（≤48 次/天/模型）；"0" 关停（完全回到现状行为）；
		// 空值回落默认。
		CostExploreInterval string `json:"cost_explore_interval"`
		// CreditFloor 积分保底：账号余额低于该值时，对实测收费模型（tier 2）不再
		// 参与选号——防止收费请求把余额打穿、连免费模型都 402 冷却到次日签到。
		// tier 0（免费）/ tier 1（无观测）不受限；签到回血越过 floor 自动恢复。
		// 默认 0 = 关闭；负值钳 0。
		CreditFloor int64 `json:"credit_floor"`
		// ModelFallback 模型降级路由表：key = 客户端请求的模型（裸名），
		// value = 该模型被 6004 模型级限流时改用的等价模型。
		// 例：{"deepseek-v4.1-flash": "deepseek-v4-flash"}。
		//
		// 语义：等价模型是同一底层能力、只是计价不同；便宜的那个被限流时改用
		// 稍贵的继续服务（可用性优先），限流解除后自动回原模型（不写持久状态）。
		// 只对 6004 生效；11102/11101/普通 429/5xx 都不降级。缺省 nil = 不降级。
		ModelFallback map[string]string `json:"model_fallback"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur            time.Duration `json:"-"`
	SoftRateMaxDur         time.Duration `json:"-"`
	SoftResetMaxDur        time.Duration `json:"-"`
	BreakerCooldownDur     time.Duration `json:"-"`
	BreakerCooldownMaxD    time.Duration `json:"-"`
	DegradeCooldownDur     time.Duration `json:"-"`
	DegradeCooldownMaxD    time.Duration `json:"-"`
	SessionTTL             time.Duration `json:"-"`
	SessionGCInterval      time.Duration `json:"-"`
	BalanceRefreshInterval time.Duration `json:"-"` // 0 = 不启动（enabled=false）
	ExpiringSoonDur        time.Duration `json:"-"`
	// KeepaliveRefreshWindowDur 解析后的周期保活临期窗口；空回落 168h，<=0 报错。
	KeepaliveRefreshWindowDur time.Duration `json:"-"`
	// CostExploreIntervalDur 解析后的 costTier 探索窗口（issue #136）；0 = 关停。
	CostExploreIntervalDur time.Duration `json:"-"`
	// ServerReadTimeoutDur 解析后的入站请求读取上限（issue #100）；0 = 不限制。
	ServerReadTimeoutDur time.Duration `json:"-"`
	// ServerMaxBodyMB /v1/chat/completions 请求体上限（MB）；0 = 不限制。
	ServerMaxBodyMB int `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		// 默认只监听回环（review H7）：默认值决定绝大多数部署的实际暴露面，
		// ":7863" + 空密钥 = 同网段任何人都能白嫖账号、调面板接口、读日志。
		// 要对外提供服务必须显式改成 0.0.0.0:7863 **并**配置 api_key（见 checkListenKey）。
		Listen:    "127.0.0.1:7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Cooldown.SoftResetMax = "24h"
	c.Server.ReadTimeout = "300s"
	c.Panel.PackageDetailLimit = 5
	c.Logging.RequestArchiveEnabled = true
	c.Logging.RequestRetentionDays = 7
	c.Logging.RequestArchiveMaxMB = 100
	// 缺省 true 靠显式赋值实现（同 Schedule 开关）：JSON 里键缺席时字段保留此值，
	// 只有显式 false 才关闭来源记录。
	c.Logging.RequestClientInfo = true
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.TravelHours = []int{9, 21}
	c.Schedule.ActivityHours = []int{10}
	c.Schedule.KeepaliveHours = []int{22}
	// 周期保活临期窗口默认 7 天：排程保活只刷新一周内到期的凭证，其余账号不动。
	c.Schedule.KeepaliveRefreshWindow = "168h"
	c.Schedule.BlackcatHours = []int{23}
	c.Schedule.GrowthHours = []int{1}
	// 开关「缺省 true」靠这几行实现：Load 先取 Default() 再 json.Unmarshal 覆盖，
	// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
	c.Schedule.CheckinEnabled = true
	c.Schedule.GrowthEnabled = true
	c.Schedule.TravelEnabled = true
	c.Schedule.ActivityEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BlackcatEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 5
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "passthrough" // 缺省 passthrough：透传客户端原始 system（对齐上游；custom 由用户显式选择）
	// 身份首句替换缺省关闭（老配置无该键 → false），但文本预置默认值，
	// 面板勾选后无需再填。
	c.Prompt.IdentityOverrideText = prompt.DefaultHarnessIdentity
	c.Pool.MaxInFlight = 3
	// MaxInFlightGlobal 缺省 2：global 域 WAF 风控更紧，压低单号并发（WAF 403 修复
	// P1-1）；0/负数 normalize 回落默认（与 max_in_flight 的 0=不限语义不同，分档键
	// 的 0 没有合理语义，回退分档默认最稳）。
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.PreferExpiring = true
	c.Pool.ExpiringSoon = "168h" // 快过期窗口默认 7 天：官方活动奖励积分多在两周内过期
	// costTier 探索默认 30m（issue #136：垄断破除 + 搭车改道零新增请求）；"0" 关停。
	c.Pool.CostExploreInterval = "30m"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		// 目录检查：Docker bind mount 在宿主机文件缺失时会静默创建同名目录，
		// 直接 ReadFile 会报 "Incorrect function" 之类晦涩错误，这里给出可操作提示。
		if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
			return nil, fmt.Errorf("config %s 是目录而非文件——"+
				"Docker 部署时若宿主机缺少 config.json，bind mount 会创建同名目录。"+
				"请删除该目录（程序会自动生成带随机 api_key 的配置），或手工放一份自己的 config.json", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		// 只做 JSON 覆盖，**不校验**（bug P0-1）：ParseConfigInto 内部会 normalize，
		// 而 normalize 里的 checkListenKey 要看 api_key —— 此时 applyEnv 还没跑，
		// "文件里只写 listen、密钥走 WB2A_API_KEY" 这种最常见的容器部署会直接
		// 报「必须配置非空 api_key」起不来，尽管环境变量里明明有。
		if err := ApplyJSON(raw, c); err != nil {
			return nil, err
		}
	}
	applyEnv(c) // 必须在 normalize 之前
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ApplyJSON 只把 JSON 覆盖到 c 上，不做 normalize（不校验、不补默认）。
//
// 调用方必须自己保证顺序：ApplyJSON → applyEnv → normalize。
// 顺序错了就会出现「env 里的密钥还没生效、校验已经用文件里的空值把你拦下」（bug P0-1）。
func ApplyJSON(raw []byte, c *Config) error {
	if err := json.Unmarshal(raw, c); err != nil {
		// 容错重试（回归修复）：面板表单曾把 logging.request_retention_days 以
		// **字符串**提交（input 没写 type=number），config.json 里落成 "7"，
		// 下次启动就是这个错：
		//   parse config: json: cannot unmarshal string into Go struct field
		//   Config.logging.request_retention_days of type int
		// 网关直接起不来。前端已修，这里兜住**已经在盘上的坏文件**：
		// 只纠正明确知道的数值字段（不做全局类型猜测 —— 那会把 "123" 这类
		// 合法字符串字段也改掉），纠正后重试一次。
		if fixed, names, ok := coerceNumericStrings(raw); ok {
			if err2 := json.Unmarshal(fixed, c); err2 == nil {
				log.Printf("WARN: config 里 %s 被存成了字符串，已按数字解析"+
					"（在面板里重新保存一次即可写回数字）", strings.Join(names, ", "))
				return nil
			}
		}
		return fmt.Errorf("parse config: %w", err)
	}
	return nil
}

// numericStringFields 面板表单可能以字符串提交的数值字段（section, key）。
//
// 为什么要列出来：前端 input 的类型决定 collectConfig 是发数字还是发字符串，
// 漏写 type="number" 就会把 "7" 写进 config.json，而 Go 侧字段是 int —— 解析
// 直接失败，表现出来的却是「网关起不来」。这张表只用于**容错解码**，
// 不是校验白名单。
var numericStringFields = [][2]string{
	{"logging", "request_retention_days"},
	{"logging", "request_archive_max_mb"},
	{"panel", "package_detail_limit"},
	{"pool", "max_in_flight"},
	{"pool", "max_in_flight_global"},
	{"pool", "breaker_threshold"},
	{"pool", "degrade_threshold"},
	{"pool", "credit_floor"},
	{"schedule", "balance_refresh_minutes"},
	{"schedule", "account_spread_seconds"},
	{"upstream", "timeout_seconds"},
	{"upstream", "header_timeout_seconds"},
	{"upstream", "idle_timeout_seconds"},
	// 这两个在 **pool** 段（config.go 的 Pool.IdleWeightPerHour/Max），
	// 早先挂到 upstream 段的话永远命中不到真实字段 —— 报告 #1。
	{"pool", "idle_weight_per_hour"},
	{"pool", "idle_weight_max"},
}

// coerceNumericStrings 把这些字段上「看起来是数字的字符串」改回 JSON 数字。
// 返回纠正后的字节、被纠正的字段名、是否发生了纠正。
//
// 用 UseNumber 读，未改动的数字原样保留（避免 float64 往返改变精度/写法）。
func coerceNumericStrings(raw []byte) ([]byte, []string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, nil, false
	}
	var fixed []string
	for _, pair := range numericStringFields {
		sec, _ := root[pair[0]].(map[string]any)
		if sec == nil {
			continue
		}
		str, ok := sec[pair[1]].(string)
		if !ok {
			continue
		}
		t := strings.TrimSpace(str)
		if t == "" {
			continue
		}
		if _, err := strconv.ParseFloat(t, 64); err != nil {
			continue // 不是数字就不动，交给正常报错
		}
		sec[pair[1]] = json.Number(t)
		fixed = append(fixed, pair[0]+"."+pair[1])
	}
	if len(fixed) == 0 {
		return nil, nil, false
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, nil, false
	}
	return out, fixed, true
}

// ParseConfigInto 把 JSON 覆盖到 c 上并 normalize（不做 env、不读文件）。
// 面板保存配置走这条路径：与 Load 完全同一套解析/校验逻辑，避免两处漂移。
func ParseConfigInto(raw []byte, c *Config) (*Config, error) {
	if err := ApplyJSON(raw, c); err != nil {
		return nil, err
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfig 基于默认值解析一段配置 JSON（等价于 Load 的文件分支，但不读环境变量）。
func ParseConfig(raw []byte) (*Config, error) {
	return ParseConfigInto(raw, Default())
}

// WriteDefault 在 path 落一份推荐配置（首次运行自动生成，双击即开免手工复制样例）。
// 值取自 Default()（含超时/熔断/签到排程等推荐值），api_key 用 crypto/rand 随机生成：
// 安全默认优于示例占位符（listen 绑定 0.0.0.0，空 key 会把网关裸暴露给局域网）。
// 返回生成的 key 供启动日志透出。已存在时经 O_EXCL 原子拒绝，绝不改写用户配置。
func WriteDefault(path string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	key := "sk-" + base64.RawURLEncoding.EncodeToString(raw)
	c := Default()
	c.APIKey = key
	_ = c.normalize() // Default() 全合法，normalize 仅补齐 header/idle 超时的展示值
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	// O_EXCL 原子拒绝覆盖：即使调用方漏判"不存在"，也绝不悄悄改写用户已有配置。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	// 必须 Sync 再 Close（bug #15）：这是 O_EXCL 生成的「唯一一份密钥」，
	// 掉电/进程被强杀时可能留下 0 字节或半截 config.json，下次启动 Load 直接
	// 报 parse config，而密钥无处可寻。与 auth.ReplaceAtomic 的写分支同口径。
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync config: %w", err)
	}
	return key, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_SOFT_RESET_MAX"); v != "" {
		c.Cooldown.SoftResetMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_DUMP_UPSTREAM"); v != "" {
		c.Upstream.DumpDir = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := os.Getenv("WB2A_PREFER_EXPIRING"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Pool.PreferExpiring = b
		}
	}
}

// placeholderKeys 已知的占位密钥：示例配置里的值，等于没有密钥。
var placeholderKeys = map[string]bool{"test_key": true, "CHANGE_ME": true, "changeme": true}

// checkListenKey 拒绝「非回环监听 + 空/占位密钥」的组合（review H7）。
func (c *Config) checkListenKey() error {
	if strings.TrimSpace(c.Listen) == "" || c.Listen == ":" {
		// 归一化后仍为空/只有冒号：ListenAndServe 会报 "missing port in address"，
		// 那条错误指不到"配置漏填 listen"这个真实原因（review 观察项 2）。
		return fmt.Errorf("listen: 为空（请填 127.0.0.1:7863 或 0.0.0.0:7863）")
	}
	if isLoopbackListen(c.Listen) {
		return nil // 只给本机用：裸奔可接受（也正是 APK 内的形态）
	}
	k := strings.TrimSpace(c.APIKey)
	if k == "" || placeholderKeys[k] {
		shown := k
		if shown == "" {
			shown = "（空）"
		}
		return fmt.Errorf("api_key: listen=%q 不是回环地址，必须配置非空且非占位的 api_key（当前 %s）。"+
			"生成一个：openssl rand -hex 24；或把 listen 改成 127.0.0.1:7863 只给本机用", c.Listen, shown)
	}
	return nil
}

// isLoopbackListen 判断监听地址是否只对本机开放。
// 解析不出来的（"7863"、""、"0.0.0.0:7863"）一律按「对外」处理——宁可多要一个密钥。
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // 没有端口段时 SplitHostPort 报错，直接拿整串判
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Config) normalize() error {
	var err error
	// 监听面 × 密钥一致性守卫放在 listen 归一化**之后**（见本函数末尾）：
	// 归一化会把 "7863" 补成 ":7863"，在此之前判定会把"还没补冒号"的形态当成
	// 对外地址，报错信息与真实原因错位（review 观察项 2）。
	if c.Panel.PackageDetailLimit <= 0 {
		c.Panel.PackageDetailLimit = 5
	}
	if c.Logging.RequestRetentionDays <= 0 {
		c.Logging.RequestRetentionDays = 7
	}
	if c.Logging.RequestArchiveMaxMB <= 0 {
		c.Logging.RequestArchiveMaxMB = 100
	}
	// 入站读取上限（issue #100）：空值回落默认 300s；"0" 合法（不限制）；
	// 负值无语义，fail fast（静默钳 0 会把保护悄悄关掉）。
	if c.Server.ReadTimeout == "" {
		c.Server.ReadTimeout = "300s"
	}
	if c.ServerReadTimeoutDur, err = time.ParseDuration(c.Server.ReadTimeout); err != nil {
		return fmt.Errorf("server.read_timeout: %w", err)
	}
	switch {
	case c.Server.MaxBodyMB == nil:
		c.ServerMaxBodyMB = 64 // 缺省：远超真实长上下文，只挡内存炸弹
	case *c.Server.MaxBodyMB < 0:
		return fmt.Errorf("server.max_body_mb: 负值 %d 无意义（0 = 不限制，null = 默认 64）", *c.Server.MaxBodyMB)
	default:
		c.ServerMaxBodyMB = *c.Server.MaxBodyMB // 0 = 不限制
	}
	if c.ServerReadTimeoutDur < 0 {
		return fmt.Errorf("server.read_timeout: 负时长 %q 无意义", c.Server.ReadTimeout)
	}
	// 空值回落（bug P1-2）：与 breaker_cooldown / degrade_cooldown / soft_rate_max 同一口径。
	// 此前这三个字段没兜底，config 里写 "" （用户以为"删掉=用默认"）会让
	// ParseDuration("") 失败、进程直接起不来。
	if c.Cooldown.SoftRate == "" {
		c.Cooldown.SoftRate = "600s"
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	// 上游权威重置墙钟的合理性上限（默认 24h）：空值回落、非法值 fail fast，
	// 口径与 soft_rate_max 一致。应当明显大于 soft_rate_max，否则退化成旧行为。
	if c.Cooldown.SoftResetMax == "" {
		c.Cooldown.SoftResetMax = "24h"
	}
	if c.SoftResetMaxDur, err = time.ParseDuration(c.Cooldown.SoftResetMax); err != nil {
		return fmt.Errorf("cooldown.soft_reset_max: %w", err)
	}
	// 预算闸的取值校验：负值无语义，缓存率是百分比。
	if c.Budget.DailyBudget < 0 {
		return fmt.Errorf("budget.daily_budget: %v 不得为负；0 = 不限", c.Budget.DailyBudget)
	}
	if c.Budget.BurstCreditLimit < 0 {
		return fmt.Errorf("budget.burst_credit_limit: %v 不得为负；0 = 关闭", c.Budget.BurstCreditLimit)
	}
	if c.Budget.CacheLowRate < 0 || c.Budget.CacheLowRate > 100 {
		return fmt.Errorf("budget.cache_low_rate: %v 须在 0-100 之间（百分比）；0 = 关闭", c.Budget.CacheLowRate)
	}
	// 空值回落必须在 ParseDuration **之前**（issue B4）：`ParseDuration("")` 直接
	// 返回 invalid duration，配置里写 `"degrade_cooldown": ""` 会让进程启动失败，
	// 而下面那段"空值回落默认"的分支永远走不到。口径与 soft_rate_max 一致：
	// 空 → 回落默认，非法 → fail fast。
	if c.Pool.BreakerCooldown == "" {
		c.Pool.BreakerCooldown = "30m"
	}
	if c.Pool.BreakerCooldownMax == "" {
		c.Pool.BreakerCooldownMax = "6h"
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionSticky.TTL == "" { // 空值回落（bug P1-2）
		c.SessionSticky.TTL = "30m"
	}
	if c.SessionSticky.GCInterval == "" {
		c.SessionSticky.GCInterval = "5m"
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	// 快过期窗口：空 = 禁用（ExpiringSoonDur 0）；非空必须可解析（拼写错误 fail fast）。
	if c.Pool.ExpiringSoon != "" {
		if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
			return fmt.Errorf("pool.expiring_soon: %w", err)
		}
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0
		c.Pool.ExpiringSoon = "0"
	}
	// costTier 探索窗口（issue #136）：空值回落默认 30m（Default 已置；此兜底覆盖
	// 显式 ""）；"0" 是合法值（关停，完全回到现状行为），不回落；负值钳 0 同关停
	//（"−5m" 无合理语义）。
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		// 负值钳 0 时同步回写字符串（bug #14）：与 expiring_soon 的处理同口径。
		// 否则配置里仍留 "-5m"，面板保存后写盘还是负值、重启再钳一次，
		// 而 restartRequiredFields 不含该键，用户看不到任何提示。
		c.CostExploreIntervalDur = 0
		c.Pool.CostExploreInterval = "0"
	}
	// 积分保底：负值钳 0（= 关闭）。0 是合法默认（关闭），无需空值回落。
	if c.Pool.CreditFloor < 0 {
		c.Pool.CreditFloor = 0
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// 连败降权参数缺省归一（非法/未设置回落默认，与 breaker_threshold 同风格）。
	// DegradeCooldown / DegradeCooldownMax 的空值回落已在上面的 duration 段做过
	// （bug P2-1：这里原有一份重复判断，且那时值早已被 ParseDuration 用过，永不命中）。
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	// global 在途分档（报告 #9）：`0` 是**有效值** = 不分档、回落 max_in_flight
	// （pool.SetMaxInFlightGlobal 的既定语义）。此前 `<= 0` 一律改成 2，
	// 配置里根本表达不出「不分档」，max_in_flight=8 时 global 号被静默压到 2 路。
	// 只有负数才是非法值，回落默认 2（WAF 403 修复 P1-1）。
	if c.Pool.MaxInFlightGlobal < 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	// 只有端口（"7863"）→ 补成 ":7863"；只有主机名（"localhost"）→ 明确报错（bug #14）。
	// 原先一律补冒号会把 "localhost" 变成 ":localhost"，ListenAndServe 报 unknown port，
	// 错误文本指不到 listen 配置项。
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		if _, perr := strconv.Atoi(c.Listen); perr == nil {
			c.Listen = ":" + c.Listen
		} else if c.Listen != "" {
			return fmt.Errorf("listen: %q 缺少端口（写成 127.0.0.1:7863 或 :7863）", c.Listen)
		}
	}
	// 监听面 × 密钥一致性守卫（review H7，位置在归一化之后）：默认 listen 是
	// 全网卡形态，配空密钥/占位密钥时同网段任何人都能白嫖账号积分、调面板接口
	// （含移除账号）、读日志里的昵称/UID/来源 IP。只允许回环监听裸奔。
	if err := c.checkListenKey(); err != nil {
		return err
	}
	// 空数组与 null 反序列化后覆盖掉 Default() 的排程值（键缺席才保留），在此补齐。
	// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.TravelHours) == 0 {
		c.Schedule.TravelHours = []int{9, 21}
	}
	if len(c.Schedule.ActivityHours) == 0 {
		c.Schedule.ActivityHours = []int{10}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	// 周期保活临期窗口：空回落默认 168h（Default 已置，兜底显式 ""）；非法值
	// fail fast（拼写错误静默吞掉等于窗口变 0 → 全量刷新，违背本字段意图）；
	// <=0 无合理语义（0/负值想全量应直接删键），报错。
	if c.Schedule.KeepaliveRefreshWindow == "" {
		c.Schedule.KeepaliveRefreshWindow = "168h"
	}
	if c.KeepaliveRefreshWindowDur, err = time.ParseDuration(c.Schedule.KeepaliveRefreshWindow); err != nil {
		return fmt.Errorf("schedule.keepalive_refresh_window: %w", err)
	}
	if c.KeepaliveRefreshWindowDur <= 0 {
		return fmt.Errorf("schedule.keepalive_refresh_window: %q 必须为正时长（如 168h）", c.Schedule.KeepaliveRefreshWindow)
	}
	if len(c.Schedule.BlackcatHours) == 0 {
		c.Schedule.BlackcatHours = []int{23}
	}
	if len(c.Schedule.GrowthHours) == 0 {
		c.Schedule.GrowthHours = []int{1}
	}
	// 余额后台刷新：启用时 minutes<=0 回落默认 5；关闭时 interval 保持 0（不启动）。
	if c.Schedule.BalanceRefreshEnabled {
		if c.Schedule.BalanceRefreshMinutes <= 0 {
			c.Schedule.BalanceRefreshMinutes = 5
		}
		c.BalanceRefreshInterval = time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
	}
	if err := c.validateScheduleHours(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom/append 模式）。
//
// mode 非法（非 passthrough/custom/append）启动报错，避免静默回落到某一分支；
// custom/append 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认
// （两模式共用同一加载路径，PromptText 均非空）。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（passthrough / custom / append）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// validateScheduleHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (c *Config) validateScheduleHours() error {
	// 任务已显式禁用时不再校验它的小时（BUG-7）：错误文案自己写着「如要关闭该任务
	// 请设 schedule.X_enabled=false」，用户照做之后如果 hours 里还留着哨兵值，
	// 启动会直接失败 —— 文案与行为自相矛盾。
	if c.Schedule.CheckinEnabled {
		if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", c.Schedule.CheckinHours); err != nil {
			return err
		}
	}
	if c.Schedule.TravelEnabled {
		if err := checkHourRange("schedule.travel_hours", "travel_enabled", c.Schedule.TravelHours); err != nil {
			return err
		}
	}
	if c.Schedule.ActivityEnabled {
		if err := checkHourRange("schedule.activity_hours", "activity_enabled", c.Schedule.ActivityHours); err != nil {
			return err
		}
	}
	if c.Schedule.KeepaliveEnabled {
		if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", c.Schedule.KeepaliveHours); err != nil {
			return err
		}
	}
	if c.Schedule.BlackcatEnabled {
		if err := checkHourRange("schedule.blackcat_hours", "blackcat_enabled", c.Schedule.BlackcatHours); err != nil {
			return err
		}
	}
	if c.Schedule.GrowthEnabled {
		return checkHourRange("schedule.growth_hours", "growth_enabled", c.Schedule.GrowthHours)
	}
	return nil
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
