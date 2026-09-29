# 常见问题（完整）

> 本文档由 [README](../README.md) 拆分而来（内容与主文档同步维护）；返回 [README](../README.md)。

## 常见问题

### 429 code=6004（模型级限流）的冷却语义？

上游 `429` + `code 6004` 是**该模型的使用量超限**（msg 通常带「将在 YYYY-MM-DD HH:MM:SS UTC+8 重置」），**不是账号整体被限流**。网关的处理：

- **只停「该账号 × 该模型」这一个组合**：冷却写进 `modelCooldowns[model]`，**不碰账号级 `until`**——同一账号的其它模型、池里的其它账号都不受影响
- **冷却截止 = 上游给的墙钟**：msg 带「将在 … 重置」时按 UTC+8 解释后**直接采信**，不再被 `soft_rate_max`（2h）截断——6004 的重置窗口常达数小时，截断会提前解冻、解冻即再撞 429。担心上游给出异常远时刻时可配 `cooldown.model_rate_max`（如 `24h`）设上限，空 = 不封顶
- **到期自动回到调度**：条目到期即被回收，该组合重新参与选号；`model_cooldowns` 随 `state.json` 持久化，重启不丢
- **无文案也按模型级**：6004 但没带「将在 … 重置」→ 模型级有界退避（`soft_rate` 基数起、按该组合命中次数翻倍、封顶 `model_rate_max` 或 `soft_rate_max`），**仍然不波及账号级**
- **该模型全池无号可用 → 429 + 原因**：池里的号账号级都健康、只是这个模型都被 6004 限额时，选号阶段直接回 `429 rate_limit_exceeded`，message 说明几个号受限 / 最早何时恢复 / 建议先换模型，而不是笼统的 503「网关没有可用账号」

### 多图会话请求体超限怎么办？

网关默认对聊天请求体设 **128MB** 上限（`server.max_body_mb`）：超限在读入前 / 读入中即 **413** 拒绝，不把超大 body 完整读进内存——这是对「内存放大」的兜底（恶意 / 异常客户端用超大 body 打满内存），正常多图 / 长上下文会话远低于该值。

- 确有更大的合法请求（历史图片每轮以 base64 重发，编码再膨胀约 37%）：把 `server.max_body_mb` 调大即可
- **完全对齐上游**（任意大小完整读入转发，超限交由上游自然返回错误——此前的透传语义）：设 `server.max_body_mb: 0` 关闭本上限
- 若上游返回 413/超限错误，网关按既有错误分类链路如实透传（不打码、不罚号——超限是请求侧问题）
- 客户端中途断流导致的半截 body 在读入阶段即报 `400 invalid_request`，不会把截断 JSON 喂给上游（issue #41 语义保留在读错误路径）

### Docker 部署登录后报「写入 auths/…json.tmp 失败： permission denied」？

容器以 `app` 用户（uid 10001）运行，而宿主机挂载的 `./auths`、`./data` 目录属主不是它——写凭证 tmp 文件被拒。三种解法任选（前两种均**无需 root 容器**）：

```bash
# 方案 1（推荐，非 root）：让容器以你自己的 uid 运行——挂载目录本来就是你建的
PUID=$(id -u) PGID=$(id -g) docker compose up -d --force-recreate
# 或写进 .env 文件长期生效（.env 已被 .gitignore 忽略）：
#   echo "PUID=1000" > .env && echo "PGID=1000" >> .env

# 方案 2：把挂载目录属主交给容器默认用户（需要 sudo）
sudo chown -R 10001:10001 ./auths ./data ./config.json

# 方案 3：compose 设 user: "0:0" 以 root 运行（NAS/群晖不便 chown 时用）
```

报错信息里自带这条指引；compose 的 `user` 已参数化为 `${PUID:-10001}:${PGID:-10001}`。

### 账号被 Disable 后如何恢复？

- **用 `./login.sh` 重新登录**覆盖凭证，重启后自动回池；
- 或源码侧调用 `Pool.ReviveDisabled(uid)` 清除 `disabled` 状态（`state.json` 同步刷新）。

### 系统提示词被内容策略误杀怎么办？

默认 `prompt.mode=passthrough`（对齐上游）透传客户端 system，首遇拦截会自动换 Degraded 中性提示词同请求重试一次；想从源头消除 system 来源的误报可切 `custom`（网关自有提示词**替换**客户端 system）或 `append`（两者并用）。用户 / assistant 消息中的指纹串由 `features.sanitize_blacklist_fingerprints` 清洗，两层叠加。

### 出站 UA 需要改吗？官网「使用端」列如何显示为 WorkBuddy？

官网「使用端」列按出站请求 UA 服务端归因。**默认已对齐官方桌面形态，无需配置**：chat / refresh / 模型列表走三段式 `WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1`（global 账号平台段自动切 `WorkBuddy AI`），billing / 签到域单段 `WorkBuddy/5.5.4`。需要微调版本或整体自定义时用 `upstream.client_version` / `upstream.cli_version` / `upstream.user_agent`（或环境变量 `WB2A_CLIENT_VERSION` / `WB2A_CLI_VERSION` / `WB2A_USER_AGENT`）。
