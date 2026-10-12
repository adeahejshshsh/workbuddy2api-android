# workbuddy2api 在本机（Android / DeepSeek Harness）跑起来了 —— 移植报告

> 仓库：<https://github.com/linguo2625469/workbuddy2api-panel>（Go 1.22.5 单二进制 + 内嵌 Web 面板）
> 本机：Android 15 / aarch64 / Termux 风格前缀（`com.dsharnessmobile.shell`）
> 结论：**无官方兼容产物（仓库明确无 Release，Docker 镜像也用不上），已完整移植并成功运行。**

## 一、当前状态（已验证）

| 项 | 结果 |
|---|---|
| 服务 | 运行中，监听 `:7863` |
| 管理面板 | <http://127.0.0.1:7863/panel/> → HTTP 200（72 KB 内嵌单页） |
| 健康检查 | `GET /healthz` → `{"healthy":0,...,"total":0,"service":"workbuddy2api"}`（503 是因为**还没绑账号**） |
| OpenAI 兼容接口 | `GET /v1/models`（带 key）→ `{"data":[],"object":"list"}`；不带 key → `401` |
| 面板 API | `GET /panel/api/overview` → 200 |
| api_key | 见 `workbuddy2api-panel/config.json` 的 `api_key` |
| 二进制 | `$PREFIX/lib/workbuddy2api/`：`server`(11M) `signin` `login` `credit` `trial` |

**唯一还没做的**：绑定你自己的 CodeBuddy（腾讯）账号 —— 那需要你的账号授权，我替不了。

## 二、为什么不能「直接下个产物就跑」

仓库无 Release/tag，产物只能源码自构建；而本机环境有 **四道硬墙**：

### 墙 1：官方 Go 工具链根本不能执行
官方 `go1.x.linux-arm64.tar.gz` 里的 `go` 是 **ET_EXEC（非 PIE）**，Android 的 linker 直接拒绝：
`error: ".../go" has unexpected e_type: 2`。
→ **改用 Termux 官方仓库的 `golang` 包**（Patch 成 PIE，`e_type=3`）：
从 <https://packages.termux.dev/apt/termux-main/pool/main/g/golang/> 取 `golang_3:1.27.1_aarch64.deb`，
用 `dpkg-deb -x` 手工解包（本机 `apt` 因前缀不匹配是坏的：它硬编码 `/data/data/com.termux/files/usr`），
装到 `$PREFIX/lib/go`。

### 墙 2：本机禁止 exec 私有目录里的任何文件
`/storage/emulated/0` 是 **noexec** 挂载；app data 里的文件 `execve()` 一律 **EACCES**
（本 App targetSdk ≥ 29；Termux 自己靠 targetSdk 28 规避）。
所以所有二进制都得靠 `LD_PRELOAD=libtermux-exec-ld-preload.so` +
`TERMUX_EXEC__SYSTEM_LINKER_EXEC__MODE=force` → **改由 `/system/bin/linker64` 代跑**。

### 墙 3：Go 的 argv 被「代跑」搞错位（最刁的一环）
代跑时内核栈变成 `[发起方 argv0, 程序绝对路径, 真实参数...]`（多 2 格）；
bionic libc 会用 `initial_linker_arg_count` 自行修正 1 格，**Go 不会** ——
Go 的 `_rt0_arm64` 直接从栈读 argc/argv，于是 `os.Args` 永远错位：
`go version` 报 `go <自己的路径>: unknown command`，`gofmt` 把自己的二进制当源码解析。

实测栈布局（自制探针）：
```
stack argc=4
  [0]= ./probe2                     ← 发起方 argv0
  [1]= /data/.../probe2             ← 绝对路径（多出来的这格）
  [2]= A1   [3]= A2
```
→ **补丁**：把 `_rt0_arm64` 的取参改成 `argc-1 / argv+16`（与 bionic 语义对齐）。
工具链里已有二进制用 `patches/patch_rt0.py` 等长改机器码，源码版见 `patches/patch_source.sh`。

### 墙 4：`go build` 要 spawn 子工具，而 Go 走 raw syscall（shim 拦不住）
`compile`/`link`/`asm`/`pack` 必须被 exec —— 但 Go 用裸 syscall，shim 的 PLT 拦截无效 → EACCES。
→ **`GOFLAGS=-toolexec=/system/bin/linker64`**：execve 目标是系统文件（允许），
argv 恰好是 `[linker64, 工具绝对路径, 工具参数...]`，bionic 手动模式正好把 **argv[1] 当程序加载** → 工具正常跑，且栈布局进入墙 3 的补丁射程。
（`git` 取 VCS 信息同样被拒 → 编译加 `-buildvcs=false`。）

### 附带解决：Go 的 DNS
Go 纯 resolver 读 `/etc/resolv.conf`，本机 `/etc` → `/system/etc`（只读且**没有** resolv.conf）
→ 退到 `[::1]:53` 全挂。而 Go 源码里 `src/net/dnsclient_android.go` 硬编码了 **Termux 的路径**
`/data/data/com.termux/files/usr/etc/resolv.conf` → 改成我们的 `$PREFIX/etc/resolv.conf`（内含 8.8.8.8）即可。
验证：`copilot.tencent.com -> 43.159.104.94`，`:443` 握手成功。

模块依赖同理：Go 拉不到 proxy.golang.org → 用 `curl`（走 netd，能解析）把 5 个 module 拉下来，
在 `$PREFIX/lib/workbuddy2api/goproxy` 搭 **file:// 本地代理** 喂给 Go。

## 三、落地文件

