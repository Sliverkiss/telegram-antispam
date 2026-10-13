# adbot — Telegram 反广告 Bot

一个 **Go stdlib-only**（零第三方依赖，`go.sum` 为空）的 Telegram 反灰黑产/反广告导流 Bot。
入群时筛查新成员简介（bio），群内发言按签名库 + AI 双通道判定，命中即封禁/踢出，
并维护「观察名单」与「bio / 消息」两级 sha1 签名库，逐步积累可直接命中、无需 AI 的本地知识。

- 手写 Telegram Bot API 客户端（`net/http`，long polling）
- 手写 RESP 协议 Redis 客户端（改编自内部参考实现，支持 TLS / `rediss://` / AUTH）
- OpenAI 兼容 AI 判定（默认指向 `mimo …ultraspeed` 网关），可插拔
- `net/http` 服务监听 **:3000**，供 belmo 健康检查
- `-selftest` 一键连通性验收

## 架构

```
                        ┌───────────────────────────────┐
 Telegram Bot API ─────▶│  poller (getUpdates long poll) │
  (net/http)            └──────────────┬────────────────┘
                                      │ update
                                      ▼
                        ┌───────────────────────────────┐
                        │          bot (决策逻辑)         │
                        │  A. 入群 → bio 签名库 → AI      │
                        │  B. 发言 → msg 签名库 → AI      │
                        └──────┬──────────────┬─────────┘
                               │              │
                    ┌──────────▼──┐     ┌─────▼──────────┐
                    │  internal/ai │     │ internal/store │
                    │ (OpenAI 兼容) │     │ Memory │ Redis │
                    └─────────────┘     └─────┬──────────┘
                                              │ RESP (手写)
                                              ▼
                                           Redis
                        ┌───────────────────────────────┐
                        │  HTTP :3000                    │
                        │  / /healthz /readyz /statusz   │
                        │  /admin/signatures (带 token)  │
                        └───────────────────────────────┘
```

## Redis key schema（前缀 `adbot:`）

| Key | 类型 | 用途 |
|---|---|---|
| `adbot:suspect` | SET | 观察名单 uid 集合 |
| `adbot:suspect:<uid>` | HASH(+TTL) | uid/username/first_name/joined_at/chat_id/bio |
| `adbot:biosig` | SET | bio 签名 sha1 集合 |
| `adbot:biosig:<sha1>` | STRING | 归一化原文 |
| `adbot:msgsig` | SET | 消息签名 sha1 集合 |
| `adbot:msgsig:<sha1>` | STRING | 归一化原文 |
| `adbot:track:<chatid>:<uid>` | LIST | 该用户已知消息 id（LPUSH，LTRIM 封顶） |
| `adbot:puni:<uid>` | STRING | 处罚记录（JSON） |
| `adbot:stats` | HASH | `punished` 计数 |

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `BOT_TOKEN` | — | **必填**。BotFather 发放的 token |
| `BOT_ADDR` | `:3000` | HTTP 监听地址（belmo 靠它判定健康） |
| `BOT_ACTION` | `ban` | `ban` 封禁 / `kick` 封禁后立即解封（可再次入群） |
| `BOT_IGNORE_ADMINS` | `true` | 跳过群主/管理员（防误杀自己人） |
| `BOT_DELETE_JOIN_MSG` | `false` | 入群签名命中时删除「XX 入群」系统消息 |
| `BOT_LOG_CHAT` | 空 | 处置通知目标 chat_id（空=关闭） |
| `BOT_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `BOT_LOG_FORMAT` | `text` | `text`/`json` |
| `BOT_REDIS_URL` | 空 | `redis://` 或 `rediss://`（TLS）；空=内存实现 |
| `BOT_REDIS_INSECURE_TLS` | `false` | 跳过证书校验（仅开发） |
| `BOT_SUSPECT_TTL` | 空 | 观察名单过期（如 `24h`）；空=永不过期 |
| `BOT_AI_BASE_URL` | `https://mimo.xn--ug8h.eu.org/v1` | OpenAI 兼容网关 |
| `BOT_AI_API_KEY` | — | 网关 API Key |
| `BOT_AI_MODEL` | `mimo-v2.6-pro-ultraspeed-studio` | 模型名 |
| `BOT_AI_TIMEOUT` | `30s` | 单次 AI 超时 |
| `BOT_ADMIN_TOKEN` | 空 | 非空时开启 `/admin/*` 并校验 `Authorization: Bearer`；空=关闭 |
| `BOT_MAX_TRACKED_MSGS` | `200` | 每 chat+uid 记录的消息 id 上限 |
| `BOT_POLL_TIMEOUT` | `25` | getUpdates long-poll 秒数 |

