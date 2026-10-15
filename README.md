# adbot — Telegram 反广告 Bot

一个用 Go 编写的 Telegram 反灰黑产/反广告导流 Bot。入群时筛查新成员简介（bio），
群内发言按**签名库 + AI 双通道**判定，命中即删除触发消息并封禁/踢出；同时维护
「观察名单」与「bio / 消息」两级 sha1 签名库，逐渐积累无需 AI 的本地命中知识。

本仓库只服务白名单群（`BOT_CHATS`），非白名单内的事件/消息一律忽略，零 AI 调用、零写入。

## 库选型

- **Telegram 客户端**：`github.com/go-telegram/bot v1.27.0`（2026-09 仍在维护，零传递依赖）。
  对比结论：`go-telegram-bot-api/v5`（2021 停更）、`telebot.v3`（2024 停更）均不选。
  库只包裹在 `internal/telegram` 薄接口后，**库的 models 不泄漏到 bot 决策层**，
  决策层依赖接口 + fake，单测用 `WithServerURL` 指向 `httptest`，全程离线。
- **AI 客户端**：`github.com/sashabaranov/go-openai v1.43.0`（OpenAI 兼容），模型回退链、
  多模态（图+文）、并发复核投票、body 捕获（网关 200+错误体）都包在 `internal/ai` 后。
- **存储**：默认 `memory`（见下）；Redis 保留但仅显式启用，file **明确不实现**（回落内存+WARN）。

## 架构

```
                      ┌──────────────────────────────┐
 Telegram Bot API ───▶│  internal/telegram (薄封装)    │
  (go-telegram/bot)   │  Start(ctx)+WithAllowedUpdates │
                      │  + WithErrorsHandler           │
                      └──────────────┬───────────────┘
                                     │ Update（自有领域类型）
                                     ▼
                      ┌──────────────────────────────┐
                      │        bot（决策逻辑）          │
                      │  命令 / 入群 / 成员变更 / 发言    │
                      │  白名单门 → 签名库 → AI → 投票    │
                      │     → 重试队列 → 本地规则兜底     │
                      └──────┬──────────────┬────────┘
                             │              │
                  ┌──────────▼──┐    ┌──────▼─────────┐
                  │ internal/ai │    │ internal/store  │
                  │ go-openai   │    │ Memory(默认)     │
                  │ 回退链+投票   │    │ +SimHash 索引    │
                  │ prompts/嵌入 │    │ +Sweeper/Janitor│
                  └──────┬──────┘    └─────────────────┘
                         │  ┌────────────────────────┐
                         └─▶│ internal/retryq        │
                            │ 10s×5 固定重试→本地兜底  │
                            └────────────────────────┘
                      ┌──────────────────────────────┐
                      │  HTTP :3000                    │
                      │  / /healthz /readyz /statusz  │
                      │  /admin/signatures (带 token)  │
                      └──────────────────────────────┘
```

## 入群处理顺序（amend11 定稿）

```
入群
 ├─ 记录观察名单
 ├─ 签名库匹配 ─ 命中 → 处置（不欢迎、不登记验证）
 ├─ 四条件裸号必杀 ─ 命中 → 处置（不欢迎、不登记验证）
 ├─ 有简介 → AI 判简介
 │    ├─ 结论【广告】→ 处置（**不发欢迎、不登记验证**）
 │    ├─ 结论【非广告】→ 欢迎消息 + 300 秒贴纸倒计时
 │    └─ 判定【失败】→ 重试队列（10s × 最多 5 轮 → 本地规则兜底）
 │           · 重试期间**不欢迎、不登记**，等最终结论落地
 │           · 结论非广告 → 才欢迎 + 起倒计时 ／ 结论广告 → 处置（也不欢迎）
 └─ 无简介 → 直接欢迎 + 300 秒贴纸倒计时
     发贴纸 → 通过 → 恢复「第一句发言」判定 ／ 300 秒未发 → 处置
     （到期处置前二次 getChatMember 确认：人已不在或不能发言 → 只清理不处置；
       我们自己处置过、别人踢过 → 立即撒手，绝不二次处置）
```

## 存储模式与数据丢失语义（重要）