```
$PREFIX/lib/go/                     ← Termux golang 1.27.1（已打 rt0 补丁 + resolv 补丁）
$PREFIX/lib/workbuddy2api/
    server signin login credit trial  ← 编译产物（ET_DYN，可被 linker 加载）
    goproxy/                          ← 本地 Go 模块代理（离线可用）
$PREFIX/bin/go                      ← wrapper（自动 export GOROOT 后 exec 真身）
$PREFIX/bin/wb2api                  ← → server
$PREFIX/bin/wb-login / wb-credit / signin_bin
$PREFIX/etc/resolv.conf             ← Go 现在读这里

工作区 /storage/emulated/0/新建文件夹/workbuddy2api/
    workbuddy2api-panel/            ← 上游仓库（含 config.json / auths/ / data/）
    run.sh                          ← 启停/状态/日志
    rebuild.sh                      ← 重新编译
    WorkBuddy2API.apk               ← 安卓安装包（3.9 MB，可直接安装，见第六节）
    apk-src/                        ← App 源码（Manifest/res/java/assets + 签名密钥）
    patches/patch_rt0.py            ← 二进制 rt0 补丁（幂等）
    patches/patch_source.sh         ← 源码级移植补丁（幂等/可 --revert）
    patches/build_apk.sh            ← 一键重新打包 APK
    PORTING.md                      ← 本文
```

## 四、用法

```bash
cd /storage/emulated/0/新建文件夹/workbuddy2api
bash run.sh start      # 启动（nohup，pid 落 data/server.pid）
bash run.sh status     # 状态 + /healthz
bash run.sh log 60     # 看日志
bash run.sh stop
bash run.sh panel      # 打印面板地址与 api_key
bash rebuild.sh        # 改完源码重编
```

面板：<http://127.0.0.1:7863/panel/>　API：`http://127.0.0.1:7863/v1`（`Authorization: Bearer <api_key>`）

### 绑定自己的 CodeBuddy 账号（需要你操作）

```bash
cd /storage/emulated/0/新建文件夹/workbuddy2api/workbuddy2api-panel
bash login.sh          # 打出授权 URL → 浏览器登录 → 回车轮询 → 落盘 auths/workbuddy-<uid>.json
bash ../run.sh restart # 本机没有 docker，login.sh 的重启那步会跳过，手动重启加载新账号
```

## 五、注意事项 / 已知限制

* **二进制必须放在 `$PREFIX`（app 私有、可执行）下**，不能放 `/sdcard`（noexec）。所以 `$PREFIX/bin/wb2api` 是符号链接到 `$PREFIX/lib/workbuddy2api/server`。
* **别用 `env -i`**：清掉 `TERMUX_EXEC__*` 后 shim 失效，连 `bash` 都会 `Permission denied`。
* `CGO_ENABLED=0`：本机无法让 Go 去 exec clang/lld（同墙 4），所以 CGO 一律关。
* Go 默认 `GOOS=android`（Termux 版如此），产物同样是 Linux 兼容的静态 PIE。
* 重启设备后：`$PREFIX`、`$HOME/.config/go/env`、工作区都会保留，直接 `bash run.sh start` 即可；
  `$PREFIX/tmp` 会清空（重要东西都已挪到 `lib/` 下）。
* 若以后**替换/升级 Go 工具链**，`lib/go` 里的 rt0 补丁与 resolv 补丁会丢 → 重跑 `patches/patch_source.sh` + `patches/patch_rt0.py`（或 `rebuild.sh`）。
* 上游 README 声明：本项目仅供自用账号（签到/保活/个人工具接入），禁止批量小号分发与售卖 —— 请遵守。

---

# 六、打包成安卓 App（APK）

产物：**`WorkBuddy2API.apk`（3.9 MB，包名 `com.dsh.wb2api`，v1.17.0 / versionCode 5，
minSdk 24 / targetSdk 34，已用 v2+v3 签名，内置看门狗见第七节、手机端适配见第八节、
状态判定与前台服务见第九节）**

## 它是怎么做的

本机没有 Android Studio，也没有 Gradle —— 全部工具在 Termux 侧凑齐后手工打包：

| 环节 | 用的东西 |
|---|---|
| 资源编译/链接 | `aapt2`（Termux 包）+ `android.jar`（从 Google 官方 platform-35 里取） |
| Java 编译 | `javac` 17（Termux openjdk-17） |
| dex | `d8`（Google Maven 的 `r8.jar`，Java 17 上跑） |
| 组装 | `zip` 手工塞 `classes.dex` + `lib/arm64-v8a/*.so` |
| 签名 | `apksigner` 0.9 + `keytool` 自签（v1/v2/v3 全开） |

## App 内部结构

```
WorkBuddy2API.apk
├── classes.dex                         ← MainActivity + GatewayService
├── assets/config.json                  ← 首次启动改写 api_key 后落到 filesDir/gateway/
├── res/ + resources.arsc               ← 图标、应用名
└── lib/arm64-v8a/libwb2api.so          ← 整个 Go 网关（10.5 MB，压缩后 ~3.5 MB）
```

**为什么网关要伪装成 `.so`**：Android 里 app 唯一被允许执行的位置就是
`nativeLibraryDir`（安装器把 `lib/<abi>/*.so` 解到这里）。把它放进去，
`extractNativeLibs="true"` 一开，安装器就会解出来。

启动逻辑是**双路**的（写死在 `GatewayService`）：
1. `libwb2api.so` **有 x 位** → 直接 exec（内核铺标准栈，argv 完全正确，用的是**未打 rt0 补丁**的 runtime）；
2. **没有 x 位** → 退回 `ProcessBuilder("/system/bin/linker64", bin, bin)` 交给 bionic 代跑
   （这条就是 DSH 侧一直在用的路；此处 Go 侧 flags 会被忽略，但默认读 cwd 的 `config.json`，行为一致）。

## App 做了什么

