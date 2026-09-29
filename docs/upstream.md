# 基线沿革与内部对照

> 本文档由 [README](../README.md) 拆分而来（内容与主文档同步维护）；返回 [README](../README.md)。

## 🆚 与上游的差异（继承自增强分支）

本节记录的是**基线**带来的能力与沿革：增强分支 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) 相对 [原始项目 master](https://github.com/Sliverkiss/workbuddy2api) 的增量（均已在真实多账号环境验证），以及本项目早年的两轮 fork 同步记录——这些能力本项目**继承并保留**，属基线功劳；本项目自己在基线之上的增量见 [本项目的二开特性](../README.md#-本项目的二开特性)。

### 基线新增能力

| 能力 | 说明 |
|---|---|
| **Web 管理面板** | `internal/panel`，前端 go:embed 单文件进二进制，零外部依赖。账号池可视化（健康色条 / 积分量条 / 冷却倒计时）、单号运维、批量任务、日志查看、明暗主题 |
| **浏览器内 OAuth 添加账号** | 面板「添加账号」按钮完成设备授权 → 凭证落盘 → **热加载进池（免重启）**，替代命令行 `login.sh` 流程 |
| **在线配置编辑（热生效）** | 面板直接改 `config.json`：API 密钥 / `soft_rate` / 脱敏开关 / 池参数 / 任务排程**立即生效**；装配期字段（listen 等）保存后提示需重启。写入采用深合并 + 原子替换，保留未知键 |
| **积分任务体系** | 任务列表 / 接受 / 领取接口 + 面板弹窗；「一键完成」覆盖 **17 个任务**（对话 / 领养 / 桌面行为链 / 模板 / 灵感案例 / 画布 / 专家召唤 / 技能尝鲜 / 主题 / 资料库 / 夜猫子等），推进进度、等待异步计分落定后**自动领奖**，纯 API 零客户端依赖 |
| **首启自动生成配置** | 目录下无 `config.json` 时自动生成推荐配置（含 `crypto/rand` 随机 `api_key`），双击即开 |
| **粘性会话内容回退** | 客户端不发 `conversation_id` 时，用 `system + 首条 user` 哈希派生会话键（`d-` 前缀），通用 OpenAI 客户端也能享受粘性 |
| **余额后台刷新** | `schedule.balance_refresh_minutes`（默认 5）周期查余额并更新池，冷却账号余额恢复自动解冻 |
| **模型能力透出** | `/v1/models` 附带 `supported_efforts` / `default_effort` / 积分倍率 / 输入输出上限等上游真实字段 |
| **安全加固** | 常量时间密钥比较（`internal/httpauth`）、CSP 与安全响应头、UID 白名单防路径穿越、前端属性转义修复 |
| **领养前置修复** | 上游 `travelAdopt` 缺 report 前置导致领养恒失败于 `first_buddy task not completed yet`；本分支修正后实测 +300 到账（3/3 账号） |

### 同步上游

**第一轮（fork 基线 `53ee3a1` → `9a87758`，34 个提交）**：定时任务独立排程（当时四类：签到 / 旅行 / 活跃 / 保活）、pool 文件拆分、12153 连续计数才禁用、429 `code=6004` 模型级限流收窄、11101 不罚号、请求体 413、DeepSeek 思维链、reasoning_content 回填、Codex 指纹脱敏、系统提示词体系、出站 UA 可配等。

**第二轮（`9a87758` → `ea8b1e5`，2026-09-14，只吸收底层）**：

| 上游改动 | 吸收内容 |
|---|---|
| 净化增强 | `tool_calls.arguments` 盲区修复（content=null 的工具调用轮此前完全漏净化）、裸 `11128` 反探测改写、桌面版身份句（逗号形态）漏网修复、反馈句整句改写 |
| 出站头族 | UA 对齐官方三段式 `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<ver>`（默认 5.5.4/2.137.1，可配）；`X-IDE-*` 用量归属四头 + `X-Agent-Purpose`（`client_name` 配 `WorkBuddy` 即对齐官方桌面端）；`X-Device-Token` 设备风控头（auth 每号 / config / 文件三源）；`X-IDE-Version` 补齐 |
| 并发修复 | 客户端 IP 改按请求参数传递（消除共享字段竞态）；billing 单段 UA 形态 |
| 签到幂等 | `IsAlreadyCheckin` 识别"今天已签到"（code=10001/14001），调度日志不再把重复签到当失败 |
| 粘性按模型判活 | 会话绑定的账号被 6004 模型级限额后，换模型请求自动解绑重分配（治"限额后换不动号"）；`/healthz` 探活计入模型豁免形态（治"全号被单模型限流探活误报 503"） |
| report 增强 | `ReportChatActivity` 支持独立 `requestID`（同会话多轮上报各条可区分） |

未吸收（明确不做）：脚本体系（task_runner/school 脚本—我们已有更完整的纯 API 实现）、governance/CI workflow、成本账本选号（依赖 usage.credit 观测，收益待验证）。

### 未做 / 待办

| 状态 | 事项 | 说明 |
|---|---|---|
| ✅ 已修复 | ~~single 类任务奖励领取~~ | **领奖已打通**：正确端点是 Web 域 `POST https://www.workbuddy.cn/activity/growth/tasks/<task_code>/claim`（任务码在路径、无 body、`x-client-platform: web`）。此前误用 CLI 域 `copilot.tencent.com/v2/.../reward/claim` 导致长期 400。「一键完成」现已**达标即自动领奖**（含异步计分等待），面板也可手动领取。实测 +100 分 +5 能到账、重复领取幂等 |
| ✅ 已破解 | ~~桌面端 / 交互类任务~~ | 通过客户端指纹逆向（`/v2/report` 三通道 + 判据事件载荷），**17/18 任务可纯 API 一键完成**：`RichMeow_Chat`（桌面 6 事件链）、`Buddy_App(_QQ)`、`automation_1`、`Library_read`、`template_5`、`playbook_prompt`、`create_canvas`、`expert_5`、`Expert_team_use_3`、`Expert_lighthouse`、`Hp_Appearance`、`skill_1`、`black_cat`（夜间窗口自动补足）等，多账号实测点亮 |
| ⚠️ 不支持 | **剩余 1 个任务** | `Expert_Philanthropy`（需真实捐款：服务端领奖时校验捐赠回执，已实测无法绕过）；面板展示指引 |
| ❌ 未做 | **面板侧 Upstash / 凭证目录配置** | 涉及启动期装配，需手工编辑 `config.json`（面板会提示为重启项） |
| ❌ 未做 | **HTTPS / 内置限流** | 设计上交给反向代理（Nginx / Caddy）。服务本身只提供明文 HTTP，公网部署**必须**置于 HTTPS 反代之后 |

## 关键断言 ↔ 代码出处

> 出处以**文件 + 符号**为准（不写行号：行号随重构漂移，符号可 grep 定位）。

| 断言 | 出处 |
|---|---|
| `prompt.mode` 默认 `passthrough` | `cmd/server/config.go`（`Default()`） |
| 请求体上限 `server.max_body_mb` 默认 128MB（0 = 不限） | `cmd/server/config.go`（`Default()`）；读取点 `internal/server/handler.go`（chatCompletions 读 body 段） |
| 出站强制 `stream:true` | `internal/upstream/payload.go` |
| 多协议入口（Anthropic / Responses / Gemini）与 OpenAI 互转 | `internal/convert`（`AnthropicToOpenAI` / `ResponsesToOpenAI` / `GeminiToOpenAI` 与三个 `*Stream` 编码器）；路由与管道 `internal/server/protocol.go`（`runPipeline` / `relayOpenAIStream`）；注册点 `internal/server/handler.go`（`NewHandler`） |
| 多协议鉴权凭证位置兼容（`x-api-key` / `x-goog-api-key` / `?key=`） | `internal/server/protocol.go`（`protoToken` / `withAuthAny`）、`internal/httpauth/httpauth.go`（`VerifyToken`） |
| 模型级 6004 冷却精确对齐上游墙钟（`model_rate_max` 为可选封顶，默认不封顶） | `internal/pool/cooldown.go`（`CooldownSoftForModel` / `cappedModelUntilLocked` / `modelBackoffCapLocked`）；配置解析 `cmd/server/config.go`（`ModelRateMaxDur`）；注入 `cmd/server/main.go` |
| 该模型全池限额 → 429 + 原因（而非 503） | `internal/server/handler.go`（`ModelBlockSummary` 归因分支）、`internal/pool/cooldown.go`（`ModelBlockSummary`） |
| 面板展示模型级限额（逐行 + 折叠） | `internal/panel/app.js`（`rlmRows` / `hmClock` / `renderAccounts`）；`internal/panel/index.html`（`.rlm-box` 样式） |
| DeepSeek 思维链注入（`thinking.type=enabled`） | `internal/upstream/thinking.go`（`injectThinking`） |
| `reasoning_effort` 默认档兜底 = `high` | `internal/upstream/thinking.go`（`defaultDeepSeekEffort`） |
| `reasoning_content` 多轮回填（assistant 消息） | `internal/upstream/thinking.go` |
| Degraded 中性提示词常量 | `internal/prompt/prompt.go` |
| 降级触发与次日 00:00 CST 重置 | `internal/server/degrade.go`（`Trigger` / `nextMidnightCST`） |
| 内容拦截不罚号 + `passthrough` / `append` 降级重试 | `internal/upstream/client.go`（`Classify`）、`internal/server/handler.go`（`applyErrorPolicy`） |
| 6004 模型级限流 code 与重置时间解析 | `internal/upstream/client.go` |
| `11101` / Unmarshal 失败不罚号 | `internal/upstream/client.go`（`Classify`）；处理分支 `internal/server/handler.go`（`applyErrorPolicy`） |
| 出站 UA 默认官方三段式（可整体覆盖） | `internal/upstream/headers.go`（`defaultClientVersion` / `defaultCliVersion` / `defaultWorkBuddyUAFor`）；配置接线 `cmd/server/config.go` |
| session-dead 连续阈值 3 才禁用 | `internal/pool/entry.go`（`sessionDeadThreshold`）；调用点 `internal/scheduler/scheduler.go` |
| `ReviveDisabled` 人工复活 | `internal/pool/state.go` |
| disabled 账号透出 `disabled_reason` | `internal/pool/entry.go`（State 字段）、`internal/pool/state.go` |
| 硬冷却至次日 04:00 | `internal/pool/cooldown.go`（`CooldownUntilTomorrow4AM`） |
| 软冷却退避封顶 2h | `internal/pool/entry.go`（`defaultSoftRateMax`） |
| Top-5 候选短名单 | `internal/pool/pool.go`（选号入口） |
| `activity_hours` / `growth_hours` 默认 `[10]` / `[1]` | `cmd/server/config.go`（`Default()`） |
| 成长任务队列排程挂载 | `internal/scheduler/scheduler.go`（`taskGrowth` / `GrowthHook`）、`internal/panel/taskcenter.go`（`RunGrowthQueueOnce`） |
| 活跃自检回读 streak | `internal/scheduler/scheduler.go`（`checkActivityStreak`） |
| streak 端点 `activity/growth/streak` | `internal/upstream/travel.go`（常量 / `GrowthStreak`） |
| Redis 粘性镜像 7 天 TTL | `internal/redisstore/redisstore.go` |
| 模型上下文 / 输出上限四级查找链 | `internal/upstream/context_catalog.go` · `internal/upstream/model.json` · `/v1/models` 组装在 `internal/server/handler.go` |
