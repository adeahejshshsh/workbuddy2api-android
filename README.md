# WorkBuddy2API · Android 自托管网关

把 [workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)（上游
[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)，其服务端部分仍为 MIT）打包成**能装在手机上常驻运行**的安卓 App：
网关二进制 + 管理面板 + 看门狗 + 保活，全部塞进一个 APK。

一台手机 = 一个 OpenAI 兼容网关：`http://127.0.0.1:7863/v1`（局域网访问把 `127.0.0.1` 换成手机 IP）。

> [!IMPORTANT]
> **使用声明与免责声明见文末（必读）**：本项目仅供个人管理自己的账号；反对并禁止批量注册、
> 售卖额度、二次加壳与卡密分发。上游已于 2026-10-21 停止维护，上游随时可能彻底收紧导致不可用，
> 风险自担。协议为**保留所有权利（All Rights Reserved）**。

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
* **安卓移植与运维部分（`apk-src/`、`patches/`、`run.sh` 等）：保留所有权利（All Rights Reserved）**，见 `LICENSE`
* 上游服务端与面板部分（`workbuddy2api-panel/`）仍依其原始 MIT 授权，原始版权声明与许可声明完整保留在 `NOTICE.md`
* **使用声明 / 免责声明 / 上游停止维护声明：本文文末已全文收录**，`NOTICE.md` 保留同一份原文与上游 MIT 授权全文
* 本仓库**不含**任何账号凭据、签名密钥与本机运行数据（见 `.gitignore`）

---

## 使用声明与免责声明（必读）

### 免责声明

本项目仅供学习和研究使用。使用者需遵守 CodeBuddy 服务条款，自行承担使用风险
（包括账号封禁、条款违约等）。作者不对任何因使用本项目产生的直接或间接损失负责。

### 使用声明

本项目（含上游 Sliverkiss/workbuddy2api，下同）的开发初衷只有一个：**方便个人管理自己的
CodeBuddy 账号**——自动签到、保活、给自己的本地工具提供一个 OpenAI 兼容入口。它是免费、
按「原样」提供的个人自用工具。

近期发现有人将本项目用于以下行为：

- **批量注册小号 / 收购账号，对外提供付费 API、共享池、代充等业务**；
- **二次加壳、捆绑卡密（授权码）售卖**，或以「公益服」「低价中转」等名义变相收费分发。

我们对上述行为**表达最强烈的反对**，并声明如下：

1. **一切商用 / 售卖行为与本项目及作者无关。** 本项目不授权、不支持、不参与任何面向公众的
   API 售卖、账号池出租、卡密收费分发；行为人由此产生的一切后果（包括但不限于账号封禁、
   条款违约与法律风险）由其自行承担，与作者和贡献者无任何关系。
2. **批量注册与转售接口配额违反目标平台服务条款。** CodeBuddy / 腾讯系服务条款禁止批量注册
   账号及商业转售接口。上游仓库已删除、停止公开维护——我们无法断定具体原因，但此类滥用行为
   正在毁掉所有正常使用者的环境，请勿再消耗社区的善意。
3. **请勿购买任何「收费版」「卡密版」「公益中转版」。** 本项目永远免费。任何加壳、加密、捆绑
   收费的「版本」都是他人篡改的产物，与本项目无关；且此类分发无法审计，存在被植入后门、回传
   并窃取你 CodeBuddy 凭证的风险（`auths/` 中保存的是明文 accessToken / refreshToken）。
4. **关于开源协议的诚实说明。** 上游基于 MIT 协议开源，协议允许自由使用与修改源码——这是开源
   的本意；但 MIT 赋予的是代码层面的自由，**不赋予**以本项目名义宣传、售卖、捆绑分发，或要求
   作者提供支持与背书的权利。作者不为任何第三方分发版本提供支持、更新承诺或安全保证。
5. **作者保留止损的权利。** 若滥用行为持续，作者可能随时停止维护、关闭或删除仓库，且不另行通知。

如果你的用途是管理自己的账号，欢迎正常使用。

### 上游停止维护声明（2026-10-21）

> **项目已停止维护（2026-10-21）**：后续不再更新代码、不跟进上游变化、不再处理新的 issue / PR。
>
> 原因有三：① **上游已开始禁用反代**——WorkBuddy 对非官方客户端的识别与拦截正在收紧，
> 反代通道的稳定性与存活周期已不可控；② **个人精力有限**——无力长期跟进协议变化与风控对抗；
> ③ **非法转卖泛滥**——大量批量注册小号、售卖额度、二次加壳 / 卡密分发，与初衷完全相悖。
>
> 仓库保持现状可用（已合入的修复不会回退），但请自行评估风险：**上游随时可能彻底收紧导致
> 不可用**；因使用非官方客户端导致的账号风控 / 封禁，由使用者自行承担。
>
> **维护方式披露**：上游近期的 issue / PR 处置、代码审查、测试与修复实现**大量由 AI 辅助完成**
> （自动化编码 agent），合并与发布决策由维护者作出。

### 本项目的授权

**保留所有权利（All Rights Reserved）** —— 未经书面许可不得复制、分发、修改、商用、二次打包
或以「公益服 / 低价中转 / 卡密」名义对外服务，也不得移除声明。详见 `LICENSE`。

例外：`workbuddy2api-panel/` 属第三方，仍依其原始 **MIT** 授权，原始版权声明与许可全文保留在
`NOTICE.md`（含上游双版权：Sliverkiss/workbuddy2api 与 linguo2625469/workbuddy2api-panel）。
