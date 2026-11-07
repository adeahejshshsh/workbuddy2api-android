# AGENT.md · 踩过的坑与本机环境要点

给接手这个仓库的 AI / 维护者看。**这里记录的都是本机（Android / Termux 壳）特有的问题**，
通用工程约定不放这儿（那些在 `CHANGELOG.md`）。

环境：Android，Termux 前缀 `$PREFIX=/data/data/com.dsharnessmobile.shell/files/usr`，
工作区在 `/storage/emulated/0/...`（**noexec**，只能放源码）。

---

## 一、Go 工具链

### 1. 二进制不能直接 exec（私有目录 + SELinux）

`/data/data/.../usr/bin/*` 不能直接 `execve`。所有 Go 产物（含 `go` 自己、`compile`/`asm`、
测试二进制）都要经 `/system/bin/linker64` 代跑：

```bash
export GOFLAGS="-toolexec=/system/bin/linker64"
go build ./...
```

`-toolexec` 必须留着。写 `GOFLAGS="-buildvcs=false"` 会把 `-toolexec` 顶掉，
立刻报 `fork/exec .../compile: permission denied`。

### 2. linker 代跑会把 argv 顶掉一格

linker64 的栈布局与直接 exec 不同：Go runtime 读到的 `argc` 多 1、`argv[0]` 是发起方路径。
表现是**程序能跑但所有参数错位**（`-config` 被当普通参数，启动即退，无任何提示）。

修法是给 `runtime/asm_arm64.s` 的 `_rt0_arm64` 打补丁（`argc-1` / `argv+16`），
由 `patches/patch_source.sh` 负责，幂等，原文件备份为 `.orig-dsh`。

**两个方向必须区分**：

| 场景 | 用哪份 asm |
|---|---|
| 命令行二进制（经 linker64 跑） | **打补丁**的 |
| APK 内的 `libwb2api.so`（nativeLibraryDir 里是真 exec） | **不打补丁**的（`.orig-dsh`） |

`patches/build_apk.sh` 会临时把 GOROOT 换成 unpatched、构建完再还原（还原做成了 `trap`，
中途 Ctrl-C 也不会把 GOROOT 留在错误状态）。

### 3. 工具链被重置后怎么修

环境会莫名重置（`$GOROOT`、`$PREFIX/bin/*`、`lib/jvm`、`aapt2`、`d8` 全部消失）。修法：

```bash
# Go：解包 deb（不要 apt-get，本机 dpkg 会报 'sh' not found in PATH）
dpkg-deb -x golang_3:1.27.2_aarch64.deb /tmp/g
cp -a /tmp/g/data/data/com.termux/files/usr/* $PREFIX/
python3 patches/patch_rt0.py        # 给 go/gofmt/全部 pkg/tool/android_arm64/* 打补丁
bash patches/patch_source.sh        # 打源码补丁
```

Java 侧同理用 `dpkg-deb -x` 解 openjdk-17，并手动补 `libandroid-shmem` 与
`$PREFIX/bin/{java,javac,keytool,jar,javap,jarsigner}` 软链。

#### 3.1 实测可用的恢复路径（2026-11-07 重走一遍，含几个坑）

```bash
# ① 先让 apt 把 deb 下到缓存里（它会因 dpkg 报 'sh' not found 而失败，但包已经下完）
apt-get update
apt-get install -y --no-install-recommends openjdk-21 d8 apksigner aapt2
ls $PREFIX/var/cache/apt/archives/*.deb     # ← 这些就是全部依赖，直接解包，绕开 dpkg

# ② 逐个解包进 $PREFIX（不装 dpkg 数据库，也不需要 root）
cd $PREFIX/tmp && mkdir -p x
for d in $PREFIX/var/cache/apt/archives/{openjdk-21,d8,apksigner,aapt2,abseil-cpp,fmt,libpng,libprotobuf,libzopfli,libandroid-shmem,libjpeg-turbo,littlecms}_*.deb; do
  [ -f "$d" ] && dpkg-deb -x "$d" x
done
cp -a x/data/data/com.termux/files/usr/* $PREFIX/

# ③ JDK 不装 dpkg 就没有 postinst 建的软链，手动补（少了它 java 不在 PATH 里）
J=$PREFIX/lib/jvm/java-21-openjdk/bin
for b in java javac keytool jar javap jarsigner; do ln -sf "$J/$b" "$PREFIX/bin/$b"; done
java -version    # 报 dlopen libandroid-shmem.so not found → 就是 ② 少解了那个包
```

