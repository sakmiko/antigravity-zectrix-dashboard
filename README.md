# Antigravity 额度预测看板 · ZECTRIX NOTE4

**纯 Go 实现的 Google Antigravity 额度预测看板**，为 400×300 横屏 16 级灰阶墨水屏（ZECTRIX NOTE4）渲染，单文件静态二进制、零 CGO、零第三方依赖。

把 Antigravity 订阅池的**剩余额度、消耗速率、预计耗尽时间**画成一张"油表"，按页推送到墨水屏——抬眼三秒就知道今天还能写多久。

![渲染预览](assets/preview-final.png)

---

## 一、它解决什么问题

Antigravity 的额度按池分配（`GEMINI` / `CLAUDE` / `WEEKLY` 周限额），官方界面只给当前百分比，不给趋势。等发现"突然不能用了"再回头找原因，通常已经晚了。

这个看板把**趋势**搬到常亮的墨水屏上：

- 还剩多少（`RemainingPct`）
- 每小时烧多少（`BurnRatePctH`）
- 按当前速率还能撑多久（`Runway`）
- 哪个池最先见底（`Bottleneck`，左栏黑卡聚焦）

## 二、界面解读

400×300 一屏五块：

| 区域 | 内容 |
|---|---|
| 顶栏 | `ANTIGRAVITY` 标识 + 数据时刻（取快照 `UpdatedAt`，非墙上时钟） |
| 左黑卡 | 瓶颈池：`RUNWAY` 大字 + 池名 + 消耗速率 + 余量进度条 |
| 右双行 | `GEMINI` / `CLAUDE`：大号百分比、`RST` 重置时刻、`~xx h` 预计耗尽、`-x.x%/h` 消耗速率、进度条 |
| 中部 | `FORECAST` 预测图：历史实线 + `NOW` 竖虚线 + 未来虚线外推，纵轴 0/50/100 点状网格 |
| 底部 | `WEEKLY` 周限额进度条 + 周期剩余时间 |

术语：

- **Pool（池）**：按上游模型分组的额度聚合，展示为 `remainingFraction * 100%`
- **Burn Rate（消耗速率 `%/h`）**：以最近一次重置之后的滑动窗口加权线性外推
- **Runway（预计耗尽）**：`剩余百分比 / 消耗速率`；无消耗时显示 `--`
- **Bottleneck（瓶颈池）**：有效池中 Runway 最短者

## 三、工作原理

```
CPA JSON (antigravity-*.json)
        │  首次读取后可选 AES-256-GCM 原地加密
        ▼
   OAuth 刷新（独立周期 + 401 触发重试）
        │
        ▼
POST cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels
        │  （主备自动切换 daily-cloudcode-pa）
        ▼
   快照落盘 STATE_FILE ──► 滑动窗口重算 Burn Rate / Runway / Forecast
        │
        ▼
  渲染 400×300 16 级灰阶 PNG（MonoGrid 点阵字库 + SDF 抗锯齿）
        │  SHA-256 像素去重 + 最小推送冷却
        ▼
POST {ZECTRIX_BASE_URL}/open/v1/devices/{DEVICE_ID}/display/image
     multipart: pageId / dither / images[]
```

三条独立周期互不阻塞：**额度轮询**（默认 30 min）、**OAuth 刷新**（默认 1 h）、**推送节流**（默认 60 s 冷却 + 像素去重）。采集失败时**不插值、不猜测**，仅回退展示有效期内的最后缓存并标记过期。

## 四、快速开始

```bash
git clone https://github.com/sakmiko/antigravity-zectrix-dashboard.git
cd antigravity-zectrix-dashboard

cp .env.example .env      # 填 CPA_AUTH_PATH 与 ZECTRIX 设备凭证

go build -trimpath -ldflags="-s -w" -o bin/antigravity-zectrix ./cmd/antigravity-zectrix
```

四条常用命令：

```bash
# 离线渲染预览图（不需要任何凭证，用内置样例数据）
./bin/antigravity-zectrix --preview preview.png

# 校验 CPA JSON、采集一次额度并打印预测摘要（不推送设备）
./bin/antigravity-zectrix --check

# 完整走一遍：采集 → 渲染 → 推送到墨水屏
./bin/antigravity-zectrix --trigger-refresh

# 强制刷新 OAuth Token（可选原地加密落盘）
./bin/antigravity-zectrix --refresh-auth
```

其它参数：`--env` 打印脱敏后的生效配置，`--version` 打印版本。不带参数启动即进入守护模式。

## 五、配置一览

全部通过环境变量或同目录 `.env` 生效（已存在的环境变量优先，不会被 `.env` 覆盖）。