* **前台服务**跑网关：工作目录 `filesDir/gateway`（`config.json` / `auths/` / `data/` 都在这里），
  DNS 从 `ConnectivityManager` 取系统 DNS 写成 `gateway/resolv.conf`，再用
  `WB2API_RESOLV_CONF` 环境变量告诉 Go（纯 resolver 不读系统属性）。
* **WebView 面板**：进来直接加载 `http://127.0.0.1:7863/panel/`，
  并自动把 api_key 注入 `localStorage['wb2api.key']` —— 免去手输面板密钥。
* 顶栏三个按钮：**重启网关 / 复制 API 信息 / 重载面板**；状态条实时显示进程与最后一行日志。
* 常驻通知（含"停止"按钮），App 退到后台网关照跑（签到/保活等定时任务才活得下去）。
* **添加账号**用面板自带的授权流程（`POST /panel/api/login/start` + 轮询），不需要另做界面。

## 安装与使用

1. 手机文件管理器里点开 `WorkBuddy2API.apk` → 允许"安装未知应用" → 安装。
2. 打开 App，等 2~3 秒面板自动出来（首次会在 `filesDir/gateway/` 生成 config.json，含随机 api_key）。
3. 面板里**添加账号**（扫码/授权）→ 绑好后 `/healthz` 变 healthy。
4. 给别的客户端用：App 顶栏「复制 API 信息」拿到
   `Base URL: http://127.0.0.1:7863/v1` + `API Key`；同局域网换成手机 IP（如 `http://192.168.10.213:7863/v1`）。
5. 权限：只用到网络与通知（Android 13+ 会弹通知权限，拒了也能跑，只是没常驻通知）。

## 重新打包（改源码后）

```bash
bash /storage/emulated/0/新建文件夹/workbuddy2api/patches/build_apk.sh
```
* Android 侧源码在 `apk-src/`（`AndroidManifest.xml` / `res/` / `java/` / `assets/` / `test/`）。
* 签名密钥 `apk-src/wb2api.keystore`（口令 `wb2api2026`）—— **升级必须用同一个密钥**，别弄丢。
* 构建工具已固化在 `$PREFIX/lib/wbapi-tools/`（`android.jar` + `r8.jar` + `apksigner.jar`）。
* **升级**：同一密钥 + versionCode 递增（当前 2），直接点新 APK 覆盖安装即可；
  `filesDir/gateway/` 里的 `config.json`（api_key）与 `auths/`（账号）都会保留，不用重新绑。

## 已验证 / 未验证（诚实说明）

已验证：APK 结构与清单（`aapt2 dump badging/xmltree`）、dex 内含两个类、包内 Go 二进制
（ET_DYN + PT_INTERP + PT_DYNAMIC + 未打 rt0 补丁 + 带 `WB2API_RESOLV_CONF` 补丁）、
v2+v3 签名校验通过、构建脚本可复现（5 秒重打出同尺寸包）。

未验证：**在真机上跑起来的那一下** —— 安装必须你亲手点（本机没有 Shizuku/ADB/无障碍通道，
我没法代你点"安装"）。两条启动路径都写了兜底，装完要是异常，把 App 状态条最后一行日志发我。

---

# 七、看门狗（自愈）

## App 内：四层（v1.15.0 起）

| 层 | 机制 | 参数 |
|---|---|---|
| 进程级 | 每 10s 看孩子还在不在，不在就拉起 | 退避 2/4/8/16/32/60s 封顶，稳定 120s 后计数清零 |
| 健康级 | 进程在但 `/healthz` 没响应（僵死）→ 杀掉重拉 | 启动宽限 45s |
| 系统级 | `AlarmManager`（`setAndAllowWhileIdle`）每 5 分钟兜一次 | App 被系统整个杀掉也能自己回来 |
| 开机 | `BOOT_COMPLETED` / `MY_PACKAGE_REPLACED` → `BootReceiver` | 开机与升级后自启 |
| **保活级** | 服务在跑就持 `PARTIAL_WAKE_LOCK`（tag `wb2api:gateway`） | **不持它后台就是死的**，见下 |

### 为什么"只有 App 在前台时才生效"（是电源管理，不是看门狗写错）

| 现象 | 机制 |
|---|---|
| 屏幕一灭，10s 的看门狗 tick 停摆 | 看门狗是**用户态定时器**。SoC 进 suspend → CPU 不跑 → `Thread.sleep` 和 Go 里的 timer 都不推进 |
| 那靠 AlarmManager 唤醒？ | 能唤醒，但 Doze 下 `setAndAllowWhileIdle` 被夹到 **≥9 分钟**一次；且 Doze 期间**网络被掐**（签到/保活要联网，直接被断） |
| 退到后台就被冻 | Android 11+ **cached-app freezer**：整个 app 进程组被冻结 —— Go 网关是 app 的**子进程**、同属一个 cgroup，一起冻 |
| 为什么前台就正常 | 屏幕亮 = CPU 在跑，纯粹是"没被冻住"的假象 |

**修法（v1.15.0 已做前两条，第三条得你手动）：**
1. **partial wakelock**（代码）：服务在跑就一直持着 → CPU 不睡 → tick 与 Go 定时器照常走。
   代价是耗电（CPU 不睡）；不想常驻就在通知栏点「停止」，会一并释放。
2. **省电白名单**（要你点一下）：顶栏新按钮「省电白名单」→ 系统弹窗允许 →
   Doze 不再掐网络、不推后 alarm。状态条会显示 `省电白名单 / 省电未白名单`。
3. **国产 ROM 还得手动**：自启动 + 后台无限制 + 锁定后台（MIUI：自启动 / 省电策略无限制 / 锁后台；
   ColorOS、EMUI、HyperOS 等同理，各家叫法不同）。这层代码做不到，只能你在系统设置里放行。

