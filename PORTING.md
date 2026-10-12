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