- `BOT_STORE=memory`（**默认**）：观察名单 / 签名库 / 统计都在内存，**重启后丢失**。
  - 启动日志会打印：`内存模式：重启后观察名单/签名库/统计将丢失`。
  - 丢失的只是「签名缓存 + 观察名单 + 统计」；**封禁/踢出是 Telegram 服务端生效的，
    重启不会解除**。重启后同一广告号会重新走一次 AI 判定，不会漏判。
- `redis` / `file`：代码保留，显式设置才启用（file 后端本轮回落内存并打 WARN）。
- 为什么默认内存：belmo 文件系统疑似只读、平台 Redis 外网不通。

## TTL 与自动清理

- 每条记录带写入时间 `ts`，默认保留 **72h**（`BOT_RETENTION=72h`；`0`/负数 = 永不过期）。
- 分类覆盖：`BOT_RETENTION_SUSPECT`（观察名单）、`BOT_RETENTION_SIGNATURE`（签名库），不设跟随全局。
- **滑动过期**（`BOT_RETENTION_SLIDING=true` 默认）：命中/刷新即续 `ts`，热门签名不会被反复清掉。
- janitor goroutine 每 `BOT_SWEEP_INTERVAL`（默认 `10m`）扫一次，**启动时也先跑一次**，日志：
  `event=sweep`，字段 `dropped_suspects` / `dropped_signatures` / `remaining_total` / `heap_kib` / `elapsed_ms`。
- 容量护栏：`BOT_MAX_SUSPECTS` / `BOT_MAX_SIGNATURES`（各 `50000`），超限按最旧淘汰并打 WARN。
- **不存在消息追踪**：处置只删触发的那条广告消息，不记录观察期消息 id、不做批量删除。

## 命令

启动时用 `SetMyCommands` 注册。命令消息自身跳过广告判定。

| 命令 | 可用范围 | 说明 |
|---|---|---|
| `/start` | 任何聊天 | 中文状态卡：`🟢 在线` + 版本 + 运行时长 + 存储后端 + 该聊天是否在保护范围 |
| `/help` | 任何聊天 | 命令与行为说明 |
| `/status` | 仅管理员 | 模型链与逐模型成功/总数、回退、复核（含改判）、删除（含失败）、本地兜底、SimHash 命中、待决队列、观察名单 / 签名库 / 处置 / 轮询 / 存储与 TTL / 提示词版本 |
| `/check <文本>` | 仅管理员 | 文本直送 AI 并回显 JSON（`is_ad/category/confidence/reason`）+ 耗时，**不触发处置** |

「仅管理员」= `BOT_ADMIN_USERS` 白名单用户或群主/管理员。私聊（DM）命令始终可用。

## AI 判定链路（amend2/amend3）

1. **模型回退链**：`BOT_AI_MODELS`（默认 `pro → flash → ultraspeed-studio`，amend6 起挂死的 ultraspeed 挪到链尾）。429/5xx/
   超时/网络错误或网关池类错误码（`pool_parked`/`no_accounts`/`upstream_*`）时链内推进；
   解析契约违例**不**推进（换模型无益）。
2. **关思考**：每个请求带 `reasoning_effort=none`（备份 `chat_template_kwargs.enable_thinking=false`），
   绝不发送 `max_tokens`。
3. **图片多模态**：带图消息取最大尺寸图 + caption 一起判定，图片冲突时以图为准；
   同一 `file_unique_id` 判定结果缓存（文件 hash 去重）。
4. **置信度不足 → 三路并发复核**：`is_ad=true` 但 `confidence < BOT_AI_MIN_CONFIDENCE` 时
   在复核模型上并发投票，失败/超时路**弃权**；平票/不足 → 进重试队列再判一轮。
   复核投出「广告」时最终分取**判广告复核路的平均置信度**，仍受下面的处置门限约束。
4a. **处置门限（85 分以上才踢）**：AI 判广告但最终分（百分制）< `BOT_AI_MIN_PUNISH_SCORE`
   （默认 85）时**放过**（不踢不删，日志 `amend13_below_score_release` + `/statusz` 计数
   `ai_below_score_releases_total`）。门限只约束 AI 判定通道（主判定 / 复核 / 重试轮）；
   签名库、裸号必杀、入群验证超时、防刷缓存、本地规则兜底、上游敏感拦截不受影响。
