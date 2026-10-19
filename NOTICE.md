# 授权与来源说明

本仓库以 **MIT** 授权（见 `LICENSE`）。内容分两部分：

## 1. 上游服务端与面板 —— `workbuddy2api-panel/`

* 原作者：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)（MIT，见该目录内 `LICENSE`）
* 本移植基于的镜像：[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)

该目录是按上游源码原样放置的（未含 `.git`），只做了两处改动：

* `internal/panel/index.html` 内注入手机端 CSS 补丁（`DSH-MOBILE-PATCH-BEGIN/END` 夹住，
  内容来自 `patches/panel-mobile.css`，可重跑 `patches/apply_panel_mobile.sh` 覆盖）
* 无其它代码改动

## 2. 安卓自托管移植部分

`apk-src/`、`patches/`、`run.sh`、`rebuild.sh`、`watchdog.sh`、`push_github.sh`、
`README.md`、`AGENT.md`、`CHANGELOG.md` —— 同样以 MIT 发布，保留上游版权声明。

## 不含什么

本仓库**不含**任何账号凭据、签名密钥、本机运行数据与日志（见 `.gitignore`；
`workbuddy2api-panel/data/`、`auths/`、`config.json`、`apk-src/wb2api.keystore`、
成品 APK 都被排除，成品只走 Releases 分发）。