| 变量 | 默认值 | 说明 |
|---|---|---|
| `CPA_AUTH_PATH` | `./fixtures/cpa-antigravity-example.json` | CLIProxyAPI 生成的 `antigravity-*.json`；也可指向目录自动扫描 |
| `CACHE_ENCRYPTION_KEY_FILE` | *(空)* | 非空时首次采集成功后以 AES-256-GCM 原地加密凭证（密钥由该文件 + `/etc/machine-id` 派生） |
| `ANTIGRAVITY_OAUTH_CLIENT_ID` / `_SECRET` | *(空)* | CPA JSON 未携带时回退使用 |
| `HTTP_PROXY_URL` | *(空)* | 显式代理；**留空时自动采纳标准 `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY`** |
| `ANTIGRAVITY_QUOTA_BASE_URL` | `https://cloudcode-pa.googleapis.com` | 额度接口主地址，失败自动切 `daily-cloudcode-pa` |
| `QUOTA_POLL_SECONDS` | `1800` | 额度轮询周期，程序强制下限 `300` |
| `ANTIGRAVITY_AUTH_REFRESH_SECONDS` | `3600` | OAuth 刷新周期，下限 `300` |
| `QUOTA_PAUSE_WINDOWS` | *(空)* | 静默窗口，如 `01:00-07:00`，逗号分隔，支持跨零点 |
| `TIMEZONE` | `Asia/Shanghai` | 时刻显示与静默窗口判定的时区 |
| `STATE_FILE` | `./state.json` | 历史快照与状态落盘位置 |
| `HISTORY_MAX_HOURS` | `12` | 参与预测的历史窗口长度 |
| `STALE_AFTER_SECONDS` | `5400` | 超过该时长未采集则标记过期 |
| `ZECTRIX_BASE_URL` | `https://cloud.zectrix.com` | 上屏服务地址 |
| `ZECTRIX_DEVICE_ID` | *(必填)* | 设备 ID，形如 `AA:BB:CC:DD:EE:FF` |
| `ZECTRIX_API_KEY` | *(必填)* | `X-API-Key` 鉴权头 |
| `ZECTRIX_PAGE_ID` | `antigravity-quota` | 目标页位，取值 `1` / `2` / `3` |
| `ZECTRIX_DITHER` | `false` | 是否启用硬件抖动；墨水屏一般关闭 |
| `MIN_PUSH_INTERVAL_SECONDS` | `60` | 推送最小冷却，下限 `60` |

## 六、systemd 部署

```ini
[Unit]
Description=Antigravity 额度预测看板
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=antigravity-zectrix
Group=antigravity-zectrix
WorkingDirectory=/opt/antigravity-zectrix
EnvironmentFile=/var/lib/antigravity-zectrix/service.env
ExecStart=/opt/antigravity-zectrix/antigravity-zectrix
Restart=always
RestartSec=10
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/antigravity-zectrix

[Install]
WantedBy=multi-user.target
```

要点：`WorkingDirectory` 决定 `.env` 查找位置；`STATE_FILE` 与凭证必须落在 `ReadWritePaths` 允许的目录内。

## 七、目录结构

```
cmd/antigravity-zectrix/   入口与 CLI、守护循环、采样/预测装配
internal/antigravity/      额度接口客户端（主备切换、超时、代理）
internal/auth/             CPA 凭证解析、AES-256-GCM 加解密、OAuth 刷新
internal/config/           环境变量与 .env 装载、静默窗口、脱敏摘要
internal/predictor/        快照存储、滑动窗口消耗速率、Runway 与预测
internal/render/           400×300 画布、MonoGrid 点阵字库、SDF 抗锯齿、看板编排
internal/publisher/        ZECTRIX 推送、像素去重、限流
fixtures/                  示例 CPA JSON（无真实凭证）
deploy/                    systemd 单元模板
```

## 八、排错

| 现象 | 原因与处理 |
|---|---|
| `dial tcp [2001:...]:443: connect: network is unreachable` | 机器无 IPv6 出口。设 `HTTP_PROXY_URL`，或直接导出标准 `HTTPS_PROXY`（v1.0.1 起自动采纳） |
| `Could not determine client ID from request` | 凭证里没有 `client_id`，补 `ANTIGRAVITY_OAUTH_CLIENT_ID` / `_SECRET` |
| `stat CPA auth path ...: no such file or directory` | v1.0.1 之前要求目标文件已存在；升级后按"待创建文件"处理 |
| 画面停住不动 | 先跑 `--check`：401/403 是凭证失效；`Stale=true` 是采集失败后回退缓存（设计如此） |
| 推送返回 4xx | 核对 `ZECTRIX_BASE_URL`（实机是 `cloud.zectrix.com`）、`ZECTRIX_DEVICE_ID`、`ZECTRIX_API_KEY` |
| 预览图与上次不同 | 顶栏时刻取自快照 `UpdatedAt`，跨分钟才变；同一分钟内两次渲染字节完全一致 |

## 九、变更记录

**v1.0.1**

- 修复首次保存路径：目标文件不存在时不再报错，`SaveAtomic` 可创建新文件
- 未配置 `HTTP_PROXY_URL` 时回退 `http.ProxyFromEnvironment`，兼容标准代理环境变量
- `--preview` 自动创建输出目录（多级）
- module 路径改为 `github.com/sakmiko/antigravity-zectrix-dashboard`，全仓库 `gofmt` 归一

**v1.0.0** 首个版本。

## 十、声明与许可

- 本项目读取 **CLIProxyAPI** 格式的 Antigravity OAuth 凭证并调用上游 Cloud Code Assist 内部接口（`fetchAvailableModels`）。该接口**无官方公开文档与 SLA**，字段结构、配额分组与限流策略可能随时调整；以非官方方式访问消费者 OAuth 接口存在账号风控风险，请自评估并仅在授权范围内使用。
- 看板显示的百分比与周限额仅为订阅池余量估算，**不代表 Google Cloud / Vertex AI 账单金额**，也不是精确 Token 消耗（参见 [Google Antigravity Pay As You Go 文档](https://antigravity.google/docs/pay-as-you-go/)）。
- 消耗速率与耗尽时间基于本地历史滑动窗口线性外推，**不构成任何额度承诺**；开启 Boost、切到高消耗模型或并发多 Agent 时会显著偏离。
- 凭证仅在本地使用，可选原地 AES-256-GCM 加密；不向任何第三方回传凭证或日志。

MIT License，详见 [LICENSE](LICENSE)。

参考实现：[sakmiko/codex-zectrix-dashboard](https://github.com/sakmiko/codex-zectrix-dashboard)（同架构的 Codex 版本）。