5. **失败重试**：固定 10s 间隔、最多 5 次、不退避不熔断，队列容量 `BOT_AI_RETRY_QUEUE`
   （满则淘汰最旧并打 ERROR）。5 次用尽后**本地规则引擎自动兜底**（≥阈值处置，否则继续观察）。
6. **SimHash 防改字**：签名库同时建 64 位字符 trigram simhash 索引，海明距离 ≤
   `BOT_SIMHASH_DISTANCE`（默认 3）的变体（改一个字、插 emoji、加空格）直接命中，零 AI 调用。

## 提示词工程

- 提示词是独立文件 `app/internal/ai/prompts/judge_v2.txt`（+ `judge_v1_bio.txt` 简介
  追加段），go:embed 编译进二进制；`BOT_AI_PROMPT_FILE` 可用磁盘文件覆盖（调优不发版）。
- 版本号 `PromptVersion=judge_v2` 打进**每次判定日志**与 `/status`、`/statusz`。
- 只写「必杀特征清单」不写反例（防模型学到 false 范式）；输出契约严格 JSON
  （`is_ad`/`category`/`confidence`/`reason`）；解析宽容但契约违例=失败走重试。
- `category` 使用受控词表（办证｜色情｜儿童性内容｜赌博｜诈骗｜金融｜兼职｜引流｜违禁品｜
  账号交易｜其他），但解析**从不**枚举校验（输出漂移不丢判定）。
- 待判内容包裹在 `<msg>…</msg>` 之间，分隔符内的指令注入一律视为数据。

### `-prompteval` 调优环

```bash
cd app && go run . -prompteval            # 走真实网关（唯一允许外呼的路径）
go run . -prompteval -prompteval.models=mimo-v2.6-pro
```

对 `testdata/prompt_eval.jsonl`（29 条已标注样本）跑真实判定，输出每类错误统计
（FN/FP + 契约违例），目标是 FN 优先：必杀域召回 ≥95%、真阴性零误封。标注集与提示词
一起迭代：改 prompt → 跑 eval → 看逐条失败原因 → 再改。

## 环境变量

完整契约与 `config.go` 一致；未识别的 `BOT_*` 变量会在启动时打 WARN。
逐项中文注释与默认值见 `app/.env.example`（复制为 `.env` 使用）。