**绝不看门的地方**：7863 已被外部实例占用时，只"复用不干预"，不抢不杀 —— 否则两边互杀。
状态条显示「看门狗自愈 N 次」，通知栏同步。
纯逻辑自检 `apk-src/test/WatchdogSelfCheck.java`（退避序列）由 `build_apk.sh` 自动跑，不需要真机。

## 命令行侧：`watchdog.sh`

```bash
nohup bash watchdog.sh 20 >/dev/null 2>&1 &      # 启动（间隔 20s）
kill "$(cat workbuddy2api-panel/data/watchdog.pid)"   # 停止
tail -f workbuddy2api-panel/data/watchdog.log    # 日志
```
* 进程没了 **且** 服务不通 → 立刻拉起（带退避）
* 进程在但 `/healthz` 连续 3 次不通 → 判僵死，`run.sh stop` + `start`
* 7863 上有外部实例在服务（200/503）→ 复用不干预，只提示一次

## 实测记录（都在本机真跑过，不是纸上推演）

| 场景 | 结果 |
|---|---|
| 看门狗遇上外部实例（7863 被别处占着） | 只提示一次；`server.log` 124 行 → 124 行，**没去抢**；外部实例全程 healthy ✓ |
| `kill -9` 自己那份（7999 沙盒，避免碰你的 7863） | 6s 内拉起新 pid，`/healthz` 恢复 ✓ |
| 退避序列 | 自检通过：2/4/8/16/32/60s 封顶，n≤0 退化 2s ✓ |
| 你正在用的 7863 实例 | 全程 `healthy:1, total:1`，一次未受影响 ✓ |

## 两个踩出来的坑（已写进脚本）

1. **`pgrep -f 'workbuddy2api/server'` 会误匹配**：连"命令行里恰好提到这个路径"的无关进程一起命中
   （实测：跑 `run.sh` 的 shell 自己就在 cmdline 里带着它，松散写法命中 3 个进程）。
   实测真实 cmdline 是 `…/workbuddy2api/server …/workbuddy2api/server -config config.json`（shim 保留 argv[0] 为绝对路径），
   所以改用锚定写法 `^[^ ]*/workbuddy2api/server( |$)` —— 只命中真身。
   `run.sh` 的 `running_pid` 也一并修了（pid 文件优先 + 锚定兜底）。
2. **`curl … -w '%{http_code}' || echo 000` 会拼出 `000000`**（curl 失败本来就输出 000）→ 改 `code=${code:-000}`。

## 关于"端口被别人占着"

`run.sh status` 现在会说清是哪种情况：

```
本脚本未起服务，但 7863 上有实例在服务（http=200）→ 外部实例，别抢
```
跨 uid 的进程在 `/proc` 里看不到（SELinux），所以**判断依据是端口本身**（能不能服务），不是"跑没跑我的进程"。

---

# 八、手机端适配（面板网页 + App 外壳）

## 8.1 面板网页：只补窄屏实测会坏的地方，一行 JS 没碰

上游作者其实适配过（`<meta viewport>` + 五组 media query：`.shell` 在 ≤760px 塌成单列、
KPI/统计卡自动降列、宽表装在 `.tbl-wrap{overflow-x:auto}` 里横滚）—— 所以**不是重写**。

**实测依据**（360px 视口连测试实例 7999，读 DOM 快照）：

| | 结果 |
|---|---|
| 补丁前 | `模型与档位 / 配置 / 运行日志` 三个导航项标 `[off-screen]`（要横滑才够得着），`.qrow`/`.srow` 是**不在横滚容器里的固定列宽网格** → 整页被撑出横向滚动条 |
| 补丁后 | **7 个 tab 全部可见，快照里没有任何 off-screen 节点** ✓（切视图、日志、主题都正常） |

| 补的六处 | 怎么补 |
|---|---|
| ① 导航 7 项露不全 | `.nav` sticky + `flex-wrap`；`ul` 独占一行、`li` 均分 → 7 项全见 |
| ② 往下滚找不到入口 | 同上 sticky；`.topbar` 让位改 `position: static`（否则两个 sticky 抢顶部） |
| ③ 触控目标太小 | `button` ≥40px、`button.xs`/`select.xs` ≥32px |
| ④ 固定列宽网格撑破页面 | `minmax(0,1fr) auto` 可收缩两列；`.stasks` 降 2 列 |
| ⑤ 白扔 13% 宽度 | `.view` 边距 24→12px；`.acc` min-width 640→560、单元格 11→9px |
| ⑥ 弹层在手机上偏窄 | `.veil.on` 底对齐 + `.dlg` 满宽上圆角 = 底部抽屉 |

**怎么改 / 怎么复现**：

```bash
vim patches/panel-mobile.css          # 改 CSS（别手改 index.html 里那块）
bash patches/apply_panel_mobile.sh    # 幂等注入：有标记就替换，没有就插到 </style> 前
bash rebuild.sh                       # 面板是 go:embed 进二进制的，必须重编才生效
```
补丁在 `index.html` 里由 `DSH-MOBILE-PATCH-BEGIN/END` 夹住，`git checkout internal/panel/index.html`
即可回到上游原样。**生效范围只有 ≤760px**，桌面端一行没动（全在 media query 内）。

## 8.2 App 外壳（v1.16.0 重做）

旧版是"一个灰状态条 + 4 个系统默认 Button"，压在冷石墨面板上像两个东西拼的。现在：

* **配色直接抄面板 `:root` 的暗色令牌**（bg `#0C0E14` / surface `#14171F` / line `#262C3A` /
  accent `#5B7CFA` / ok `#3DDC97` …），状态栏与导航栏染 surface，WebView 底色 = bg（不再白闪）。
* 状态条三行：`● WorkBuddy2API` + 右侧状态词（运行中/启动中/已停止，按状态染色）→
  `保活中 · 省电白名单 · 自愈 N · :7863` → 最后一行日志（单行省略）。
