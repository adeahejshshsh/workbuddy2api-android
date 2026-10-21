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
* **版本号规矩**（2026-10-22 起）：
  - **纯修 bug 不递增 `VERSION`** —— 直接改代码 + 在 `CHANGELOG` 当前段落追加条目即可。
  - **有实际变更**（新功能 / 策略调整 / 指纹变更）才递增，且**只升最小位**
    （`1.35.0` → `1.35.1`），不发 minor。
  - `VERSION` 与 `apk-src/AndroidManifest.xml` 的 `versionName`/`versionCode` 必须同步
    （`versionCode = major*10000 + minor*100 + patch`）。
* 发布分两档（**默认只提交，不发版**）：
  - **日常改 bug / 小改动 → 只推源码**：`bash push_github.sh`
    （提交并推到 main，**不建 Release、不上传 APK**）。
  - **需要可安装包时才发版**：`bash push_github.sh release`（建 Release + 上传 APK）。
    多个修复攒到一起再发，避免 Release 列表被刷爆。
  - token 走 `GITHUB_TOKEN` 环境变量或 `/sdcard/public_repo`；脚本会先扫敏感串，命中就中止。