| 分组 | 变量 | 默认 | 说明 |
|---|---|---|---|
| 核心 | `BOT_TOKEN` | — | **必填**。BotFather 发放的 token |
| | `BOT_ADDR` | `:3000` | HTTP 监听地址 |
| | `BOT_CHATS` | 空 | 授权群白名单（逗号分隔）；空 = 不授权任何群，所有群被退出 |
| | `BOT_LEAVE_OTHER_CHATS` | `true` | 被拉进非白名单群立即 `leaveChat` |
| | `BOT_LEAVE_NOTICE` | `true` | 退出前先发一句说明 |
| | `BOT_ADMIN_USERS` | 空 | 管理员 uid；消息永不判定，可用 `/status`、`/check` |
| | `BOT_DRY_RUN` | `false` | `true` 只判定+打日志，不处置 |
| 行为 | `BOT_ACTION` | `kick` | `kick` 封后即解 / `ban` 永久 |
| | `BOT_IGNORE_ADMINS` | `true` | 跳过群主/管理员 |
| | `BOT_DELETE_JOIN_MSG` | `false` | 删除广告号入群系统消息 |
| | `BOT_LOG_CHAT` | `0` | 处置通知目标 chat id（0=关） |
| | `BOT_MSG_TTL` | `60s` | bot 聊天消息（命令回复/退群提示/送客公告）延迟自动删除；`0` = 永不删（amend8，吸收 `BOT_CMD_AUTODELETE_SEC`/`BOT_ANNOUNCE_TTL`）。**通知频道推送不受影响，永久保留** |
| 存储 | `BOT_STORE` | `memory` | `memory` / `redis` / `file` / `auto` |
| | `BOT_REDIS_URL` | 空 | redis 后端连接串 |
| | `BOT_REDIS_INSECURE_TLS` | `false` | 跳过 TLS 校验（仅开发） |
| | `BOT_DB_PATH` | `data/adbot.db` | file 后端路径（本轮回落内存） |
| | `BOT_DB_FSYNC` | `false` | file 后端是否 fsync |
| 保留 | `BOT_RETENTION` | `72h` | 全局保留时长（0/负 = 永久） |
| | `BOT_RETENTION_SUSPECT` / `_SIGNATURE` | 空 | 分类覆盖，空 = 跟随全局 |
| | `BOT_RETENTION_SLIDING` | `true` | 滑动过期 |
| | `BOT_SWEEP_INTERVAL` | `10m` | janitor 清扫间隔 |
| | `BOT_MAX_SUSPECTS` / `BOT_MAX_SIGNATURES` | `50000` | 容量护栏 |
| AI | `BOT_AI_BASE_URL` | `https://api.your-gateway.invalid/v1` | **必填**：OpenAI 兼容网关（留空/占位符会走本地兜底） |
| | `BOT_AI_API_KEY` | 空 | 判定 key；缺则跳过 AI 并 WARN |
| | `BOT_AI_MODELS` | 三模型逗号链 | 模型回退链 |
| | `BOT_AI_MODEL` | `mimo-v2.6-pro-ultraspeed-studio` | 兼容旧单模型，`MODELS` 优先 |
| | `BOT_AI_TIMEOUT` | `90s` | 单次判定超时（amend6 放宽；异步池下不再阻塞别人） |
| | `BOT_AI_JUDGE_TIMEOUT` | `150s` | 单条判定整条链总预算（超出转重试队列） |
| | `BOT_AI_REASONING_EFFORT` | `none` | 关思考（网关校验过；绝不发 `max_tokens`） |
| | `BOT_AI_PROMPT_FILE` | 空 | 覆盖内嵌提示词（调优不发版） |
| | `BOT_AI_RETRY_INTERVAL` / `_MAX_ATTEMPTS` / `_QUEUE` | `10s` / `5` / `500` | 固定间隔重试（不退避、不熔断） |
| | `BOT_AI_MIN_CONFIDENCE` | `0.85` | 置信度阈值，不足触发复核（管「何时复核」） |
| | `BOT_AI_MIN_PUNISH_SCORE` | `85` | 处置门限：AI 判广告但分数 < 85 放过不踢（管「多高分才踢」；复核投 ad 时取判广告复核路平均分；0 = 关闭；签名/裸号/入群验证/防刷/兜底/上游敏感不受影响） |
| | `BOT_AI_VERIFY_VOTES` / `_MODELS` / `_MAJORITY` / `_TIMEOUT` | `3` / `mimo-v2.6-flash` / `2` / `45s` | 并发复核（单路超时，失败路弃权） |
| | `BOT_AI_FALLBACK_BASE_URL` / `_API_KEY` / `_MODELS` | 空 | 备用 provider（可选） |
| | `BOT_SIMHASH_DISTANCE` | `3` | 相似度匹配（防改字绕过） |
| | `BOT_LOCAL_RULES_ENABLED` / `_THRESHOLD` / `_KEYWORDS` | `true` / `3` / 空 | 本地规则兜底（5 次重试用尽后） |
| 运行 | `BOT_POLL_TIMEOUT` / `BOT_WORKERS` | `30` / `4` | 轮询与并发 |
| 判定池 | `BOT_JUDGE_ASYNC` | `true` | 判定异步池总开关（false = 旧行内同步，一行回滚） |
| | `BOT_JUDGE_QUEUE` / `BOT_AI_CONCURRENCY` | `500` / `8` | 有界提交队列 / 判定并发（同 uid 串行） |
| | `BOT_JUDGE_DRAIN` / `BOT_UPDATE_WORKERS` | `10s` / `16` | 优雅关闭排空预算 / 库层 update worker 数（1..128） |
| 公告 | `BOT_ANNOUNCE_KICK` | `true` | 处置成功后在群内发 AI 生成的可爱送客短公告（异步不阻塞处置链；AI 失败走本地模板池；发送后按 `BOT_MSG_TTL` 自动删除） |
| | `BOT_ANNOUNCE_TIMEOUT` | `8s` | 公告生成独立短超时（clamp 1s~20s，不重试） |
| 裸号 | `BOT_JOIN_NAKED_PREMIUM` | `true` | 入群「裸号」必杀：Premium 会员 + 英文名 + 无简介 + 无头像，四条件 AND 全中直接处置（不经 AI；`is_premium` 入群时补一次 `getChatMember` 再核） |
| 入群验证 | `BOT_JOIN_VERIFY` | `true` | 300 秒贴纸门，**顺序：过了简介判定（结论非广告）才发欢迎**。判为广告 → 直接处置，**不发欢迎、不登记验证**；判定失败 → 走重试队列（重试期间**不欢迎、不登记**），等最终结论（含本地规则兜底）落地：非广告才欢迎+起倒计时 / 广告则处置。无简介者直接欢迎+起倒计时。须在时限内发**贴纸**（只认 `message.sticker`；纯 emoji 文本/文字/图片一律不算，**无放宽开关**）才恢复原流程；期间该用户消息**绝不送 AI**；**到期处置前二次 `getChatMember` 确认**：人已不在 → 只清理不处置，被限制发言 → 只清理不处置，查询失败重试 2 次后仍处置；我们自己处置过或别人踢的 → 立即撒手、**绝不二次处置**。pending 只存内存，重启丢失 |
| | `BOT_JOIN_VERIFY_TIMEOUT` | `300s` | 验证时限（clamp 1s~24h） |
| | `BOT_JOIN_VERIFY_SCAN` | `5s` | 超时扫描间隔（单 ticker 随进程关闭；clamp 100ms~1m） |
| | `BOT_JOIN_VERIFY_ACTION` | `ban` | 超时处置：`ban`（默认，**真封禁**）/ `kick` |
| | `BOT_JOIN_VERIFY_MSG` | 固定文案 | 欢迎消息（`%s` = @用户名或名字）；**不走 `BOT_MSG_TTL` 60s 自删**，验证结束即删 |
| | `BOT_JOIN_VERIFY_MAX_PENDING` | `5000` | 待验证名单上限；超限 WARN 并走原流程 |
| 评论简介 | `BOT_COMMENT_BIO_SCREEN` | `true` | amend12：群里回复**关联频道帖子**（reply 目标 `is_automatic_forward=true`）的消息触发评论者简介检查——简介带链接或 `@` → 送 AI 审简介（`mode=bio` 复用入群提示词）；判广告 → 标准处置链（punish + 通知频道 + 群内公告 + 写 bio 签名库）；判非广告 → 零处置、不消费观察名额；简介空/无链接无 `@`/非评论消息 → 原流程零变化 |
| | `BOT_COMMENT_BIO_FLASH_TTL` | `30m` | **防刷窗口**（不是限流）：审核未过（判广告）的简介进缓存；窗口内遇到**重复**（normalized sha1 精确匹配）简介的直接踢、**零 AI 调用**；窗口过期重新送审（clamp：0=不自动过期 或 ≤24h） |
| | `BOT_COMMENT_BIO_FLASH_MAX` | `5000` | 防刷缓存容量；超限最旧淘汰。只存内存，重启丢失 |
| | `BOT_LOG_LEVEL` / `BOT_LOG_FORMAT` | `info` / `text` | 日志 |
| | `BOT_ADMIN_TOKEN` | 空 | 非空开放 `/admin/*` |
| | `BOT_VERSION` | `dev` | 构建注入 |

