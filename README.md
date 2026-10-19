# WorkBuddy2API · Android 自托管网关

把 [workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)（上游
[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)，MIT）打包成**能装在手机上常驻运行**的安卓 App：
网关二进制 + 管理面板 + 看门狗 + 保活，全部塞进一个 APK。

一台手机 = 一个 OpenAI 兼容网关：`http://127.0.0.1:7863/v1`（局域网访问把 `127.0.0.1` 换成手机 IP）。

* 包名 `com.dsh.wb2api`，minSdk 24 / targetSdk 34
* 下载：[Releases](../../releases) —— 装 APK，覆盖安装即升级（账号与 api_key 保留）
* 版本号唯一来源：仓库根 `VERSION`

---

## 能做什么

**对外提供 OpenAI 兼容接口**

* `/v1/chat/completions`（流式 / 非流式）、`/v1/models`
* 多账号池 + 轮转：失败自动换号、模型级冷却、成本分层（优先免费号）、会话粘性
* 双域路由：`[realm:]model` 前缀区分国内（cn）与海外（global），跨域自动隔离
* 上游错误原样透传（code / requestId / 原文），并附带 `gateway_hint` 说明该怎么处理

**管理面板**（App 内就是这套网页，`/panel/`）

* 绑账号（OAuth 设备授权 / cockpit 导入）、签到、余额刷新、保活、猫猫旅行、活跃上报
* 账号页可「全部接受 / 领取 / 一键完成可自动任务」
* 模型目录、请求记录、用量统计、配置在线修改（部分字段需重启）
* 手机端已做窄屏适配（触控目标 ≥40px、弹层改底部抽屉）

**手机上常驻的保障**

* **看门狗五层**：进程级（10s tick，退避 2→60s）/ 健康级（`/healthz` 45s 无响应判僵死）/
  AlarmManager / JobScheduler（系统托管，跨重启仍在）/ partial wakelock（锁屏不停摆）
* **被杀诊断**：网关退出码翻成人话（`rc=137` = 被强杀）显示在状态条；顶栏「系统重启 N」= 进程被杀过几次
* **防冻结**：常驻 1×1 全透明图层 + Wi-Fi 高性能锁 + 冻结自检（治「过一会儿连不上、拉回前台就好」）
* **任务列表划掉不停服务**（`stopWithTask=false`），只有通知栏「停止」才真停

---

## 怎么用

### 1. 装 APK

从 [Releases](../../releases) 下 `WorkBuddy2API.apk` 安装。

打开 App 后按提示点完「保活设置」三项：**省电白名单 → 显示在其他应用上层 → 闹钟与提醒**。
国产 ROM 再放行三处：**自启动 / 省电策略无限制 / 任务列表锁定后台**（各家叫法不同）。

### 2. 绑账号

App 内打开面板 → 添加账号 → 走 CodeBuddy 的 OAuth 设备授权（或导入 cockpit 导出的 JSON）。

### 3. 客户端接入

```
Base URL : http://<手机IP>:7863/v1
API Key  : 面板里的 key（点「复制密钥」）
```

本机自用就把 `<手机IP>` 换成 `127.0.0.1`。

> 安全：默认只监听 `127.0.0.1`。要对外提供服务必须同时把 `listen` 改成 `0.0.0.0:7863`
> **并**配置非空 `api_key`，否则启动会被拒绝（守卫在 `config.normalize`）。

### 4. 从源码构建

**APK**（不需要 Android Studio / Gradle）：

```bash
bash patches/build_apk.sh      # 产出 ./WorkBuddy2API.apk
```

需要 `aapt2`、`javac`（17）、`android.jar`、`d8`（在 `r8.jar` 里）、`apksigner`、`zip`、`keytool`，
以及自备 keystore（仓库不含签名密钥，口令走 `WB2API_KS_PASS` 环境变量）。

**服务端**（Go）：

```bash
bash rebuild.sh                      # 编 cmd/* 到 $PREFIX/lib/workbuddy2api/
bash patches/apply_panel_mobile.sh   # 改了面板 CSS 之后要重跑（面板 go:embed 进二进制）
```

---

## 目录结构

```
VERSION                  版本唯一来源
apk-src/                 App 源码（无 Gradle：AndroidManifest + java + res + assets + test）
patches/                 构建与移植脚本（build_apk.sh / apply_panel_mobile.sh / patch_*.sh）
workbuddy2api-panel/     上游服务端源码（Go，含手机端 CSS 补丁）
run.sh / rebuild.sh      命令行侧：起停服务 / 重编服务端
watchdog.sh              命令行侧看门狗（外部实例只提示、不抢端口）
AGENT.md                 踩过的坑与本机环境要点（给 AI / 维护者看）
CHANGELOG.md             变更记录
```

---

## 许可与致谢

* 上游服务端与面板：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)（MIT，见
  `workbuddy2api-panel/LICENSE`），本移植基于 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)
* 安卓移植部分（`apk-src/`、`patches/`、`run.sh` 等）同样以 MIT 发布，保留上游版权声明
* 本仓库**不含**任何账号凭据、签名密钥与本机运行数据（见 `.gitignore`）
