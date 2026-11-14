// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"io"
	"io/fs"
	"log"
	"net/http"
	_ "net/http/pprof" // WB2API_PPROF 打开时提供 /debug/pprof（默认无监听）
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/balance"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// appVersion 网关版本，透出到 /panel/api/overview 与 /status。
//
// 单一版本源（issue ISSUE-05）：构建时由 -ldflags "-X main.appVersion=$(cat VERSION)"
// 注入；此处只是未注入时的兜底。此前硬编码 "1.13.0-panel"，与 APK 的 1.26.x 差了
// 十三个小版本，面板上显示的版本号会让人误判"要不要升级"。
var appVersion = "dev"

// buildStamp 构建时间戳（YYYYMMDDHHMM），由 -ldflags "-X main.buildStamp=..." 注入。
// 留空时版本串就是纯 x.y.z（`go run` / 测试不受影响）。
var buildStamp = ""

// VersionString 对外展示的版本：主版本 + 构建时间，例如 1.35.1-202610221657。
//
// VERSION 文件**仍是 x.y.z 的单一版本源**（格式不变，语义版本不该被构建时间污染）；
// 构建时间只追加在展示串与产物名上，便于区分「同一个版本号下的不同构建」。
func VersionString() string {
	if buildStamp == "" {
		return appVersion
	}
	return appVersion + "-" + buildStamp
}

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

// maskSecret 日志里展示密钥时只留首尾各两位：既能核对「是不是这把」，又不泄露。
func maskSecret(s string) string {
	if s == "" {
		return "(未设置)"
	}
	r := []rune(s)
	if len(r) < 8 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:2]) + strings.Repeat("*", len(r)-4) + string(r[len(r)-2:])
}

// apiKeySource 说明这把密钥是从哪儿来的。
//
// 为什么需要（issue #155）：启动日志此前只打 api_key=true/false，运维分不清
// 「文件里的还是我设的 WB2A_API_KEY 生效了」—— 而这两种情况下改配置文件都不会
// 生效，排查方向完全不同。path 为空表示既没读到文件、也不是 env，走的是内置默认。
func apiKeySource(path string) string {
	if os.Getenv("WB2A_API_KEY") != "" {
		return "环境变量 WB2A_API_KEY"
	}
	if path == "" {
		return "内置默认值"
	}
	return "配置文件 " + path
}