* 按钮改 4 个**胶囊 chip**（圆角 7px = 面板 button 同款、surface-2 底、按下描边 accent）。
* 2dp **加载进度线**（WebChromeClient 回调，加载完自动收起）。
* WebView 设置修正：**`setLoadWithOverviewMode(false)`** —— 面板自带 viewport meta，
  原来开 overview 会把整页缩小（宽表一多就"字小到看不清"，这正是"没适配"的观感来源）；
  另加 `setTextZoom(100)`、保留双指缩放、`OVER_SCROLL_NEVER`。

两边主题要同步改时：面板改 `internal/panel/index.html` 的 `:root`，App 改 `MainActivity` 顶部的常量块。

---

# 九、状态判定与前台服务（v1.17.0）

## 9.1 为什么"不论是否运行都显示已停止"（我的设计错误，已修）

v1.16.0 把状态词从**中文散文串**里猜出来：

```java
if (st.contains("失败") || st.contains("未启动")) return "已停止";   // ← 就是这一行
```

两个致命点一起发作：

| 问题 | 后果 |
|---|---|
| 真实运行态串是 `直接 exec pid=1234` / `linker 代跑 pid=1234` | 不含任何关键词 → 该显示"运行中"却显示"未知" |
| `state` 初值是 `"未启动"`，而 `startForegroundCompat()` 失败时**异常被吞**、直接 `stopSelf()` | `state` 永远停在"未启动" → **永远显示"已停止"** |

**修法**：

1. 服务导出**机器可读**状态：`statusOf()`（`ST_STOPPED/STARTING/RUNNING/EXTERNAL`）+ `reasonText()`；
   状态/文案/原因由 `setState(status, text, why)` 一次改齐，杜绝"改了串忘改状态"。
2. UI **不再解析中文**，判定按事实 —— 后台线程探 `127.0.0.1:7863`（150ms 连接，**永不占主线程**）：

   | 事实 | 显示 |
   |---|---|
   | 端口在服务 | **运行中**（服务自称外部实例 → 「运行中（外部）」） |
   | 服务自称在跑但端口不通 | 启动中 |
   | 服务没起来 | 已停止 + 第三行给出**原因** |
   | 首次探测还没回来 | 启动中（不闪一下"已停止"） |

3. 异常不再吞：`kick()` / `startForegroundCompat()` 的失败原因写进 `reason`，直接显示在状态条第三行。
4. 删掉 `buildNotification()` 里的 `isUp()` 网络探针 —— 它跑在 `onStartCommand`（主线程），
   最长拖 2s，而系统要求 5s 内进前台，等于自己给自己上 ANR。

## 9.2 前台服务类型：dataSync → specialUse（本机是 Android 16 / SDK 36）

`dataSync` 型前台服务在 Android 15+ 有 **6 小时/天**上限，对一个要 7×24 常驻的网关是致命的
（到点被系统掐掉，只在 `onTimeout()` 里说一声）。本机实测 `getprop ro.build.version.sdk = 36`，所以：

* manifest 改 `android:foregroundServiceType="specialUse"` + `PROPERTY_SPECIAL_USE_FGS_SUBTYPE` 说明
  （自用侧载不需要商店审核），权限加 `FOREGROUND_SERVICE_SPECIAL_USE`（dataSync 那条留着做回落）。
* 加 `Service.onTimeout(int)` / `onTimeout(int, int)`（API 34/35）→ 被系统回收时**立刻挂 5 分钟 alarm** 兜回。
* `startForegroundCompat()` 在 API 34+ 显式传 `FOREGROUND_SERVICE_TYPE_SPECIAL_USE`；
  个别 ROM 不认 → 自动回落不带类型的老调用；两条都失败才报错（原因进 `reason`，界面可见）。

**验证**：`aapt2 dump xmltree` 里 `foregroundServiceType=0x40000000`（= SPECIAL_USE）、property 在、
`FOREGROUND_SERVICE_SPECIAL_USE` 权限在 ✓。

⚠️ 仍未验证：`specialUse` 在国产 ROM 的省电策略下是否能一直活 —— 这版装完，状态条第三行会直接告诉你
"为什么不在跑"（把那一行发我即可定位）。

---

# 十、图标与"被杀"防治（v1.18.0）

## 10.1 图标

源图 `docs/WorkBuddy.png`（800×800 RGBA，取自 <https://github.com/DGZSbot/ai-icon>）。
本机 Termux **没有 PIL**（Pillow 要现场编译，装不上）也没有 ImageMagick，所以自写纯标准库缩图器：

```bash
python3 patches/make_icons.py docs/WorkBuddy.png apk-src/res
#   → mipmap-{mdpi,hdpi,xhdpi,xxhdpi,xxxhdpi}/ic_launcher.png（48/72/96/144/192）
```

实现要点：自己解 PNG（非交错、位深 8/16、色彩类型 0/2/3/4/6，含 5 种扫描线过滤器还原）→
**面积平均**缩放（每个目标像素按覆盖面积加权），且全程在**预乘 alpha** 空间做平均 ——
图省事用最近邻、或直接平均 RGBA，都会在这张图的圆角/半透明边缘出灰边。

## 10.2 差点白干半天的坑：aapt2 编译了"错目录"

`build_apk.sh` 原来是 `aapt2 compile --dir res -o res.zip`（**相对路径**），而脚本前面先
`cd "$PREFIX/build/apk"` → aapt2 编译的是 `$PREFIX/build/apk/res`，一份 15:52 留下的老副本
（只有旧图标 + `strings.xml`）。

症状极具欺骗性：**换了图标没变化、新增 res 目录完全不生效，而且一声不响** —— aapt2 正常退出，
`set -e` 也救不了"成功编译了错误的目录"。更迷惑的是那份老 res 一直没变，于是**每次打出来的
APK 字节数一模一样**（3,891,817），一度让我以为"文件根本没重新生成"。

