# minimaxcode2api

把 **MiniMax Code 桌面客户端的自有 LLM 通道** 包成标准 API：一套二进制同时对外提供
**OpenAI 兼容**（`/v1/chat/completions`）和 **Anthropic 原生**（`/v1/messages`）两个入口，
自带账号池、自动刷新、用量统计和一个无框架 Web 管理台。

> 定位：个人/小团队自用网关。不做多租户计费，不做模型路由编排——那会把一个薄网关做成半成品平台。

```
客户端(SDK/Claude Code/Cursor…)  ──►  minimaxcode2api  ──►  agent.minimax.cn
        OpenAI 或 Anthropic 协议          选号/刷新/记账           Anthropic Messages
```

## 它凭什么能跑（逆向结论）

这些是静态审计 `app.asar` + 真实请求验证得到的协议事实，不是猜的：

| 项 | 结论 |
|---|---|
| 上游地址 | `POST https://agent.minimax.cn/mavis/api/v1/llm/v1/messages` |
| 协议 | **Anthropic Messages**（不是 OpenAI，也不需要中转转换） |
| 鉴权 | 只需 `Authorization: Bearer mmoat_…` + `anthropic-version: 2023-06-01` |
| 签名 | **不需要**。`x-signature = md5(timestamp + 固定盐 + body)` 只作用于 `/matrix`、`/minimax-cloud` 等业务接口，LLM 通道不校验 |
| 客户端指纹 | 无 `device_id`/`yy`/`params.client` 之类要求，UA 对齐官方即可 |
| 证书固定 | 主进程无 `setCertificateVerifyProc`/pinning，可直接抓包 |
| 模型 | `MiniMax-M3.1-Flash`（思考**强制开启**，传 `thinking` 会 400）、`MiniMax-M3`（可开关） |
| 上下文 | 两者都是 `context 512000` / `output 128000` |
| OAuth | Device Authorization Grant + PKCE(S256)，`client_id=mcode-public`、`scope=agent.default`、`audience=agent-backend` |
| 凭据落盘 | `~/.minimax/auth/prod/cn/mcode-public/auth.json`（`accessToken` / `refreshToken` / `expiresAtMs`） |

## 快速开始

### 二进制

```bash
go build -o minimaxcode2api ./cmd/server
./minimaxcode2api -config ./config.json
```

首次启动会自动生成 `config.json`（含随机 `api_key`，形如 `sk-mmx-…`）并打印：

```
minimaxcode2api 0.1.0 启动中
  监听      : :7864
  上游      : https://agent.minimax.cn/mavis/api/v1/llm/v1
  账号      : 0 个（启用 0）
  管理台    : http://127.0.0.1:7864/panel/
  数据目录  : ./data
  提示      : 还没有账号，打开管理台用「设备授权」添加
```

打开管理台，用 `api_key` 登录 → **添加账号 → 设备授权** → 浏览器里点「授权」，账号自动入池。

### Docker

```bash
cp .env.example .env      # 可选：固定 API_KEY
docker compose up -d --build
# 面板 http://127.0.0.1:7864/panel/
```

数据全部落在 `./data`（`config.json` / `accounts.json` / `usage.json`），备份这个目录就够了。

## 账号怎么来（重要）

有两种方式，**优先用设备授权**：

1. **设备授权（推荐）**：面板里点「生成授权码」，在浏览器打开验证页确认。
   拿到的是网关**独立**的一份授权，和桌面客户端互不影响。
2. **导入 token**：粘贴 `~/.minimax/auth/.../auth.json` 里的 `access_token`
   （`mmoat_` 开头，约 12 小时有效）或 `refresh_token`（`mmort_` 开头）。

⚠️ **不要把桌面客户端正在用的 `refresh_token` 导进来**，原因不是洁癖：

> 上游对 `refresh_token` 做了单次使用 + reuse detection。同一个 RT 被客户端和网关
> 同时使用，会让整个 token family 被吊销——客户端本地凭据会被清空、双方一起 401。
> 这个坑本项目实测踩过一次（导入后几分钟，两边同时失效）。

因此代码里也做了对应防护：**只要请求里给了 `access_token`，就直接用它，绝不去轮换
`refresh_token`**；只有"仅给 refresh_token"时才不得不刷一次。

## 接口

| 路径 | 说明 |
|---|---|
| `POST /v1/chat/completions` | OpenAI 兼容（含 `stream` / `tools` / `tool_choice` / `reasoning_effort` / 图片输入） |
| `POST /v1/messages` | Anthropic 原生直通（`thinking`、`cache_control`、`tool_use` 全部原样保留） |
| `POST /v1/messages/count_tokens` | 本地估算（上游该端点返回 405） |
| `GET /v1/models` / `GET /v1/models/{id}` | 模型列表与详情 |
| `GET /healthz` | 健康检查（免鉴权） |
| `GET /status` | 运行状态（鉴权） |
| `GET /panel/` | Web 管理台 |