// listenBanner 启动横幅：监听地址 + 密钥掩码 + 密钥来源 + 面板地址。
// 纯函数，便于用测试钉住「密钥不泄露」与「来源如实」。
func listenBanner(listen, path, key string) string {
	return fmt.Sprintf("workbuddy2api listening on %s (api_key=%s, 来源 %s)，管理面板 http://127.0.0.1%s/panel/",
		listen, maskSecret(key), apiKeySource(path), panelListenPath(listen))
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()
	// 实际生效的配置路径。必须在 flag.Parse() **之后**取值：flag.String 返回的指针在
	// Parse 时才写入命令行值，此前取到的是默认值拷贝 —— 用 -config 自定义路径启动时，
	// 横幅会报一份根本没读的 config.json（issue #155 的诉求在自定义路径场景下失效）。
	loadedFrom := *cfgPath

	cfg, err := Load(*cfgPath)
	if err != nil {
		// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置（含随机 api_key）再加载。
			// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
			key, werr := WriteDefault(*cfgPath)
			if werr == nil {
				// 只打掩码：完整 key 写进 config.json 就够了。日志会进 docker logs /
				// journald / logcat / CI 输出并长期留存，能读日志的人拿到 key 即拥有
				// 全量 API、/status 全池账号，admin.enabled 打开时还能触发运维任务。
				// 启动横幅那边一直是掩码（maskSecret），这里此前是漏网。
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，完整值见该文件，可自行修改）",
					*cfgPath, maskSecret(key))
				cfg, err = Load(*cfgPath)
			}
			if werr != nil || err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
				// 日志必须打**生成失败的原因**（review P2-20）：此前打的是 err（原始的
				// 「文件不存在」），写着 auto-generate failed 却报 not exist，排障被带偏。
				why := werr
				if why == nil {
					why = err
				}
				log.Printf("config %s 自动生成失败（改用默认值+环境变量）: %v", *cfgPath, why)
				cfg, err = Load("")
				loadedFrom = "" // 实际没读到任何文件：横幅别报一份没读的路径（issue #155）
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		p.Close()
		_ = store.Close()
	}()
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 闲置补偿调优（从 config 注入，
	// 非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetSoftResetMax(cfg.SoftResetMaxDur)               // 权威重置墙钟上限（soft_reset_max，默认 24h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetCreditFloor(cfg.Pool.CreditFloor)               // 积分保底（默认 0 = 关闭）
	p.SetCreditFloorByRealm(cfg.Pool.CreditFloorByRealm) // 按域覆盖（issue #147）
	p.SetCreditFloorMaxRate(cfg.Pool.CreditFloorMaxRate) // 保底放行倍率上限（0 = 严格）
	// 保留积分（缺省 10，显式 0 = 关闭）+ 账号级路由偏好（issue #62）。
	p.SetReserveCredits(reserveCreditsOf(cfg))
	p.SetReserveCreditsByRealm(cfg.Pool.ReserveCreditsByRealm)
	p.SetAccountRouting(cfg.Pool.AccountPriority, cfg.Pool.AccountShare)
	p.SetStrategy(cfg.Pool.SelectionStrategy) // 选号策略（weighted/round_robin/sequential）
	p.SetBalanceWindow(balanceWindowOf(cfg))  // 窗口用量调度（默认 1h，"0" 关停）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(cfg.Pool.PreferExpiring)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
			// 裸名走 cn（现状零回归）。闭包内部 resolveModel 剥前缀，再按 realm 过滤。
			AvailableForModel:   realmAwareAvailableForModel(p),
			IsAvailableForModel: realmAwareIsAvailableForModel(p), // 快路径单点校验（bug #13）
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	// 出站 DNS 服务器必须在 New 之前注入（报告 #1）：Dialer/Transport 在
	// upstream.New() 里构造，晚于它调用等于永不生效。
	upstream.SetDNSServers(cfg.Upstream.DNSServers)
	up := upstream.New()
	// TLS 指纹档位（node = 复刻官方 Node/OpenSSL 的 JA3/JA4；go = 原生，排障用）。
	// 头部对齐只到 HTTP 层，TLS 在 HTTP 之前就握手完了，这一层才是下一道。
	if err := up.SetTLSFingerprint(cfg.Upstream.TLSFingerprint); err != nil {
		log.Fatalf("CONFIG: %v", err)
	}
	if up.TLSFingerprint == upstream.TLSFingerprintNode {
		log.Printf("[upstream] TLS 指纹复刻已启用（JA3 %s / JA4 %s）",
			upstream.OfficialJA3MD5, upstream.OfficialJA4)
		// 代理由我们接管（proxytunnel.go）：先 CONNECT/SOCKS5 建隧道，再在隧道上
		// 做同一套指纹握手 —— 所以经代理也不再丢 JA3/JA4 与头序。
		// 已知边界：明文 http:// 目标不经代理（我们没有任何 http:// 上游）。
		for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
			if v := os.Getenv(k); v != "" {
				log.Printf("[upstream] 出站代理已接管（%s）：隧道建好后仍按 %s 指纹握手", k, up.TLSFingerprint)
				break
			}
		}
	} else {
		log.Printf("[upstream] TLS 指纹为原生 Go 形态（JA3/JA4 与官方客户端不一致）")
	}
	// 思考档位策略（thinking 节）：启动装载，面板保存配置时热更新。
	up.SetEffortPolicy(effortPolicyOf(cfg))
	// 按轮裁剪历史（0 = 关）。此前这一句被写了两次（合并残留）：改一行以为另一行
	// 负责别的语义，实际两行恒等。
	up.KeepRecentRounds.Store(int64(cfg.Prompt.KeepRecentRounds))
	if cfg.Thinking.Enabled {
		priority := "客户端优先（只在缺档时补默认档）"
		if !cfg.Thinking.ClientPriority {
			priority = "网关优先（改写客户端档位）"
			if cfg.Thinking.OverrideScope == "all" {
				priority = "网关优先（档位与关闭意图一起覆盖）"
			}
		}
		log.Printf("思考档位策略：默认档 %s；%s", cfg.Thinking.DefaultEffort, priority)
	} else {
		log.Printf("思考档位策略已关闭（不补档、不改写客户端档位）")
	}

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （kimi-k3-1 实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 位于 up 装配之后：倍率表由探测下发，闭包每次调用读实时快照。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })
	// 免费号事前不派「免费调不通」的模型（modelTiers accessMode=exclusive）：
	// 只在付费状态**已知为免费**时跳过，没观测过照旧参与。
	p.SetExclusiveModels(up.ModelExclusive, up.PaidUser)

	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// 身份类规则作用域（features.sanitize_brand_scope，缺省 all = 旧行为）。
	up.SanitizeBrandIdentityOnly.Store(cfg.Features.SanitizeBrandScope == "identity")
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientVersionGlobal = cfg.Upstream.ClientVersionGlobal
	up.CliVersionGlobal = cfg.Upstream.CliVersionGlobal
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.DumpDir = cfg.Upstream.DumpDir
	up.DumpRequestMax = cfg.Upstream.DumpMaxBytes
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// （dns_servers 已在 New 之前注入，见上方）
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	// 国内版 base 覆盖（上游 PR #220）：空则保持内置默认，不覆盖。
	if cfg.Cn.ChatBase != "" {
		up.ChatBaseCN = cfg.Cn.ChatBase
	}
	if cfg.Cn.BillingBase != "" {
		up.BillingBaseCN = cfg.Cn.BillingBase
	}
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))

	// 余额采样记录器：与 state 文件同目录（路径由 state 推出，不加配置项）。
	// 采样动作挂在余额刷新之后（见 internal/scheduler），这里只管装配与落盘生命周期。
	balPath := stateSibling(cfg.StateFile, "balance_history.json")
	bal := balance.New(balPath)
	bal.Start()
	defer bal.Stop()
	log.Printf("[balance] 余额趋势采样已启用: %s 每 %v 一点（%s）", balPath, balance.Interval(), bal.Describe())

	sch := scheduler.New(scheduler.Config{
		Pool:     p,
		Upstream: up,
		// 余额采样记录器：挂在余额刷新之后，不额外打上游。
		Balance:                    bal,
		AccountSpreadSeconds:       cfg.Schedule.AccountSpreadSeconds,
		AccountSpreadMinutes:       cfg.Schedule.AccountSpreadMinutes,
		AccountSpreadMinGapSeconds: cfg.Schedule.AccountSpreadMinGapSeconds,
		CheckinHours:               cfg.Schedule.CheckinHours,
		TravelHours:                cfg.Schedule.TravelHours,
		ActivityHours:              cfg.Schedule.ActivityHours,
		KeepaliveHours:             cfg.Schedule.KeepaliveHours,
		GrowthHours:                cfg.Schedule.GrowthHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		GrowthDisabled:     !cfg.Schedule.GrowthEnabled,
		// 周期保活只刷新临期凭证（默认 7 天窗口）；面板手动全量保活不受影响。
		KeepaliveRefreshWindow: cfg.KeepaliveRefreshWindowDur,
		// 保号类四任务是否覆盖禁用账号（缺省 false = 禁用即跳过，保持既有行为）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
		// 启动补跑去重：与 state.json 同目录，重启多次只在当天补一轮。
		RunStampFile: filepath.Join(filepath.Dir(cfg.StateFile), "task-runs.json"),
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}
	switch {
	case !cfg.Schedule.TaskSweepEnabled:
		log.Printf("补领清扫已禁用（schedule.task_sweep_enabled=false）")
	case cfg.TaskSweepInterval > 0:
		log.Printf("补领清扫：每 %s（只 accept 待接受的、只 claim 已达标未领的）", cfg.TaskSweepInterval)
	}
	if cfg.Schedule.IncludeDisabledInTasks {
		log.Printf("保号任务覆盖禁用账号（schedule.include_disabled_in_tasks=true）：禁用号仍签到 / 活跃 / 保活 / 刷新余额，但不参与选号")
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:                 cfg.APIKey,
		SoftCooldown:           cfg.SoftRateDur,
		SanitizeFingerprints:   cfg.Features.SanitizeBlacklistFingerprints,
		EmbeddingsEnabled:      cfg.Features.EmbeddingsEnabled,
		WatermarkStripEnabled:  cfg.Features.WatermarkStripEnabled,
		ToolOnlyRescue:         cfg.Features.ToolOnlyRescue,
		RecordClientInfo:       cfg.Logging.RequestClientInfo,
		RecordContext:          cfg.Logging.RequestArchiveContext,
		ModelFallback:          cfg.Pool.ModelFallback,
		ModelAliases:           livecfg.NormalizeModelAliases(cfg.Models.Aliases),
		HiddenModels:           livecfg.NormalizeHiddenModels(cfg.Models.Hidden),
		ThinkingEnabled:        cfg.Thinking.Enabled,
		ThinkingDefaultEffort:  cfg.Thinking.DefaultEffort,
		ThinkingClientPriority: cfg.Thinking.ClientPriority,
		ThinkingOverrideScope:  cfg.Thinking.OverrideScope,
		ThinkingModelDefaults:  cfg.Thinking.ModelDefaults,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	// 请求指标始终启用；JSONL 归档只写脱敏元数据，写盘失败不影响聊天请求。
	requestLog := reqlog.New(reqlog.Config{
		Dir:           stateSibling(cfg.StateFile, "request-logs"),
		Enabled:       cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes:      int64(cfg.Logging.RequestArchiveMaxMB) << 20,
	})
	defer requestLog.Close()
	rs := requestLog.Snapshot().Archive
	if rs.Enabled {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档 %s（保留 %d 天，上限 %d MiB）",
			rs.Dir, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档已关闭")
	}

	// 积分历史：把每次**真实查到**的余额与上一次比对，变动即留痕（新账号首次只建
	// 基线）。挂载在余额查询路径上（upstream 的余额观察者），签到 / 活跃上报 /
	// 旅行 / 保活 / 面板手动刷新全覆盖 —— 上游只在少数渠道落金额日志，比对余额是
	// 唯一可靠口径。与 state/usage/request-logs 同目录，随 state_file 搬移。
	creditHist := credithist.New(stateSibling(cfg.StateFile, "credit-history.json"), 2000)
	up.SetCreditObserver(creditHist.Observe)
	log.Printf("[credithist] 积分历史已启用: %s", stateSibling(cfg.StateFile, "credit-history.json"))

	pn := panel.New(panel.Config{
		Pool:        p,
		Balance:     bal, // 余额趋势采样记录器（nil 时该视图返回 501）
		Usage:       rec,
		RequestLog:  requestLog,
		Upstream:    up,
		Scheduler:   sch,
		AuthDir:     cfg.AuthDir,
		APIKey:      cfg.APIKey,
		RedisMode:   redisMode,
		StickyCount: sessCount,
		Version:     VersionString(),
		Live:        live,
		// 降级路由表：面板据此在限流提示里标注「已路由至 <等价模型>」。
		ModelFallback: func() map[string]string { return live.Load().ModelFallback },
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		// 积分历史账本（只读展示；写入由上面的余额观察者完成）。
		CreditHistory: creditHist,
		ProbeFile:     stateSibling(cfg.StateFile, "output_probes.json"),
		// 任务跳过名单（v1.22.0）：attempt 开关 + 反复无效的 (账号,任务码)。
		// 与 state 同目录，容器/自托管部署跟着 state 卷一起走。
		SkipFile:          stateSibling(cfg.StateFile, "task_skip.json"),
		ConfigPath:        *cfgPath,
		GrowthConcurrency: cfg.Schedule.GrowthConcurrency,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveConfig(raw, *cfgPath, live, p, up, sch)
		},
	})
	livePanel.Store(pn)
	// 账号级后台任务作业（taskjobs.go）：与 state 同目录落盘，重启后仍在。
	// 启动失败不致命（只是没有作业历史），但必须留日志 —— 静默降级会让
	// 「面板上作业历史一直空」变成查不出原因的现象。
	if err := pn.StartTaskJobs(stateSibling(cfg.StateFile, "task-jobs.json")); err != nil {
		log.Printf("WARN: [panel] 作业记录启动失败（面板作业历史不可用）: %v", err)
	}
	// 面板运行日志全频道按天落盘（logging.panel_log_*）：重启后回填、可按区间导出。
	// 装配顺序必须在 log.SetOutput 之前 —— 回填完再把实时输出接上，否则启动期那几行
	// 会先入环、再被回填的历史插到前面，时间就乱了。
	var panelLogs *panel.LogStore
	if cfg.Logging.PanelLogEnabled {
		panelLogs = panel.NewLogStore(stateSibling(cfg.StateFile, "panel-logs"),
			cfg.Logging.PanelLogRetentionDays, cfg.Logging.PanelLogMaxMB)
		if panelLogs == nil {
			log.Printf("WARN: 面板日志落盘不可用（数据目录建不出来），退回纯内存环")
		} else {
			if err := pn.Logs().SetLogStore(panelLogs); err != nil {
				log.Printf("WARN: 面板日志回填失败（不影响新日志）: %v", err)
			}
			// 落盘在 Ring 的后台 goroutine 里做：退出前先把排队的行写完，再关文件
			// （defer 后进先出：这条在 Close 之前执行）。
			defer pn.Logs().Flush()
			defer panelLogs.Close()
			log.Printf("[panel] 运行日志落盘 %s（保留 %d 天，上限 %d MiB）",
				panelLogs.Dir(), cfg.Logging.PanelLogRetentionDays, cfg.Logging.PanelLogMaxMB)
		}
	}
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))

	// 成长任务排程接线（issue ISSUE-15）：scheduler 的 growth_hours 到点会调
	// GrowthHook，而全仓从未 SetGrowthHook —— 到点什么都不做，面板却写着
	// "成长任务自动推进"。panel 构造晚于 scheduler，所以在这里补挂。
	// 队列内部已按账号串行、账号间并发、并遵守跳过名单与尝试型开关。
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)
	// 国际版网页活跃腿（缺省关）：这条腿每天给每个国际号建一条真实会话、占一条活跃
	// 名额（上限 10），默认开就是替用户做决定。
	sch.SetWebActivityDisabled(!cfg.Schedule.WebActivityEnabled)
	// 补领清扫执行器（issue #174）：分钟级只 accept/claim，不跑行为链路。
	// 与按钮共用同一执行体（panel.RunTaskSweepNow → sweepAccount），口径不会漂移。
	sch.SetSweepRunner(pn.RunTaskSweepNow)

	// /admin 操作审计（缺省关闭）：开一次失败就 Fatal —— 审计开不起来还继续跑，
	// 等于承诺了「操作有据可查」却拿不出证据。
	var auditLog *server.AuditLog
	if cfg.Admin.AuditEnabled {
		al, aerr := server.NewAuditLog(cfg.Admin.AuditFile, cfg.APIKey)
		if aerr != nil {
			p.Flush() // Fatal 绕过 defer：至少把池状态落盘再退
			log.Fatalf("admin audit: %v", aerr)
		}
		auditLog = al
	}

	h := server.NewHandler(server.Config{
		Pool:           p,
		Upstream:       up,
		APIKey:         cfg.APIKey,
		Session:        sessRouter,
		StickyCount:    sessCount,
		RedisMode:      redisMode,
		SoftCooldown:   cfg.SoftRateDur,
		MaxBodyMB:      cfg.ServerMaxBodyMB,
		MetricsEnabled: cfg.MetricsEnabled,
		// 11140 连坐全池暂停（upstream.restriction_hold_seconds，启动时读取）。
		RestrictionHold: cfg.RestrictionHoldDur,
		// 首帧心跳：等上游首帧期间补 SSE 注释帧（server.head_keepalive，缺省 1.5s）。
		HeadKeepalive: cfg.HeadKeepaliveDur,
		// 6020/6021 渠道排队：网关内先等一轮（features.busy_retry / busy_wait_seconds）。
		// DeferredToolCall 残缺参数修复（features.repair_deferred_tool_args，默认关；
		// 启动时读取）。只影响 DeferredToolCall，其它工具零改写。
		RepairDeferredToolArgs: cfg.Features.RepairDeferredToolArgs,
		BusyRetry:              cfg.Features.BusyRetry,
		BusyWaitMax:            time.Duration(cfg.Features.BusyWaitSeconds) * time.Second,
		// 运维接口（手动补跑排程）+ 操作审计：整段缺省关闭。
		AdminEnabled:                  cfg.Admin.Enabled,
		AdminAPIKey:                   cfg.AdminAPIKey,
		WatermarkStripEnabled:         cfg.Features.WatermarkStripEnabled,
		EmbeddingsEnabled:             cfg.Features.EmbeddingsEnabled,
		ToolOnlyRescue:                cfg.Features.ToolOnlyRescue,
		Tasks:                         sch,
		GrowthRunner:                  pn.RunGrowthQueueOnce,
		Audit:                         auditLog,
		ModelsAllowed:                 cfg.Models.Allowed, // 模型白名单（issue #68）
		ModelsHidden:                  cfg.Models.Hidden,  // 模型黑名单（上游 PR #225）
		ModelsAliases:                 cfg.Models.Aliases, // 模型名映射（issue #30）
		Panel:                         pn,
		Live:                          live,
		Usage:                         rec,
		RequestLog:                    requestLog,
		PromptMode:                    cfg.Prompt.Mode,
		PromptText:                    cfg.PromptText,
		PromptIdentityOverrideEnabled: cfg.Prompt.IdentityOverrideEnabled,
		PromptIdentityOverrideText:    cfg.Prompt.IdentityOverrideText,
		// 日预算 + 两道快速闸（0 = 关闭）。
		DailyBudget:      cfg.Budget.DailyBudget,
		BudgetStateFile:  cfg.BudgetStateFile,
		BurstCreditLimit: cfg.Budget.BurstCreditLimit,
		CacheLowRate:     cfg.Budget.CacheLowRate,
		// 模型降级路由表：读 live 快照（支持面板热更，改完即生效、无需重启）。
		ModelFallback: func() map[string]string { return live.Load().ModelFallback },
		// 来源记录开关经 livecfg 热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		RecordContext:    cfg.Logging.RequestArchiveContext,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled: cfg.Global.Enabled,
	})

	// 接管 SIGPIPE：Go 运行时对 **stdout/stderr** 写 EPIPE 有特殊对待 —— 直接以
	// SIGPIPE 退出（exit code 141）。安卓看门狗把网关 stdout 接到管道上，一旦读取
	// 线程退出/关闭读端（旧版 pump 写满 2MB 就 break 正是如此），网关下一次打日志
	// 就会被内核打死，而它其实是健康的。signal.Notify 之后：写失败返回 EPIPE、
	// 进程继续跑（日志丢了就算了，服务不能死）。
	sigpipe := make(chan os.Signal, 1)
	signal.Notify(sigpipe, syscall.SIGPIPE)
	go func() {
		for range sigpipe {
			// 丢弃：管道断了就当日志丢失，不致命。
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	// 人味化参数（缺省 0 = 旧行为）：抖动比例先设，StartBalanceRefresh 每轮现读。
	sch.SetBalanceJitter(cfg.Schedule.BalanceRefreshJitterPermill)
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)
	// 补领清扫（缺省 15 分钟）：人工不点按钮时，达标未领的奖励也照样被领掉。
	sch.SetTaskSweepJitter(cfg.Schedule.TaskSweepJitterPermill)
	sch.StartTaskSweep(ctx, cfg.TaskSweepInterval)

	// 账号目录热重载（issue #199）：外部脚本往 auths/ 同步文件后不必重启进程。
	// 间隔 "0s" 关闭轮询，手动接口仍然可用。启动首轮会先对齐一次。
	pn.SetReloadConfig(cfg.ReloadIntervalDur, cfg.Accounts.ReloadDeleteConfirm)
	go pn.ReloadLoop(ctx.Done())

	// 启动即预热模型积分倍率表：倍率只在 FetchModels/FetchGlobalModelInfos 成功时
	// 填充（两者均懒触发），重启后到首次 /v1/models 或面板模型页被访问之前，
	// ModelRate 恒返回空串——积分保底的目录兜底在这段空窗期内形同虚设，触底号
	// 会被当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费
	// 模型归零；倍率表当时尚未建立）。
	// 异步执行：不阻塞监听启动；失败仅记日志（下一轮懒触发或本轮重试仍可补上）。
	go warmModelRates(ctx, up, p)

	// 账号池可用性告警（alerting.*，缺省关闭）：连续 N 个 tick 都坏才推 webhook，
	// 连续 M 个 tick 都恢复才解除——单次抖动不该把人半夜叫起来。观测源直接取
	// handler 的池健康与 WAF 状态（见 server/alert_source.go）。
	alertMon := alert.New(alert.Config{
		Enabled:          cfg.Alerting.Enabled,
		WebhookURL:       cfg.Alerting.WebhookURL,
		Secret:           cfg.Alerting.Secret,
		Interval:         time.Duration(cfg.Alerting.IntervalSeconds) * time.Second,
		Timeout:          time.Duration(cfg.Alerting.TimeoutSeconds) * time.Second,
		StartupGrace:     time.Duration(cfg.Alerting.StartupGraceSeconds) * time.Second,
		MinHealthyCN:     cfg.Alerting.MinHealthyCN,
		MinHealthyGlobal: cfg.Alerting.MinHealthyGlobal,
		RecoverHealthy:   cfg.Alerting.RecoverHealthy,
		BreakerThreshold: cfg.Alerting.BreakerThreshold,
		ForTicks:         cfg.Alerting.ForTicks,
		ClearTicks:       cfg.Alerting.ClearTicks,
		SendResolve:      cfg.Alerting.SendResolve,
	}, h.AlertSource())
	go alertMon.Run(ctx)
	// 上游目录改版监控（watchdog.*，缺省关闭）：盯住「客户端实际看到的 /v1/models」
	// 的指纹，变了就经告警通道推一条（没配 webhook 时只写日志）。首轮只建基线。
	if cfg.Watchdog.Enabled {
		go h.WatchCatalog(ctx, time.Duration(cfg.Watchdog.IntervalSeconds)*time.Second, alertMon.Notify)
		log.Printf("watchdog: 上游目录监控开，每 %ds", cfg.Watchdog.IntervalSeconds)
	}

	// 诊断用：WB2API_PPROF=127.0.0.1:6060 时另起一个只跑 pprof 的监听。
	// 默认不开启（不设环境变量就完全没有这个监听），供「并发一高就降速」这类只能靠
	// 真实负载定位的 CPU 问题取 profile（取完即关，不常驻生产）。
	if addr := os.Getenv("WB2API_PPROF"); addr != "" {
		go func() {
			log.Printf("[pprof] 诊断监听开启 %s（CPU: /debug/pprof/profile?seconds=30）", addr)
			if err := http.ListenAndServe(addr, nil); err != nil {
				log.Printf("WARN: [pprof] 监听失败 %v", err)
			}
		}()
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
		// 缺省 300s（issue #100：旧固定 60s 会掐掉大上下文/文件块经反代链的慢速上传，
		// 客户端收到 400 "read body: ... i/o timeout"）；server.read_timeout="0" 显式关闭。
		// 注意：请求体另有 server.max_body_mb 上限（handler 的 MaxBytesReader，默认 64MB）；
		// 早先这里写的是「已无网关侧上限」，与实现相反，已更正（BUG-16）。
		// 改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		pn.StopTaskJobs() // 作业记录：停节流循环并强制落盘
		p.Flush()         // 信号触发：先落盘再做优雅停机
		// 在途请求排干窗口可配（server.shutdown_timeout，缺省 30s）：长流式 SSE 的
		// 合法时长可达分钟级，固定 5s 必然把在途对话掐成连接重置；超时也不再静默。
		//
		// "0" 必须是「不等待直接退」而不是回落到某个默认值：配置文档这么写，代码就得
		// 这么做，否则「我配了 0 怎么还等了 5 秒」。用 srv.Close() —— 立刻断监听与全部
		// 在途连接。（负值在 config.normalize 已 fail fast，这里不会出现。）
		if shutdownTimeout := cfg.ShutdownTimeoutDur; shutdownTimeout <= 0 {
			_ = srv.Close()
		} else {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				log.Printf("WARN: 优雅关停未在 %s 内完成，在途请求被放弃: %v", shutdownTimeout, err)
			}
		}
	}()

	log.Print(listenBanner(cfg.Listen, loadedFrom, cfg.APIKey))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		p.Flush() // Fatal 绕过所有 defer：至少把池状态落盘再退，否则丢整轮运行状态
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么需要：倍率表只在 FetchModels（CN）/ FetchGlobalModelInfos（global）成功时
// 填充，两者都是懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发
// 之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出收费，触底号会被
// 当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——后续懒触发仍会补上）；global 域
// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelRates(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.PickExistingAuth(uids); a != nil { // 依次回退（bug P1-2）
			if _, err := up.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	if up.GlobalEnabled && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.PickExistingAuth(uids); a != nil { // 依次回退（bug P1-2）
				// FetchGlobalModelInfos 无错误返回（内部负缓存自行节流），
				// 仅按结果条数判断是否拿到目录。
				infos := up.FetchGlobalModelInfos(a)
				if len(infos) == 0 && up.GlobalProbeInFlight() {
					// 探测在飞时 fetchGlobalModelsOnce 直接返回 nil（不打上游、不等待），
					// 此前一律记 WARN 把「正在探测」误报成「上游没模型」（报告 #16）。
					// 面板侧早已用 GlobalProbeInFlight 区分，这里补齐。
					log.Printf("[upstream] warm model rates: global 探测在飞，跳过本轮预热")
				} else if len(infos) == 0 {
					log.Printf("WARN: [upstream] warm model rates (global): empty model list")
				} else {
					log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
				}
			}
		}
	}
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval/SetPreferExpiring/SetCreditFloor/SetCreditFloorByRealm
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval/SetExpiringSoonWindow
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
// saveConfigMu 串行化配置保存：tmp 文件名是固定的 path+".tmp"，两个并发
// POST /panel/api/config 会同时写同一路径再 rename，可能产出交错的 JSON（损坏）
// 并互相覆盖结果（review P1-12）。面板是单用户场景，一把包级锁足够。
var saveConfigMu sync.Mutex

