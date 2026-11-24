# 领域上下文与运行规范（CONTEXT）

## 1. 领域术语
- **额度池（Pool）**：按上游模型分组聚合的剩余额度（`GEMINI`、`CLAUDE` 与 `WEEKLY` 周限额），展示为 `remainingFraction * 100%` 的剩余百分比。
- **消耗速率（Burn Rate, `%/h`）**：基于最近一次重置之后的滑动窗口历史快照计算的加权每小时消耗百分点。
- **预计耗尽时间（Runway）**：`剩余百分比 / 消耗速率`，无明显消耗时显示 `--` / `STABLE`。
- **瓶颈池（Bottleneck）**：在有效额度池中预计最快耗尽（Runway 最短）的池，展示在看板左侧暗黑焦点栏中。

## 2. 独立周期与边界
- **额度轮询**：默认 `QUOTA_POLL_SECONDS=1800`（30 分钟），受 `QUOTA_PAUSE_WINDOWS` 约束。
- **OAuth 令牌刷新**：默认 `ANTIGRAVITY_AUTH_REFRESH_SECONDS=3600`（1 小时）独立刷新，同时支持 401 触发即时刷新重试与冷代理 EOF 传输重试。
- **推送节流**：画面像素缓存经 `SHA-256` 去重，且受 `MIN_PUSH_INTERVAL_SECONDS=60` 冷却限制。
- **严禁虚构数据**：采集失败时不得插值或猜测当前额度，仅回退展示历史缓存并置 `Stale=true`。