**android.jar 要 API 36，不是 34**：`GatewayService` 里 `@Override public void onTimeout(int, int)`
是 API 36 的 `Service.onTimeout(int,int)`（API 34 的 stub 里只有 `onTimeout(int)`，javac 直接
报 `method does not override`）。清单里的 `targetSdkVersion 34` 与它无关。

```bash
curl -sSL -o plat36.zip https://dl.google.com/android/repository/platform-36_r02.zip   # 63 MB，2 秒
unzip -o -j -q plat36.zip android-36/android.jar -d $PREFIX/lib/wbapi-tools/          # build_apk.sh 认这个路径
```

自检四件套（都过了才谈出包）：

```bash
java -version && javac -version && aapt2 version
java -cp $PREFIX/share/java/d8.jar com.android.tools.r8.D8 --version
java -jar $PREFIX/share/java/apksigner.jar version
```

> ⚠ **绝不要在 node/JVM 正在运行时原地覆盖 `$PREFIX/lib/libc++_shared.so` 之类的共享库**。
> 已经 mmap 的库被替换会让进程立刻 `SIGBUS (BUS_ADRERR)` 崩掉（不是内存不足 ——
> 当时 MemAvailable 还有 4.5GB）。升级共享库要么换路径，要么先停进程。

#### 3.2 Go 模块下载（DNS 坏掉时的绕法）

Go 自己的 resolver 读不到可用的 resolv.conf（回落到 `[::1]:53` → connection refused），
`GODEBUG=netdns=cgo` 也救不了；而 curl 走系统解析是好的。所以：用 curl 把模块灌进缓存，
再 `GOPROXY=off` 离线构建。

```bash
C=$($GOROOT/bin/go env GOMODCACHE)/cache/download
for mv in "github.com/andybalholm/brotli v1.2.7" "github.com/redis/go-redis/v9 v9.18.0" \
          "github.com/refraction-networking/utls v1.8.2" "golang.org/x/crypto v0.36.0" \
          "golang.org/x/sys v0.31.0" "github.com/klauspost/compress v1.17.4" \
          "github.com/cespare/xxhash/v2 v2.3.0" "go.uber.org/atomic v1.11.0" \
          "github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f"; do
  set -- $mv; mkdir -p "$C/$1/@v"
  for e in info mod zip; do curl -fsSL -o "$C/$1/@v/$2.$e" "https://goproxy.cn/$1/@v/$2.$e"; done
done
```

> ⚠ **绝不要在 node/JVM 正在运行时原地覆盖 `$PREFIX/lib/libc++_shared.so` 之类的共享库**。
> 已经 mmap 的库被替换会让进程立刻 `SIGBUS (BUS_ADRERR)` 崩掉（不是内存不足 ——
> 当时 MemAvailable 还有 4.5GB）。升级共享库要么换路径，要么先停进程。

---

## 二、跑测试

沙箱禁止 `go test` 直接 fork 测试二进制。固定套路：

```bash
go test -buildvcs=false -c -o $PREFIX/tmp/pool.test ./internal/pool/
cd internal/pool && PATH=/system/bin /system/bin/linker64 $PREFIX/tmp/pool.test -test.v
```

**cwd 必须是包目录**：测试夹具用相对路径（`app.js`、`testdata/`）。

面板包有 15 个前端测试依赖 `node`。本机 `exec.LookPath("node")` 能找到，但**执行不了**
（私有目录不能直接 exec），所以探针必须真跑一次 `node --version` 再决定 skip ——
只看 LookPath 会让这 15 个测试全红。

---

## 三、打 APK

### 1. aapt2 与仓库的 abseil/protobuf ABI 不匹配

`aapt2` 报 `cannot locate symbol ...ZeroCopyInputStream::ReadCord...` 直接崩。
所以 `build_apk.sh` 探测 `aapt2 version`，失败就走**降级路径**：
复用上一版 APK 的清单与资源，只重编 `classes.dex` + 换 `.so`，并把 AXML 字符串池里的
`versionName` 按 UTF-16LE **等长就地改写**。

后果：**二进制清单的 `versionCode` 改不了**（只能改 versionName），系统里的版本号不会递增。
要真正改清单，得先找到与当前 abseil 兼容的 aapt2。

### 2. AXML 版本串必须按 UTF-16LE 匹配

手写字节模式永远匹配不上。正确做法：

```python
pat = re.compile(rb"(?:(?:[0-9]\x00)+\.\x00(?:[0-9]\x00)+\.\x00(?:[0-9]\x00)+)")
```

替换要求**新旧长度一致**（`1.26.0` → `1.26.3` 都是 6 字符）。长度不同就得走 aapt2 重编。

### 3. d8 / apksigner 的位置不固定