// livePanel 面板实例的进程级引用：面板「保存配置」这条路径在 panel.New 的参数闭包里
// 就要能回调面板自身（成长队列并发热生效），而那时 pn 还没赋值。
var livePanel atomic.Pointer[panel.Panel]

func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) ([]string, error) {
	saveConfigMu.Lock()
	defer saveConfigMu.Unlock()
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	// mergeConfigMaps 是**原地**合并（返回的就是 cur），所以先留一份原始快照，
	// 否则后面拿 cur 做「改了什么」的差异比对时两边永远是同一个对象。
	var prev map[string]any
	_ = json.Unmarshal(oldRaw, &prev)
	merged := mergeConfigMaps(cur, incoming)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	// 顺序必须是「解析 → 叠 env → 校验」（bug B1 / P0-1）：
	// ① ParseConfig 内部就 normalize，而 checkListenKey 要看 api_key —— 用
	//    WB2A_API_KEY 起服务时文件里那个键是空的，校验会先把保存请求拒掉；
	// ② 即便校验通过，直接把 newCfg 灌进 livecfg 也会把 env 提供的密钥/软冷却/
	//    脱敏开关覆盖成文件值（常为空），鉴权当场失效而日志还说"已立即生效"。
	// 所以这里跟 Load 走同一套：Default → ApplyJSON → applyEnv → normalize。
	newCfg := Default()
	if err := ApplyJSON(mergedJSON(merged), newCfg); err != nil {
		return nil, err
	}
	applyEnv(newCfg)
	if err := newCfg.normalize(); err != nil {
		return nil, err
	}

	// 3) 落盘用**规范化后**的值（bug B7）：此前直接写 merged（用户提交的原始值），
	// 于是一个会被 normalize() 修正/补默认的字段呈现「进程内按修正值跑、重启后又变回
	// 文件里的原值」—— 用户看到「改了没变 / 重启后行为又变了」。
	//
	// 注意不能直接写 newCfg：它已经叠过 WB2A_* 环境变量，落盘会把环境变量里的
	// 密钥/地址写进文件（env 该保持「外部注入」语义）。所以这里用**不叠 env** 的
	// 一份副本做规范化；它可能因「密钥只在 env 里」而在 checkListenKey 处提前失败，
	// 那种情况下文件本身没有可规范化的密钥，保持原样即可（错误忽略）。
	if diskCfg := Default(); ApplyJSON(mergedJSON(merged), diskCfg) == nil {
		_ = diskCfg.normalize() // 部分场景会提前返回：落盘不需要校验结论
		if b, merr := json.Marshal(diskCfg); merr == nil {
			var normMap map[string]any
			if json.Unmarshal(b, &normMap) == nil && normMap != nil {
				// 未知键（用户手写、结构体没有的字段）从 merged 补回，避免保存时被抹掉。
				merged = fillUnknownKeys(normMap, merged)
			}
		}
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	// 用 auth.ReplaceAtomic（带 fsync + 单文件 bind mount 的 EBUSY 兜底）：面板保存是
	// config.json 的唯一写入路径，而 api_key 只存在于这一份文件里。此前是
	// os.WriteFile + Rename、中间没有 fsync —— 点保存后立刻断电/被 SIGKILL，可能留下
	// 0 字节或半截 config.json，下次启动 Load 直接 parse 失败 log.Fatalf 起不来，
	// 随机生成的 api_key 也一起没了。同一失败模式在 WriteDefault（bug #15）与
	// auth.ReplaceAtomic 里都已经修过，只有这条路没对齐。
	if werr := auth.ReplaceAtomic(path, out, 0o600); werr != nil {
		if !errors.Is(werr, syscall.EBUSY) {
			return nil, fmt.Errorf("write config: %w", werr)
		}
		// EBUSY：单文件 Docker bind mount 不能 rename 覆盖挂载点，退回原地写
		// （原地写内部仍有 f.Sync()，掉电安全性与 ReplaceAtomic 同档）。
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, out, 0o600); err != nil {
			return nil, fmt.Errorf("write config: %w", err)
		}
		if err := os.Rename(tmp, path); err != nil {
			if !errors.Is(err, syscall.EBUSY) {
				return nil, fmt.Errorf("replace config: %w", err)
			}
			f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
			if openErr != nil {
				_ = os.Remove(tmp)
				return nil, fmt.Errorf("replace config (bind mount fallback): %w", openErr)
			}
			_, writeErr := f.Write(out)
			if writeErr == nil {
				writeErr = f.Sync()
			}
			closeErr := f.Close()
			// 写失败时保留 tmp（挂载文件已被 O_TRUNC 破坏，tmp 里是完整新内容，
			// 可手工恢复）；写成功才清理。
			if writeErr != nil {
				return nil, fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", tmp, writeErr)
			}
			_ = os.Remove(tmp)
			if closeErr != nil {
				return nil, fmt.Errorf("replace config (bind mount fallback): %w", closeErr)
			}
		}
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	live.Store(livecfg.Snapshot{
		APIKey:                newCfg.APIKey,
		SoftCooldown:          newCfg.SoftRateDur,
		SanitizeFingerprints:  newCfg.Features.SanitizeBlacklistFingerprints,
		EmbeddingsEnabled:     newCfg.Features.EmbeddingsEnabled,
		WatermarkStripEnabled: newCfg.Features.WatermarkStripEnabled,
		ToolOnlyRescue:        newCfg.Features.ToolOnlyRescue,
		RecordClientInfo:      newCfg.Logging.RequestClientInfo,
		RecordContext:         newCfg.Logging.RequestArchiveContext,
		// review #3：这两个表是 handler 每次请求从 live 快照读的，漏填等于「保存任意
		// 配置 → 运行时降级表/别名表被清空」，直到进程重启才恢复。
		ModelFallback: newCfg.Pool.ModelFallback,
		ModelAliases:  livecfg.NormalizeModelAliases(newCfg.Models.Aliases),
		HiddenModels:  livecfg.NormalizeHiddenModels(newCfg.Models.Hidden),
		// 思考档位策略镜像：与 up.SetEffortPolicy 同批热更新，面板立即回显新生效值。
		ThinkingEnabled:        newCfg.Thinking.Enabled,
		ThinkingDefaultEffort:  newCfg.Thinking.DefaultEffort,
		ThinkingClientPriority: newCfg.Thinking.ClientPriority,
		ThinkingOverrideScope:  newCfg.Thinking.OverrideScope,
		ThinkingModelDefaults:  newCfg.Thinking.ModelDefaults,
	})
	up.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	up.SanitizeBrandIdentityOnly.Store(newCfg.Features.SanitizeBrandScope == "identity")
	// 思考档位策略热生效：改默认档 / 切换客户端优先或作用域后，下一次 chat 即按新策略。
	up.SetEffortPolicy(effortPolicyOf(newCfg))
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetSoftResetMax(newCfg.SoftResetMaxDur)               // 权威重置墙钟上限热生效
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	p.SetCreditFloor(newCfg.Pool.CreditFloor)               // 积分保底热生效（0 = 关闭）
	p.SetCreditFloorByRealm(newCfg.Pool.CreditFloorByRealm) // 按域覆盖热生效（issue #147）
	p.SetCreditFloorMaxRate(newCfg.Pool.CreditFloorMaxRate) // 保底放行倍率上限热生效
	p.SetReserveCredits(reserveCreditsOf(newCfg))           // 保留积分热生效
	p.SetReserveCreditsByRealm(newCfg.Pool.ReserveCreditsByRealm)
	p.SetAccountRouting(newCfg.Pool.AccountPriority, newCfg.Pool.AccountShare)
	p.SetStrategy(newCfg.Pool.SelectionStrategy) // 选号策略热生效
	p.SetBalanceWindow(balanceWindowOf(newCfg))  // 窗口用量调度热生效
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	sch.SetKeepaliveRefreshWindow(newCfg.KeepaliveRefreshWindowDur) // 临期窗口热生效
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours,
		newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled,
		!newCfg.Schedule.GrowthEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	sch.SetBalanceJitter(newCfg.Schedule.BalanceRefreshJitterPermill) // 人味化抖动热生效
	sch.SetTaskSweepInterval(newCfg.TaskSweepInterval)                // 补领清扫间隔热生效
	sch.SetTaskSweepJitter(newCfg.Schedule.TaskSweepJitterPermill)
	// 成长队列并发热生效：saveConfig 先于面板构造可用（面板内部保存配置），
	// 所以走一个构造后才填充的引用，而不是加参数（那会与 panel.New 的参数闭包成环）。
	if pp := livePanel.Load(); pp != nil {
		pp.SetGrowthConcurrency(newCfg.Schedule.GrowthConcurrency)
	}
	sch.SetIncludeDisabledInTasks(newCfg.Schedule.IncludeDisabledInTasks)
	sch.SetWebActivityDisabled(!newCfg.Schedule.WebActivityEnabled)

	// 比对前把 prev 也规范化（bug #12）：prev 是**原始文件**反序列化结果，而 merged
	// 是规范化后的结构。配置文件里缺省某个键时规范化会补默认值，
	// DeepEqual(nil, "120") 直接不等 → 用户什么都没改也会被告知"这些项需重启"。
	// 触发面：手写的最小配置（只写 listen/api_key/auth_dir）必误报。
	return restartRequiredFields(normalizeForCompare(oldRaw, prev), merged), nil
}

// effortPolicyOf 把 config thinking 节转成 upstream 运行时策略。
// 未启用时 Enabled=false，管线行为与未接入策略时完全一致。
func effortPolicyOf(cfg *Config) *upstream.EffortPolicy {
	if cfg == nil {
		return nil
	}
	return &upstream.EffortPolicy{
		Enabled:        cfg.Thinking.Enabled,
		DefaultEffort:  cfg.Thinking.DefaultEffort,
		ClientPriority: cfg.Thinking.ClientPriority,
		OverrideScope:  cfg.Thinking.OverrideScope,
		ModelDefaults:  cfg.Thinking.ModelDefaults,
	}
}

// restartRequiredFields 返回本次改动中**真的变了**、且无法热生效需要重启的字段名。
//
// 此前它恒返回全量清单：改个能热生效的字段也照样提示「需要重启」，用户照着重启了
// 一遍发现没变化，久而久之就无视这条提示了（狼来了）。现在按前后 JSON 快照做差异，
// 只有值真的变了才列出。
// normalizeForCompare 把「原始文件 map」过一遍与保存路径相同的规范化，
// 让 restartRequiredFields 的前后两侧口径一致（bug #12）。
// 规范化失败（文件本身非法）时退回原始 map —— 宁可多报，不要因为比对口径崩掉保存。
func normalizeForCompare(raw []byte, orig map[string]any) map[string]any {
	cfg := Default()
	if err := ApplyJSON(raw, cfg); err != nil {
		return orig
	}
	_ = cfg.normalize() // 与保存路径一致：可能因 env-only 密钥提前返回，忽略
	b, err := json.Marshal(cfg)
	if err != nil {
		return orig
	}
	var norm map[string]any
	if json.Unmarshal(b, &norm) != nil || norm == nil {
		return orig
	}
	// 用户手写的未知键补回后再比较（否则它们会被判成"被删了"）。
	return fillUnknownKeys(norm, orig)
}

func restartRequiredFields(prev, next map[string]any) []string {
	// 这些字段在进程内被监听地址/HTTP client/目录句柄等装配期对象捕获，改完必须重启。
	keys := []string{
		"listen", "auth_dir", "state_file",
		"upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds",
		"upstream.user_agent",
		// 以下字段在启动期被装配进 upstream.Client / server.Handler / auth 包级开关，
		// 保存路径不碰它们（bug B2）：此前清单漏列 → 面板提示"已立即生效"，
		// 实际行为直到重启都不变，用户反复点保存怀疑面板坏了。
		"upstream.client_version", "upstream.cli_version", "upstream.client_name",
		"upstream.device_token", "upstream.device_token_file", "upstream.passthrough_ip",
		"global.enabled", "global.chat_base", "global.billing_base",
		"cn.chat_base", "cn.billing_base", // base 在启动时注入（上游 PR #220）
		"models.hidden",        // 列表过滤与点名拒绝在 Handler 构造时快照（上游 PR #225）
		"models.allowed",       // 同上（issue #68）
		"upstream.dns_servers", // Dialer 在启动时构造（上游 PR #144）
		"prompt.mode", "prompt.file",
		"server.max_body_mb",
		"upstash",
		// enabled 决定启动时是否创建 Router（main.go 初始化段），saveConfig 不做热应用 ——
		// 漏了它会让面板提示「已保存并立即生效」，实际要重启（BUG-4）。
		"session_sticky.enabled", "session_sticky.ttl", "session_sticky.gc_interval",
		"logging.request_archive_enabled", "logging.request_retention_days", "logging.request_archive_max_mb",
		"server.read_timeout", "server.shutdown_timeout",
		// account_spread_seconds 在 Scheduler 构造时读入 cfg（报告 #5）：保存路径没有
		// 对应 setter，不列进来就会提示「已保存并立即生效」，实际要重启才生效。
		"schedule.account_spread_seconds",
		// review #4：以下键只在启动期装配，saveConfig 没有对应 setter —— 漏列会
		// 让面板提示「已保存并立即生效」，实际要重启才生效。
		// pool.model_fallback / models.aliases 不在此列（review #12）：handler 每次
		// 请求从 live 快照读，保存即生效 —— 列进来会诱导用户做无意义的重启。
		"prompt.identity_override_enabled", "prompt.identity_override_text", // handler 构造时读入
		"upstream.dump_dir", "upstream.dump_max_bytes", // up.DumpDir 启动期装配
		"schedule.account_spread_minutes", "schedule.account_spread_min_gap_seconds", // scheduler 构造时读入
	}
	var out []string
	for _, k := range keys {
		if !reflect.DeepEqual(lookupDotted(prev, k), lookupDotted(next, k)) {
			out = append(out, k)
		}
	}
	return out
}

// lookupDotted 按 "a.b.c" 路径取嵌套 map 里的值（缺失返回 nil）。
func lookupDotted(m map[string]any, path string) any {
	var cur any = m
	for _, seg := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = mm[seg]
		if !ok {
			return nil
		}
	}
	return cur
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// fillUnknownKeys 把 src 里 dst 没有的键补进 dst（递归），返回 dst。
// 用途：落盘用「规范化后的结构体」时，用户手写的未知键不会被抹掉（bug B7）。
func fillUnknownKeys(dst, src map[string]any) map[string]any {
	for k, v := range src {
		if cur, ok := dst[k]; ok {
			if dm, ok1 := cur.(map[string]any); ok1 {
				if sm, ok2 := v.(map[string]any); ok2 {
					dst[k] = fillUnknownKeys(dm, sm)
				}
			}
			continue
		}
		dst[k] = v
	}
	return dst
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// balanceWindowOf 解析 pool.balance_window：缺省 1h，"0"/空串之外的非正值一律关停。
func balanceWindowOf(cfg *Config) time.Duration {
	raw := strings.TrimSpace(cfg.Pool.BalanceWindow)
	if raw == "" {
		return pool.DefaultBalanceWindow()
	}
	if raw == "0" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// reserveCreditsOf 把 config 的保留积分指针解成生效值（nil 已在 normalize 补默认 10，
// 这里再兜一次 0 = 关闭；负值不可达 —— normalize 已钳）。
func reserveCreditsOf(cfg *Config) int64 {
	if cfg.Pool.ReserveCredits == nil {
		return 10
	}
	if v := *cfg.Pool.ReserveCredits; v > 0 {
		return v
	}
	return 0
}
