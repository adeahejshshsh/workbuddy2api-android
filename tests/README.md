# tests/ —— 所有「测试代码」的落点

这个目录存在的原因只有一个：**别让测试代码和产品代码混在一起**。

## 目录

| 路径 | 是什么 | 怎么跑 |
|---|---|---|
| `run.sh` | Go 单测入口（按包挑或全量），外加面板前端 JS 冒烟 | `bash tests/run.sh [all\|pkg…]` |
| `android/WatchdogSelfCheck.java` | 看门狗退避序列自检（宿主 JVM，不需要真机） | `patches/build_apk.sh` 会编进来跑 |
| `android/GwConfigSelfCheck.java` | 网关配置摘要自检（同上） | 同上 |

## Go 单测为什么不在这里

Go 工具链有一条硬约束：**`xxx_test.go` 必须和被它测试的包在同一个目录**，否则
`go test` 根本不认——它不是"文件放哪都行"的约定，而是包加载规则的一部分。搬走
等价于把测试改成黑盒（`package foo_test` + 只碰导出标识符），本仓 400+ 条用例里
绝大多数要读未导出状态（`p.taskJobs`、`a.tokens`、`q.items`…），改写成黑盒等于
把断言全部重写一遍、并且失去对内部不变量（锁、队列下标、计数口径）的覆盖。

所以 Go 侧的分离只能做到两层：

1. **命名**：测试文件一律 `*_test.go`，文件名带功能域前缀（`regression5_20261021_test.go`、
   `autotask_async_test.go`），列表里一眼能分出源码与测试；
2. **夹具**：测试数据放各包的 `testdata/`（Go 工具链默认忽略该目录，不参与编译，
   `internal/upstream/testdata/` 即此用法）。

如果哪天要真正物理分离，唯一干净的办法是把大类拆成子包（`internal/panel/tasks`、
`internal/panel/accounts`…），测试随领域走。那是架构重构，不是挪文件，得单独做。

## 新增测试往哪放

- 测 Go 代码 → 跟着包走，`internal/<pkg>/*_test.go`；
- 测 App（Java）里的纯逻辑 → `tests/android/`，并在 `patches/build_apk.sh` 的自检步骤里挂上；
- 测脚本/构建流程 → `tests/` 下新增 `*.sh`，从 `run.sh` 里调用。
