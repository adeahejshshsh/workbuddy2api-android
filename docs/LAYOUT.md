# 仓库布局

```
workbuddy2api-android/
├── apk-src/                     Android 客户端工程（**只有产品代码**）
│   ├── AndroidManifest.xml
│   ├── java/com/workbuddy/api/  22 个 .java：控制台/主题/弹层/HTTP/服务
│   ├── res/                     VectorDrawable 图标、values、mipmap
│   ├── assets/                  随包字体（Fira Sans / Fira Mono）
│   └── wb2api.keystore          签名密钥（.gitignore 排除）
├── workbuddy2api-panel/         Go 网关（module 根，CI 的 working-directory）
│   ├── cmd/                     入口：server / login / credit / trial
│   ├── internal/                业务包（每包 test 与被测代码同目录，见下）
│   ├── internal/panel/app.js    网页面板前端（go:embed 打进二进制）
│   └── scripts/                 上游自带的任务探测脚本（Python）
├── tests/                       **所有测试代码的落点**（见 tests/README.md）
│   ├── run.sh                   Go 单测 + 前端 JS 冒烟入口
│   └── android/                 App 侧自检（宿主 JVM 跑，不需要真机）
├── patches/                     构建与移植补丁（不是测试）
├── run.sh / rebuild.sh          运行与重编（命令行侧起停服务）
├── watchdog.sh                  命令行侧看门狗
├── push_github.sh               推送到 GitHub（PR / Release / APK 附件）
├── dist/                        构建产物（APK），.gitignore 排除
├── docs/                        文档与截图
├── design-system/               设计系统（令牌 → Android 映射）
├── tools/                       辅助工具（模型指纹脚本等）
└── VERSION / CHANGELOG.md / README.md / AGENT.md / NOTICE.md
```

## 三条硬规矩

1. **产品代码只放 `apk-src/java` 与 `workbuddy2api-panel/internal`**；测试代码只放
   `tests/` 或与 Go 包同目录的 `*_test.go`（Go 工具链要求，见 `tests/README.md`）。
2. **产物只进 `dist/`**：`patches/build_apk.sh` 默认写 `dist/`，`push_github.sh` 也从
   那里取附件。仓库根不再出现 `.apk` / `.idsig`。
3. **一次性脚本不进根目录**：调试用的 `foo.sh` 放 `$PREFIX/tmp`，用完即弃；要长期
   保留的进 `tests/`（测试）或 `patches/`（构建/移植）。

## 为什么 Go 单测仍与源码同目录

`go test` 的包加载规则要求 `*_test.go` 与被测包同目录。搬走只能变成黑盒测试，
会丢掉对未导出状态（锁、队列下标、计数口径）的断言。详见 `tests/README.md`。