> 所有变量走环境变量，代码不硬编码真实凭据。启动日志与 `-selftest` 输出会**脱敏**
> （token/password 只显示前缀或不显示）。

## HTTP 接口

- `/` → `200 adbot ok`
- `/healthz` / `/readyz` → 存活 / 就绪（就绪检查存储 `Ping`）
- `/statusz` → JSON 状态快照：版本、uptime、轮询/处置/删除计数、AI 调用与失败、
  各类记录条数、`store_backend`、`retention*`、`sweep_interval`、`dry_run`、`started_at`，
  以及 amend3 项：`ai_models` / `ai_model_stats`（逐模型 calls/oks）、`ai_fallbacks`、
  `ai_verify_rounds` / `ai_verify_flipped`、`local_fallback_count`、
  `simhash_hits`、`pending_queue`、`prompt_version`、`last_sweep` / `next_sweep_in`
  （janitor 跑过才出现），以及 amend8 项：`msg_ttl`（当前配置值）、
  `msg_ttl_scheduled_total`、`msg_ttl_deleted_total`、`msg_ttl_error_total`，
  amend9 项：`join_rule_enabled` / `join_rule_hits_total`，amend10 项：
  `join_verify_enabled` / `join_verify_pending` / `join_verify_passed_total` /
  `join_verify_timeout_total`，amend12 项：`comment_bio_screen_enabled` /
  `comment_bio_triggered_total` / `comment_bio_flash_hits_total` /
  `comment_bio_flash_cached` / `comment_bio_flash_ttl`