修法三件套：

```bash
rm -rf ... res                                   # 构建前清掉那份陈旧副本
[ -d "$SRC/res" ] || { echo "❌ 找不到资源目录 $SRC/res"; exit 1; }
aapt2 compile --dir "$SRC/res" -o res.zip        # 资源输入一律绝对路径
```

识别特征：APK 里 `res/mipmap-*/` 的条目数与大小跟 `apk-src/res` 对不上。
一行自查：`unzip -l WorkBuddy2API.apk | grep mipmap`（本版应恰好 5 条）。
修好后 APK 从 3,891,817 → 3,969,965 字节（+78KB 就是图标）。

## 10.3 7863 老被杀：先让它能说话，再加一层兜底

被杀是系统行为（内存回收 / ROM 清后台 / 用户划掉任务），没 root 禁不掉；能做的是三件事：
**降低概率、被杀后更快回来、知道是谁杀的**。

* **第五层兜底 `ReviveJobService`**：JobScheduler 周期任务（15 分钟 —— Android 8+ 的最小值），
  `setPersisted(true)` 让它在**重启手机后**依然被系统记着。为什么 alarm 之外还要它：
  两者是**互不相干的调度机制**（Doze 里 alarm 会被拖，job 走系统维护窗口），互为补位；
  且只在"端口不在服务"时才动作（`portUp()` 已存在就不打扰）。
* **`stopWithTask="false"` + `onTaskRemoved()`**：任务列表被划掉 ≠ 停服务（划掉后服务继续跑，
  并重挂 alarm/job）；只有通知里的"停止"才置 `userStop` 并取消 alarm/job。
* **alarm 能精确就精确**：`SCHEDULE_EXACT_ALARM` + `canScheduleExactAlarms()`；
  未授权时自动回落 `setAndAllowWhileIdle`（不会 SecurityException 崩）。
* **诊断（关键）**：以前只能说"又挂了"。现在网关退出码翻成人话写进状态条第三行，并跨 App 重启保留：

  | 退出码 | 含义 |
  |---|---|
  | `rc=137` | 被强杀 SIGKILL —— **内存不足或系统清后台**（最常见） |
  | `rc=143` | 收到 SIGTERM（正常停止流程） |
  | `rc=139` | 段错误 SIGSEGV |
  | `rc=0` | 自己正常退出 |

  同时 App 侧用 `SharedPreferences` 记「App 进程被系统杀过几次」（顶栏显示「系统重启 N」）。
  **下次再被杀，界面那一行就是证据。**

## 10.4 换图标/改资源后的自查清单

```bash
bash patches/build_apk.sh                     # 重打包（8 秒）
unzip -l WorkBuddy2API.apk | grep mipmap      # 应有 5 条
aapt2 dump xmltree --file AndroidManifest.xml WorkBuddy2API.apk | grep -E "ReviveJobService|stopWithTask"
python3 -c "import zipfile;d=zipfile.ZipFile('WorkBuddy2API.apk').read('classes.dex');print(b'ReviveJobService' in d)"
```

---

# 十一、"过一会儿连不上，拉回前台就好"（v1.19.0）

## 11.1 现象与判据

症状：App 启动一会儿后 7863 连不上；把 App 拉到前台立刻恢复。

**"拉回前台就好"这句话本身就是判据** —— 它说明进程**没死**，是被**冻结**了：

| 状态 | 表现 | 能不能自救 |
|---|---|---|
| 被杀（LMK / ROM 清理） | 端口不通，但看门狗线程还活着 → 10s 内自动重拉 | ✅ 不需要人工 |
| **被冻结**（app-freezer / cgroup freezer） | **连看门狗线程和 Go 定时器一起停摆** | ❌ 自己的定时器也冻着，救不了自己 |

Android 12+ 引入了 app-freezer：系统把处于**缓存态**的 App 整组冻结以省电；
国产 ROM 的省电策略更激进。所以"需要人工拉前台"= 缓存态冻结，而不是内存被杀。

## 11.2 为什么"常驻图层"能治它

系统的进程重要性分级看的是**有没有可见窗口**：有可见窗口的进程 oom_adj 更高、不会被判成缓存态，
而**只有缓存态才会进 freezer**。所以挂一个：

```
1×1 像素、alpha=0、TYPE_APPLICATION_OVERLAY、FLAG_NOT_TOUCHABLE|FLAG_NOT_FOCUSABLE|FLAG_NOT_TOUCH_MODAL
```

对用户完全不可见、也绝不抢触摸，但让系统认为"这个 App 在显示东西" → 不冻它。
代价是要用户在系统设置里授予「显示在其他应用上层」（`Settings.canDrawOverlays`）；
没授权就静默跳过，功能照旧（只是可能再被冻）。

## 11.3 另外两件：Wi-Fi 锁 + 冻结自检

* **Wi-Fi 高性能锁**（`WIFI_MODE_FULL_HIGH_PERF`）：屏幕一灭 Wi-Fi 可能进省电/休眠，
  从电脑或别的设备连过来就断（本机 `127.0.0.1` 不受影响）。这是"局域网连不上"的另一半原因。
  创建失败自动回落 `WIFI_MODE_FULL`。
* **冻结自检**：看门狗每次 tick（10s）写一次 `elapsedRealtime`；下次发现间隔 >30s，
  就说明这中间被整体冻过 → `冻过 N` 计数 +1，并把「系统冻了我 N 秒（HH:mm 恢复）」
  写进状态条。没有 root 时，这是唯一能拿到"到底是不是被冻、冻了多久"的证据。

## 11.4 用户侧还要做什么（ROM 权限，App 改不了）