`$PREFIX/lib/wbapi-tools/` 里的私有副本会被环境重置抹掉，脚本已改成双路径探测，
回落到 `$PREFIX/share/java/{d8.jar,apksigner.jar}`。

### 4. apksigner 的 `file:` 密码是**按行顺序读**的

`--ks-pass file:p --key-pass file:p` 指向同一个文件时，第二个参数会读到 EOF，报
`end of file reached`。PKCS12 的 key 密码与 store 密码相同，**省略 `--key-pass`** 即可。

口令文件内容必须**带换行**（`printf '%s\n'`）。

### 5. 签名口令存放

优先级：`WB2API_KS_PASS` 环境变量 → `$PREFIX/etc/wbapi.kspass`（0600，私有目录）→
工作区 `.kspass`（`/sdcard` 上 chmod 无效，仅兜底）。仓库里**不放**口令。

---

## 四、运行期踩过的坑

### 1. `rc=141`（SIGPIPE）—— 网关莫名其妙死掉

Go 对 stdout/stderr 写 EPIPE **直接以 SIGPIPE 退出**。如果宿主（App 的 pump 线程、
shell 的管道）关掉了读端，网关立刻 141 退出，日志上看不出任何原因。

两侧都要防：

* Go 侧：`signal.Notify(SIGPIPE)` 接管，写失败只返回 EPIPE；
* Java 侧（`GatewayService.pump()`）：超限后**只读空、不关读端**，循环只在 EOF 结束。

### 2. 面板"改了没生效"

面板是 `go:embed` 进二进制的。改了 `internal/panel/*.html|js|css` 必须重编服务端；
改 `patches/panel-mobile.css` 还要先跑 `patches/apply_panel_mobile.sh` 把它注入 `index.html`。

### 3. aapt2 编译了"错目录"

`aapt2 compile --dir res` 这种**相对路径**会去编译构建目录里那份陈旧副本，图标/字符串
永远是旧的且一声不响。脚本里一律写绝对路径。

### 4. `patch_source.sh` 的幂等性

第 2 步（resolv.conf 路径替换）的锚点是 `"/etc/resolv.conf"`，而第 3 步追加的 helper
函数里也含这个字符串 —— 重跑时会把 `.orig-dsh` 备份覆盖成"已打补丁的版本"，`--revert`
从此失效。已加守卫：文件里已有 `wb2apiResolvConfPath` 就跳过第 2 步。

### 5. 前台服务类型必须是 `specialUse`

`dataSync` 在 Android 15+ 有 6 小时/天上限，对 7×24 网关是致命的。

### 6. "只有 App 在前台时才生效"

不是看门狗写错，是电源管理：屏幕一灭 SoC 进 suspend，用户态定时器与 Go 定时器全冻住。
必须持 `partial wakelock`；"过一会儿连不上、拉回前台就好"则是 app-freezer，
靠常驻 1×1 全透明图层规避（系统不把带悬浮窗的进程判成缓存态）。

### 7. 状态判定按事实，不按自报字符串

后台线程探 `127.0.0.1:7863` 的真实连通性。早期靠"猜中文状态串"判断，串一改就误报
"已停止"。

---

## 五、本机专属的部署事实

* 网关数据在 `/data/data/com.dsh.wb2api/files/gateway/`（`config.json`、`auths/`、
  `data/state.json`、`data/server.log`）—— 这个 uid 读不到，只能 `adb`/`run-as` 取。
* 面板与 API 都在 `:7863`；默认监听 `127.0.0.1`，对外必须显式改 `listen` + 配 `api_key`。
* 端口上如果已经有一个不是本服务拉起的实例，服务会判成「运行中（外部）」并**不抢端口**。
* **测试节奏**（2026-10-22 起）：
  - 迭代中只跑**改动涉及的包**：`bash patches/test.sh`（自动从 git diff 推包名）；
    也可显式 `bash patches/test.sh server pool`。
  - **发版前**跑一次全量：`bash patches/test.sh all`（≈18s，含编译）。
  - 单包 ≈1s，全量 ≈18s —— 别在迭代里无脑全量。
  - 新增等待/节流类变量时，**必须**在包内测试初始化里把它置 0（`zz_testinit_test.go`）；
    跨包的私有常量用导出的 `...ForTest` 钩子（如 `upstream.SetBillingRetryDelayForTest`）。
* **版本号规矩**（2026-10-22 起）：
  - **纯修 bug 不递增 `VERSION`** —— 直接改代码 + 在 `CHANGELOG` 当前段落追加条目即可。
  - **有实际变更**（新功能 / 策略调整 / 指纹变更）才递增，且**只升最小位**
    （`1.35.0` → `1.35.1`），不发 minor。
  - `VERSION` 与 `apk-src/AndroidManifest.xml` 的 `versionName`/`versionCode` 必须同步
    （`versionCode = major*10000 + minor*100 + patch`）。