- `/admin/signatures` → 需要 `BOT_ADMIN_TOKEN`（`Authorization: Bearer`），
  `GET`/`POST`/`DELETE` 维护 bio/msg 签名库

## belmo 部署

belmo 用 **Nixpacks** 构建，且部署引擎固定扫描仓库的 **`app/` 子目录**——所以 Go 源码放在
`app/`，仓库根只留 `Dockerfile` / `docker-compose.yml` / `README.md` 供本地与容器使用。

**平台参数（逐项对应 belmo 表单）：**

| 项 | 值 |
|---|---|
| Build Pack | **Nixpacks** |
| Base Directory | **`app`** ← 关键，留空会检测不到 Go 项目 |
| Build Command | **留空**（`app/nixpacks.toml` 已固定 `go build -o app .`） |
| Start Command | **留空**（`app/nixpacks.toml` 已固定 `./app`） |
| Port | **3000** |
| Health Check | `/healthz`（就绪探针用 `/readyz`） |

**环境变量（按需填）：**

| 变量 | 必填 | 填什么 |
|---|---|---|
| `BOT_TOKEN` | ✅ | BotFather 发的 bot token |
| `BOT_CHATS` | ✅ | 授权群 id（`-100` 开头），多个逗号分隔；留空等于不保护任何群 |
| `BOT_AI_BASE_URL` | ✅ | 你的 OpenAI 兼容网关地址，形如 `https://your-gateway.example.com/v1` |
| `BOT_AI_API_KEY` | ✅ | 上面网关的 key |
| `BOT_ADMIN_USERS` | 建议 | 你的 Telegram user id，可用 `/status`、`/check` |
| `BOT_LOG_CHAT` | 可选 | 处置/报错通知的频道 id，`0` = 关闭；启动时会自检可达性与发帖权限 |
| `BOT_DRY_RUN` | 建议 | 首次部署填 `true`（只判定不处置），观察日志确认无误后改 `false` |
| `BOT_ACTION` | 可选 | `kick`（默认，封后即解=踢出）/ `ban`（永久） |

其余变量都有合理默认值，完整清单见 `app/.env.example`（75 个变量与代码契约一一对应）。

**部署后自检：**

- `GET /statusz` → 看 `store_backend`、`prompt_version`、`ai_models`、`notify.target_ok`、`dry_run`
- 群里发一句广告文本（或用管理员的 `/check <文本>`）验证 AI 链路
- `BOT_DRY_RUN=false` 后，处置会推到 `BOT_LOG_CHAT`（⏱️/💬/👤/✅ 四行模板）

**注意：`BOT_STORE` 保持默认 `memory`** —— belmo 容器文件系统只读、平台 Redis 外网不通，
内存模式是唯一可用后端。重启会丢观察名单/签名库/统计；**已封禁的号不会因重启恢复**
（封禁在 Telegram 服务端生效）。

## 自检 CLI（连通性验收）

```bash
# 仓库根，先建 .env（从 app/.env.example 复制并填值）
cd app && go run . -selftest
# 或已构建产物
./adbot -selftest
```

依次检查：配置解析（脱敏摘要 + **生效 TTL 配置** + `prompt_version`）→ 存储（后端 + 各表
当前条数）→ AI（发「专业办理各种证件 加V信 xxx」判定并打印 JSON 与耗时）→ Telegram
（`getMe`）。每项输出 `[PASS]/[FAIL]`，任一项失败退出码非 0。

另有一条**显式 opt-in 的外呼**子命令（否则一切测试离线）：

```bash
cd app && go run . -prompteval   # 真实网关跑 29 条标注样本，输出 FN/FP 统计
```

## 本地运行