1. App 顶栏点 **「保活设置」**，按提示依次点完：省电白名单 → 显示在其他应用上层 → 闹钟与提醒
2. 国产 ROM 再放行四处：**自启动 / 省电策略「无限制」/ 任务列表加锁 / 允许后台弹窗**
3. 若仍会被冻：把 App 从最近任务里"锁定"（各 ROM 叫法不同），或干脆把屏幕常亮关掉后
   用「屏幕关闭时保持 Wi-Fi 连接」系统选项配合

---

# 十二、任务中心"执行待办没效果"（v1.20.0）

## 12.1 根因：队列路径把错误吞了，还伪装成"没有待办"

`startGrowthQueue`（`执行全部待办` 与每日调度器共用同一个核心）里，拉取任务列表**失败**的账号
被静默跳过：

```go
if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {   // ← err != nil 时什么都不做
```

于是"上游挂了/token 失效"与"确实没事可做"走同一个出口 `全部账号没有待办任务`，而前端
`if (!r.started)` 把它弹成**绿色 ok** —— 用户看到的就是"点了没效果"，且无从得知到底是
token 失效、每日锁（Sequential 族零点解锁）、已领过，还是**本移植版没有该任务码的自动化动作**
（`growthPending` 要求 `autoActionFor(code) != nil`，不满足的直接从列表消失）。

另外 `扫描待办`（`tasksScanAll`）**是**返回每账号 `growth_error` 的，但前端 `groupItems()`
只渲染 `a.growth` —— 错误同样看不见。

## 12.2 改法

| 位置 | 改动 |
|---|---|
| `internal/panel/taskcenter.go` | 新增 `queueDiag{errs,locked,claimed,noauto,gated}` + `explain()`：扫描时顺手记账，无待办时拼成人话；`startGrowthQueue` 多返回一个 `level` |
| 同上（HTTP） | `run_queue` 响应加 `level`：`err` = 真有账号拉取失败，其余 `ok` |
| `internal/panel/app.js` | 按 `level` 选弹窗颜色；`groupItems()` 把 `growth_error` 渲染成**红行 + 原因**；`qrowHTML` 支持 `it.title` 覆盖标题 |
| `taskcenter.go` | `errBrief()` 压平空白 + 截断 140 字 —— 上游报错常是 HTML，换行会把提示撑成多行 |

**为什么必须在服务端判定**：把"没事可做"和"出错了"分开是**语义**问题，前端只能照 `level` 上色；
否则换个前端又退化成一种颜色，"点了没效果"复发。

## 12.3 怎么复现/验证（不需要真账号）

```bash
S=$PREFIX; D=$S/tmp/tc; rm -rf $D; mkdir -p $D/auths
cd $D && timeout 3 /system/bin/linker64 $S/lib/workbuddy2api/server -config config.json   # 生成推荐配置
sed -i 's/:7863/:7999/' config.json
cat > auths/workbuddy-fake.json <<'J'
{"auth":{"accessToken":"bogus-token","refreshToken":"r","expiresAt":1900000000,"domain":""},
 "account":{"uid":"fake-uid-0001","nickname":"测试账号（坏token）"}}
J
/system/bin/linker64 $S/lib/workbuddy2api/server -config config.json &
KEY=$(python3 -c "import json;print(json.load(open('config.json'))['api_key'])")
curl -sS -XPOST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{}' \
     localhost:7999/panel/api/tasks/run_queue
# 期望：{"started":false,"level":"err",
#       "message":"没有可执行待办 —— 1 个账号拉取任务失败：测试账号（坏token）：upstream client (http 401)…"}
curl -sS -XPOST -H "Authorization: Bearer $KEY" localhost:7999/panel/api/tasks/scan_all
# 期望：accounts[0].growth_error 带同样的 401（前端会渲染成红行）
```

⚠️ **端口一律别用 7863**：那是用户自己跑着的实例，任何测试另起端口（本仓统一用 7999）。

## 12.4 面板 JS 怎么自检（本机 node 不能直接 exec）

`go test ./internal/panel/` 里那批 `TestAppJS*` 在本机全挂，原因是 `fork/exec .../node:
permission denied`（SELinux 不让 DSH uid 直接 exec node），**不是 JS 语法问题**。自检走 linker 代跑：

```bash
/system/bin/linker64 $PREFIX/bin/node --check internal/panel/app.js   # 语法 ✓
```

---

# 十三、App 详细日志（v1.21.0）

## 13.1 之前为什么"没日志"

日志其实**一直在写**：`GatewayService.pump()` 把网关进程的 stdout 追加到
`files/gateway/data/server.log`（`redirectErrorStream(true)` → stderr 也在里面）。
问题是没有入口：手机上看不到、也复制不出来，等于没有。

## 13.2 两个日志文件（分工不同）

| 文件 | 谁写 | 内容 | 轮转 |
|---|---|---|---|
| `files/logs/app.log` | `GatewayService.appLog()` | **只有 App 知道的事**：服务启动 startId、App 进程被杀次数、前台服务被拒启动的原因、拉起网关的 mode/pid、网关退出码、**冻结检测**（冻了 N 秒）、看门狗自愈、常驻图层失败、onTaskRemoved、FGS 超时回收、JobScheduler 触发、保活三件套状态 | 512KB → `app.log.1` |
| `files/gateway/data/server.log` | 网关进程自己（stdout+stderr） | 任务中心每个动作、上游 HTTP 状态、chat 流水行、账号 token 失效、队列启动/结束 | 每次拉起前若 >4MB → `server.log.1` |

**为什么必须分两个**：网关日志能说明"任务做了什么、上游回了什么"，但"系统什么时候冻了我/杀了我、
前台服务有没有被拒"只有 App 进程知道 —— 分开写才不会互相冲掉。

## 13.3 日志页（v1.21.0 有、**v1.22.1 已按用户要求拆掉**）