> 所有变量都走环境变量；代码中不硬编码任何真实凭据。启动日志 / `-selftest` 输出会**脱敏**
> （token/password 只显示前缀）。

## belmo 部署

1. 导入仓库到 belmo，build pack 选 **Nixpacks**。
2. **Base Directory: `app`**（关键——留空则检测不到 Go 项目）。
3. Build / Start 命令**留空**，`app/nixpacks.toml` 已固定（`go build -o app .` / `./app`）。
4. **端口 `3000`**（`BOT_ADDR` 默认 `:3000`）。
5. 配置环境变量（至少 `BOT_TOKEN`、`BOT_AI_API_KEY`；生产建议再配 `BOT_REDIS_URL` 与 `BOT_ADMIN_TOKEN`）。

## 自检 CLI（连通性验收）

```bash
./app -selftest
```

依次检查：配置解析（脱敏摘要）→ Redis（PING + SET/GET/DEL 往返）→ AI（发一条
「专业办理各种证件 加V信 xxx」判定并打印 JSON 与耗时）→ Telegram（`getMe`）。
每项输出 `[PASS]/[FAIL]`，任一项失败时退出码非 0。

```bash
BOT_TOKEN=xxx BOT_AI_API_KEY=yyy BOT_REDIS_URL=rediss://:pw@host:6379 ./app -selftest
```

## 本地运行

```bash
# 直接跑（内存存储，适合开发）
cd app && BOT_TOKEN=xxx BOT_AI_API_KEY=yyy go run . 

# docker（仓库根）
docker compose up --build

# 测试（全程离线，无需 Redis/Telegram/网络）
cd app && go build ./... && go vet ./... && go test ./... -count=1
```

## 部署前给 Bot 的权限要求

- 把 Bot 加进目标群并设为**管理员**，务必勾选 **删除消息（Delete messages）** 权限，
  否则无法删除他人消息；`banChatMember` 也要求 Bot 有管理员身份。
- 建议开启 `/setprivacy` 设为 *Disabled*？——实际由 `getUpdates` 的
  `allowed_updates`（`message` + `chat_member`）驱动，`my_chat_member` 仅用于忽略自身状态变更。

## 已知限制

- **>48h 的消息不可删**：Bot API 限制，删除失败只记 warn，不中断处置流程。
- **bio 仅 `getChat` 可取**：单聊/用户 chat 才返回 `bio`，取不到时留空继续（不算错误）。
- **Bot API 不能按用户批量删历史消息**：因此靠「观察期消息 id 记录」
  （`adbot:track:<chat>:<uid>`，上限 `BOT_MAX_TRACKED_MSGS`）删除已知消息——
  入群前的历史消息不在记录内，无法删除。
- **AI 判定存在误封风险**：解析失败/超时一律 fail-safe（不处置）；AI 判定的广告
  会回写签名库，但误判也会沉淀为签名，需用 `/admin/signatures` 人工维护修正。

## 目录

```
.
├── Dockerfile
├── docker-compose.yml
├── README.md
└── app/
    ├── go.mod                 # module adbot，无任何 require（stdlib only）
    ├── nixpacks.toml
    ├── .env.example
    ├── main.go                # 入口：配置、存储、AI、poller、HTTP、优雅退出
    ├── poller.go              # getUpdates long polling + 409/网络退避
    ├── selftest.go            # ./app -selftest 连通性验收
    └── internal/
        ├── ai/                # OpenAI 兼容判定（围栏剥离、脏输出、重试、fail-safe）
        ├── bot/               # 决策核心：入群/发言/AI/签名命中/处置
        ├── config/            # 环境变量加载与脱敏
        ├── logx/              # 结构化日志（text/json）
        ├── normalize/         # 归一化 + sha1 签名
        ├── server/            # HTTP :3000（health/ready/status/admin）
        ├── store/             # Store 接口 + Memory + Redis（手写 RESP）
        └── telegram/          # 手写 Bot API 客户端
```