鉴权：`Authorization: Bearer <api_key>` 或 `x-api-key: <api_key>`。

```bash
# OpenAI 兼容
curl http://127.0.0.1:7864/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"MiniMax-M3.1-Flash","messages":[{"role":"user","content":"你好"}],"max_tokens":256}'

# Anthropic 原生（Claude Code 直接指过来即可）
curl http://127.0.0.1:7864/v1/messages \
  -H "x-api-key: $KEY" -H "anthropic-version: 2023-06-01" -H "Content-Type: application/json" \
  -d '{"model":"M3","max_tokens":256,"messages":[{"role":"user","content":"你好"}]}'
```

接入 Claude Code：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:7864
export ANTHROPIC_AUTH_TOKEN=sk-mmx-…
export ANTHROPIC_MODEL=MiniMax-M3.1-Flash
```

模型别名：`M3` / `minimax-m3` → `MiniMax-M3`，`M3.1-Flash` / `MiniMax-M3.1` / `m3.1-flash` → `MiniMax-M3.1-Flash`。

### 思考（thinking）怎么处理的

这是唯一需要"猜"的地方，规则写在 `internal/convert/decideThinking`：

| 模型 | 行为 |
|---|---|
| `MiniMax-M3.1-Flash` | `forced_adaptive`——**任何情况都不传 `thinking`**。传 `type=disabled` 会被上游 400。 |
| `MiniMax-M3` | 按 `thinking_policy`（auto/on/off）+ `reasoning_effort` 决定开关；预算是 `budget_tokens < max_tokens`，`max_tokens < 1536` 时放不下最小预算，降级为关闭思考（而不是发一个必然 400 的组合）。 |
| 未知模型 | 不传，交给服务端默认。 |

## 配置

`config.json` 全部字段可热改（面板「设置」只开放白名单字段，改完立即生效）。

| 字段 | 说明 |
|---|---|
| `listen` | 监听地址，默认 `:7864` |
| `api_key` | 对外 `/v1/*` 密钥，同时是面板登录口令 |
| `admin_password` | 面板单独口令，留空则回落到 `api_key` |
| `data_dir` | 账号/用量落盘目录（`accounts.json` 权限 0600） |
| `region` | `cn`（国内版）/ `intl`（国际版），整组切换上游与 OAuth 域名 |
| `upstream.base_url` | 到 `/v1` 为止，代码里再拼 `/messages` |
| `pool.strategy` | `weighted`（在途少+最久未用优先，top-3 随机）或 `round_robin` |
| `pool.max_in_flight` / `max_in_flight_per_account` | 总闸门 / 单账号在途上限 |
| `pool.cooldown_429` / `cooldown_429_max` | 429 指数退避（封顶） |
| `pool.cooldown_5xx` | 5xx 冷却；401/403 取它的 2 倍 |
| `pool.breaker_failures` / `breaker_cooldown` | 连续失败熔断 |
| `pool.refresh_before` | 提前多久刷新 token（默认 10 分钟） |
| `defaults.model` / `max_tokens` / `thinking_policy` | 未显式指定时的缺省值 |
| `models.expose` / `aliases` / `hidden` | 对外模型清单与别名 |

环境变量（优先级高于文件，容器部署用）：

| 变量 | 说明 |
|---|---|
| `MMX2A_API_KEY` | 覆盖 `api_key` |
| `MMX2A_ADMIN_PASSWORD` | 覆盖 `admin_password` |
| `MMX2A_LISTEN` | 覆盖 `listen` |
| `MMX2A_DATA_DIR` | 覆盖 `data_dir` |
| `MMX2A_REGION` | 覆盖 `region`（`cn` / `intl`） |

## 失败与重试

一次请求最多换 3 个账号：

- **401/403**：先对**同账号**强制刷新 token 再打一次（token 过期是最常见原因），仍失败才换号；
- **429**：按指数退避冷却该账号后换号；
- **5xx / 网络错误**：短冷却后换号；
- **400/404/422**：请求本身有问题，换号也一样失败，**直接透出**上游状态与消息。

刷新是**账号级串行**的，且拿到锁后会重读账号——避免并发请求用同一个 `refresh_token`
二次轮换，把自己刷成 `invalid_grant`。

## 管理台

- **概览**：账号健康度、今日/累计请求与 token、缓存命中、14 天趋势
- **账号**：状态、掩码 token、过期倒计时、用量；测试 / 停用 / 改名 / 恢复 / 删除
- **模型**：对外清单、别名、能力（上下文、输出上限、思考特性）
- **请求日志**：最近 N 条（时间、模型、状态、耗时、token、账号、来源 IP/UA、换号次数）
- **设置**：调度策略、超时、默认模型、面板口令等白名单热改

界面无框架、无外部字体/CDN，`script-src 'self'` 严格 CSP，所有文本走 `textContent`。

## 媒体能力（端点已摸清，本版未代理）

文本走 `/mavis/api/v1/llm/v1/messages`，图片/视频/语音走的是**另一个网关**
`/mavis/api/v1/mcp/<tool>` —— 桌面端在 `DesktopMatrixClient` 里把
`/matrix/api/v1/mcp/*` 重写成 `/mavis/api/v1/mcp/*` 再发。鉴权同样只要
`Authorization: Bearer`，**不需要 `x-signature`**（签名只用于少数旧接口）。

全量 17 个工具（来自 `@mavis/agent-tools/.../matrix-tools/tool-defs.js`）：

| 端点 | 用途 | 验证程度 |
|---|---|---|
| `image_synthesize` | 文生图 / 图生图，`requests[]` 需 `prompt` + `output_file`，可带 `input_urls` / `aspect_ratio` / `resolution` | 鉴权通过（空参数被正确判 400） |
| `submit_video_generation` / `query_video_generation` | 提交 + 轮询视频任务 | 200，缺 `task_id` 时返回明确提示 |
| `gen_videos` / `batch_text_to_video` / `batch_image_to_video` | 视频批量生成 | 未实测 |
| `get_voice_list` | 音色列表 | ✅ 返回真实音色表 |
| `synthesize_speech` / `batch_synthesize_speech` / `batch_text_to_audio` / `batch_text_to_music` | 语音合成 / 音乐 | 未实测 |
| `transcribe_audio`（路径 `listen_audio`）/ `audios_understand` | 转写 / 音频理解 | 未实测 |
| `images_understand` / `videos_understand` | 图片 / 视频理解 | 未实测 |
| `web_search` / `images_search_and_download` / `image_reverse_search` | 搜索类 | 未实测 |

画布另有 `remove_image_background`、`enhance_image` 两个图像处理端点。

> **注意**：本节列的是"上游能不能调通"，不是"网关已经支持"。当前版本对外只暴露
> 文本接口；要接媒体，新增 `/v1/images/generations`、`/v1/video/generations`
> 之类的路由即可，凭据复用同一个账号池。

## 国内版 / 国际版

客户端二进制里是**一套双区域映射**（`dist/main/config/constants.js`）：

| | 国内版 `region=cn` | 国际版 `region=intl` |
|---|---|---|
| Agent 网关 | `https://agent.minimax.cn` | `https://agent.minimax.io` |
| 账号服务 | `https://account.minimax.cn` | `https://account.minimax.io` |
| 媒体网关 | 同一 Agent 网关下的 `/mavis/api/v1/mcp/*` | 同构 |

改 `config.json` 的 `region`（或 `MMX2A_REGION=intl`）即整组切换。改写规则是
**只在字段为空、或仍等于另一区域预设值时才覆盖**：所以从 cn 切到 intl 能一把切过去，
而你显式写死的自定义域名不会被抹掉。

⚠️ 国际版四个域名取自客户端的 en 分支与主进程 CORS 白名单，域名本身可靠，但
**没有国际版账号做过真实调用**；国内版四个域名均已实测通过。

## 边界（明确不做）

- 不做 `/v1/responses`、embeddings、rerank；
- **这一版只代理文本**：图片/视频/语音的端点在另一个网关（见下节），已摸清但未接进来；
- 不做计费与配额，用量只统计不扣费；
- 不做多用户/RBAC，面板是单人运维工具；
- 请求日志只在内存环形缓冲里，重启即清空（长期审计请接外部日志）；
- `count_tokens` 是按字符数的粗略估算，别拿它做精确计费。

## 目录结构

```
cmd/server/          入口：装配 + 优雅退出
internal/config/     配置加载/默认值/热更新 Holder
internal/store/      账号持久化（原子写，0600）
internal/oauth/      设备授权（PKCE）、刷新、撤销
internal/upstream/   上游客户端（Anthropic Messages）
internal/convert/    OpenAI ⇄ Anthropic 协议转换（请求/响应/流式）
internal/pool/       账号池调度、冷却、熔断
internal/server/     HTTP 路由、SSE、重试与换号
internal/panel/      管理台 API + 内嵌 UI（go:embed）
internal/usage/      按天/模型/账号用量聚合
internal/reqlog/     请求日志环形缓冲
```

## 测试

```bash
go test ./... -count=1
```

覆盖的是易回归的点：坏 token 换号、400 不换号、全 401、流式工具调用拼装、
`[hidden]`/信封字段清理、forced_adaptive 绝不传 `thinking`、小 `max_tokens` 下的思考预算、
首次启动的环境变量注入。

## 声明

仅用于个人学习与自用接入，非官方项目。使用前请自行确认符合 MiniMax 服务条款；
账号风控、封禁等后果由使用者承担。