* v1.21.0 加了顶栏「日志」芯片：一页看全 ① App 事件 + ② 网关日志尾部，带「复制全部」+ 自动脱敏。
* **v1.22.1 删除**：动作条回到 4 个胶囊，`openLog` / `copyText` / `tailOf` / `mask` 与
  `HorizontalScrollView` 容器一并移除。**文件照旧写**（不占界面）：
  `files/logs/app.log`（512KB 翻代）、`files/gateway/data/server.log`（4MB 翻代）。
  要看得用 adb：`adb shell run-as com.dsh.wb2api cat files/logs/app.log`。

## 13.4 "执行过了但还是未完成"该在日志里找什么

1. **队列视图那一行**（不用挖日志）：`✅ 进度 +N` / `⚠️ 进度未变化…` / `✓ 已领取` / `还差 N 次` ✓
2. `panel: 队列 growth uid=… code=…: …`（v1.21.0 起带进度判定）
3. `autotask …: accept 尝试N …` / `… 未登记生效（回读=…）` → **上游没把任务标成已接受**
   （未接受的任务不计数，这是"跑了没效果"最常见的原因）
4. `panel: 任务动作 uid=… code=… progress A -> B claimable=… claimed=…` → 进度有没有真的动
5. chat 流水行（`internal/server/logging.go`）：那次"行为"请求上游到底回了什么
6. 若队列行是 `skipped 该账号有其它任务动作在执行` → 同一账号在跑别的（串行锁）

## 13.5 自查（不用装 App 也能看）

```bash
adb shell run-as com.dsh.wb2api cat files/gateway/data/server.log | tail -80
# App 事件日志（冻/杀/拒启动/兜底）：files/logs/app.log
```

---

# 十四、任务中心：只做能做得了的（v1.22.0）

## 14.1 症状与证据

用户原话："任务中心执行待办无效果 —— 做不了就直接砍掉，或者留下能做的。"

代码里对得上的证据有两条：

1. `autoAction.Attempt`（注释："上游未证实可脚本化，跑了可能不点亮"）**只被序列化给前端**
   （`autotask.go` 里那唯一的 `"attempt": act.Attempt`），**从不参与待办筛选** —— 于是
   `black_cat` 这类动作照旧进队列、照旧执行、照旧不点亮。
2. `Sequential_Tasks_4~7` 的注释自己写着"**预留**""判据待解锁验证"，但它们**没被标 Attempt** ——
   即被测者眼中的"做不了的任务"，天天出现在待办里。

## 14.2 设计（三种状态，全部可见）

| 状态 | 判据 | 行为 |
|---|---|---|
| **能做** | 有动作 + `!Attempt`（或开关打开）+ 不在跳过名单 | 进待办，执行 |
| **尝试型** | `Attempt == true` 且开关关闭（**默认**） | 不进待办；扫描结果里列出原因；单任务路径 409 |
| **已跳过** | 同 (uid,code) 连续 `skipFailLimit=3` 次无进展/报错 | 不进待办（落盘，`skipCooldown=7d` 后自动放回，或一键清空） |

"无进展"的判据来自 `runGrowthQueued` 的返回值 `progressed`：`after.Claimed` / 领到奖 /
`after.Current > before.Current` 三者之一为真才算有进展 —— 这正是"执行过了但还是未完成"的机器判据。

## 14.3 数据结构与接口

* `data/task_skip.json`（路径 = `stateSibling(cfg.StateFile, "task_skip.json")`）
  ```json
  { "allow_attempt": false,
    "entries": { "<uid>|<task_code>": { "fails": 3, "why": "进度未变化（上游未计入这次行为）",
                                        "last": "…", "until": "…" } } }
  ```
* `GET /panel/api/tasks/skip` → `{allow_attempt, limit, entries:[{uid,code,fails,why,last,until,blocked}]}`
* `POST /panel/api/tasks/skip` → `{allow_attempt?:bool, clear?:bool, uid?, code?}`
* 扫描项新增 `skipped:[{code,title,reason}]`（前端渲染成"跳过"行，**不静默过滤**）
* 队列诊断账新增 `skipped` 计数（`explain()` 里出声）

## 14.4 怎么验证（本机实测命令）

```bash
# 单测（跳过门控/冷却/清空/开关/坏文件容错）
go test -c -o /tmp/panel.test ./internal/panel/
/system/bin/linker64 /tmp/panel.test -test.run 'TestSkip|TestAttempt' -test.v   # 5/5 PASS

# 端到端（临时实例，绝不动用户运行中的 7863）
cp apk-src/assets/config.json /tmp/tc/config.json   # listen→:7999，api_key→sk-scratch-e2e
$PREFIX/lib/workbuddy2api/server -config /tmp/tc/config.json &
K=sk-scratch-e2e
curl -sH "Authorization: Bearer $K"  http://127.0.0.1:7999/panel/api/tasks/skip
curl -s -X POST -H "Authorization: Bearer $K" -d '{"allow_attempt":true}' \
     http://127.0.0.1:7999/panel/api/tasks/skip      # → 落盘 data/task_skip.json
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7999/panel/api/tasks/skip   # 无鉴权 → 401
```

## 14.5 没验证的部分（如实说明）

* **每个尝试型任务"到底能不能点亮"**：没有真实账号、也不能拿用户账号做实验，所以只能按
  上游注释与既有实测记录标（"三账号点亮"的保持非 Attempt）。若某个被标 Attempt 的其实能点亮，
  勾上「包含尝试型任务」即可，不影响它进队列。
* **`skipped` 行在真实任务列表下的渲染**：本机临时实例没有真实任务，只验证到接口字段与
  前端渲染代码路径（`groupItems` 的 `a.skipped` 分支）；0 账号/坏 token 路径已实测。