* **每次改动都必须提交**（2026-10-23 起，用户明确要求）：改完立刻
  `bash push_github.sh "(fix) 说明"`，不要攒着等下次；**Release 仍然只在需要可安装包时才发**
  （下面两档里的第一档就是默认动作，第二档必须显式加 `release` 参数）。
* 发布分两档（**默认只提交，不发版**）：
  - **日常改 bug / 小改动 → 只推源码**：`bash push_github.sh`
    （提交并推到 main，**不建 Release、不上传 APK**）。
  - **需要可安装包时才发版**：`bash push_github.sh release`（建 Release + 上传 APK）。
    多个修复攒到一起再发，避免 Release 列表被刷爆。
  - token 走 `GITHUB_TOKEN` 环境变量或 `/sdcard/public_repo`；脚本会先扫敏感串，命中就中止。

---

## 六、与上游（misakano7545/workbuddy2api-panel）的能力对照

2026-11-04 做过一次逐项对照：**路由层面已经清零**（上游的每一条 HTTP 路由我们都有；
我们只多一条 `GET /livez`）。剩下的差异是四个**已判定不移植**的子系统——写在这里，
免得下次有人重新推一遍。

| 上游 | 我们的状态 | 为什么不动 |
|---|---|---|
| `internal/server/continue.go`（截断自动续写，551 行） | 未移植 | **上游自己没接线**：`continueReader` 只在它自己的测试里构造，生产路径零引用。把一段上游都不敢启用的流式手术搬进来，风险全归我们 |
| `internal/upstream/timing.go`（分段耗时 prep/do） | 未移植 | 纯观测、不改行为。要动 `prepareBody` 的返回值（热路径）与 `logChatRow` 的签名，而我们的日志行早已与上游分叉（effort/think 字段不同源）。收益是「把 TTFB 拆开 0.5s」，不值得为它在热路径上重新布线 |
| `internal/panel/taskjobs.go` + `tasksweep.go`（持久化作业记录） | **设计不同，不是缺口** | 上游是「作业记录模型」（ID/生命周期/上限/清扫）；我们是「队列 + 同步执行」（`taskcenter.go` 的 queue + `autotask.go`）。两者都提供「异步启动 + 轮询进度」，换实现是重构不是移植 |
| `internal/proxypool/*` + `panel/proxies.go` | 不适用 | 我们走 env 代理 + 自建 CONNECT/SOCKS5 隧道（`upstream/proxytunnel.go`），没有节点池概念 |
| `internal/updater/*` + `panel/update.go` | 不适用 | 产物是本地签名 APK，更新走 GitHub Release，不需要进程内自更新 |
| `internal/probe/*` | 不适用 | `//go:build ignore` 的真机测量工具，不是运行期子系统 |

### 已经收敛的差异（别再按老结论办）

我们曾在一处判断上与上游分叉，后来上游用更好的办法解决了同一个问题，已改用上游做法：

| 曾经的我们 | 上游做法 | 现在的我们 |
|---|---|---|
| 视频台账未命中的任务收养后标 `Foreign`，**永不记账**（怕外来 `task_id` 污染成本账本） | `adopt(…, charged = state.Done())`：已终态视为已记账（防重复计入），还在跑的出片时照常记一次 | 已改上游做法。`Foreign` 反而有洞——重启会丢掉所有在跑视频的记账；而上游的探测只会命中「某个号真能看见」的任务，那笔 credit 确实是这个号花掉的 |
| 台账未命中就**对两域每个可用账号各问一次**，查不到也不留记忆 | `videos_probe.go` 有界探测：负缓存 30s + 游标、单次最多 5 个号、全局 30 次/分钟、撞限流即停并暂停 30s、探测失败不罚号 | 已移植。旧实现让任意有效 key 换着填 id 就能把请求放大 N 倍打满整池 |

**对照方法**（下次复用）：别比文件数量，比**路由**和**是否接线**。

```bash
# 路由对照
git show FETCH_HEAD:internal/server/handler.go | grep -o 'HandleFunc("[A-Z]* [^"]*"' | sort
git show FETCH_HEAD:internal/panel/panel.go     | grep -o 'HandleFunc("[A-Z]* [^"]*"' | sort
# 接线判定：函数有调用点才算「能力」，只在测试里出现的是死代码
git grep -n "SomeThing" FETCH_HEAD -- '*.go' | grep -v _test.go | grep -v "func SomeThing"
```