```bash
# 内存存储（默认），适合开发
cd app && cp .env.example .env   # 填入 BOT_TOKEN 等再启动
set -a && . ./.env && set +a && go run .

# docker（仓库根，从 .env 读取）
docker compose up --build

# 测试（全程离线，无需 Redis/Telegram/网络）
cd app && go build ./... && go vet ./... && go test ./... -race -count=1
```

## 部署前给 Bot 的权限要求

- 把 Bot 加进目标群并设为**管理员**，务必勾选 **删除消息（Delete messages）** 权限，
  否则无法删除他人消息；`banChatMember` 也要求 Bot 有管理员身份。
- 轮询由库 `WithAllowedUpdates`（`message` + `chat_member` + `my_chat_member`）驱动。

## 已知限制

- **入群验证 pending 只存内存（amend10）**：`BOT_STORE=memory` 下进程重启会丢失
  待验证名单——重启后那批已进群但未发贴纸的用户**不会再被超时处置**（观察名单
  也一并丢失，靠签名库兜底）；残留的欢迎消息不做补偿删除（超 48h 也删不掉），
  属已知代价。验证期间该用户的消息不送 AI、不判签名。
- **bot 聊天消息 TTL（amend8）**：bot 发进群/私聊的消息按 `BOT_MSG_TTL`（默认 60s，
  `0`=永不删）自动删除——命令回复、退群提示、送客公告统一走它；**通知频道
  （`BOT_LOG_CHAT`）的推送永久保留，绝不删**。删除任务只存内存：**进程重启会丢失
  尚未到期的删除任务，那几条消息不会被删**（已知代价，不做兜底）。删除失败
  （已被人手删/超 48h 返回 400）只 WARN。
- **file 后端明确不实现**：内存模式是最终选择（belmo 文件系统疑似只读）；配置 `file`
  会打 WARN 并回落内存，**重启丢观察名单/签名库/统计**属预期行为（封禁在 Telegram
  服务端生效不受影响）。
- **>48h 的消息不可删**：Bot API 限制，删除失败只记 warn，不中断处置流程。
- **bio 仅 `getChat` 可取**：单聊/用户 chat 才返回 `bio`，取不到时留空继续（不算错误）。
- **只删触发消息**：不再记录观察期消息 id、不做批量删除；入群前的历史消息无法删除。
- **AI 判定存在误封风险**：解析失败/超时一律 fail-safe（不处置）；AI 判定的广告会回写签名库，
  误判也会沉淀为签名，需用 `/admin/signatures` 人工维护修正。

## 目录

```
.
├── Dockerfile
├── docker-compose.yml
├── README.md
├── .gitignore              # 含 .env，绝不入库真实凭据
├── .dockerignore           # 排除 .env / .git / 日志 / 构建产物
└── app/
    ├── go.mod / go.sum     # module adbot；唯一依赖 github.com/go-telegram/bot v1.27.0
    ├── nixpacks.toml       # belmo 构建固定
    ├── .env.example        # 环境变量契约（§6）
    ├── main.go             # 入口：配置、存储、AI、HTTP、janitor、优雅退出
    ├── janitor.go          # TTL/capacity 清扫 goroutine
    ├── poller.go           # 库驱动 long-poll 的文档锚点
    ├── selftest.go         # -selftest 连通性验收
    ├── prompteval.go       # -prompteval 标注集评估（唯一外呼路径）
    ├── testdata/           # prompt_eval.jsonl（29 条标注样本）
    └── internal/
        ├── ai/             # go-openai 判定：回退链/投票/多模态/prompts(judge_v2)
        ├── bot/            # 决策核心：命令/入群/发言/AI/签名命中/处置/白名单
        ├── config/         # 环境变量加载、脱敏、未识别变量告警
        ├── logx/           # 结构化日志（text/json）
        ├── localrules/    # 本地规则兜底引擎（AI 不可达时的终局决策）
        ├── normalize/      # 归一化 + sha1 签名 + SimHash
        ├── retryq/         # 10s×5 固定重试队列 + 后台 worker
        ├── server/         # HTTP :3000（health/ready/status/admin）
        ├── store/          # Store 接口 + Memory/Redis + SimHash 索引 + Sweeper
        └── telegram/       # go-telegram/bot 薄封装（领域类型 + 错误映射）
```