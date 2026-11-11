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

图片、视频、语音的路由见《[媒体接口](#媒体接口图片--视频--语音)》。

未匹配的路径回 **JSON** `404 unknown_route`（不是 Go 默认的纯文本 405）。`OPTIONS *`
一律 204，CORS 允许 `GET/POST/DELETE`。

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
- **400/404/422**：请求本身有问题，换号也一样失败，**直接透出**上游状态与消息；
- **全部账号都 401/403**：`502 upstream_auth` —— 下游的 `sk-mmx` 已经验过，失效的是
  网关自己的上游账号。这里**不能**回 401：客户端会以为自己 key 错了，重配换 key 都无救。

错误对象统一是 OpenAI 的四键形状 `{message, type, param, code}`（`param` 拿不到字段名时
为 `null`）—— 图像、视频、音频三条链路共用同一个写入函数，避免某个域把 `param` 写漏。

刷新是**账号级串行**的，且拿到锁后会重读账号——避免并发请求用同一个 `refresh_token`
二次轮换，把自己刷成 `invalid_grant`。

## 管理台

- **概览**：账号健康度、今日/累计请求与 token、缓存命中、14 天趋势
- **账号**：状态、掩码 token、过期倒计时、用量；测试 / 停用 / 改名 / 恢复 / 删除
- **模型**：对外清单、别名、能力（上下文、输出上限、思考特性）
- **请求日志**：最近 N 条（时间、模型、状态、耗时、token、账号、来源 IP/UA、换号次数）
- **设置**：调度策略、超时、默认模型、面板口令等白名单热改

界面无框架、无外部字体/CDN，`script-src 'self'` 严格 CSP，所有文本走 `textContent`。

## 媒体接口（图片 / 视频 / 语音）

文本走 `/mavis/api/v1/llm/v1/messages`；媒体走**另一个网关**
`/mavis/api/v1/mcp/<tool>`。桌面端在 `DesktopMatrixClient` 里把
`/matrix/api/v1/mcp/*` 重写成 `/mavis/api/v1/mcp/*` 再发。鉴权同样只要
`Authorization: Bearer`，**不需要 `x-signature`**。

已实现的路由（都真调过上游）：

| 路由 | 说明 | 状态 |
|---|---|---|
| `POST /v1/images/generations` | OpenAI 图像协议（canonical），见下节 | ✅ 真调 + schema 全验 |
| 视频四个端点 | 异步视频协议（canonical），见下节 | ✅ 提交/查询/列表/删除全验 |
| `POST /v1/audio/speech` | 文本转语音，直接回音频字节 | ✅ 真调通过 |
| `POST /v1/audio/transcriptions` | 语音转文字，支持 multipart 与 JSON(base64/url) | ✅ TTS→ASR 闭环一致 |
| `POST /v1/audio/music` | 音乐生成，默认回字节，`response_format=url` 回链接 | ✅ 真调通过 |
| `GET /v1/voices` | 音色列表（实测 327 个） | ✅ 真调通过 |

### 上游的五个坑（都是实测踩出来的）

1. **业务错误永远 HTTP 200**，真正的码在 body 的 `code` 里：`0` 成功、`400` 参数、
   `402` 计费、`401` 凭据。只看状态码会把"限额触顶"当成成功。
2. **`code=503` 不是失败，是"稍后再查"**。视频会返回
   `status=succeeded` + `code=503` + "已就绪但还没准备好下发，查同一个 task_id，
   别重新提交"。网关此时对外报 `status=running` 并带上 `upstream_status`，
   否则客户端会立刻取 url 落空、把已完成任务判成失败。这类响应**绝不换号重试**。
3. **产物 Content-Type 会撒谎**。实测 JPEG 被标成 `image/png`、文件名还是 `.png`。
   网关一律按 magic bytes 嗅探（`internal/server/media.go: sniffMime`），
   `b64_json` 与音频字节都走同一套判断。
4. **`resolution` 是三档档位不是像素**：`1K/2K/4K`，实际输出由上游按比例决定
   （实测 1K+1:1=1024²，1K+16:9=1408×768，2K+16:9=2752×1536）。
5. **`image_synthesize` 是槽位制**：`requests[]` 每项必须给 `output_file`
   （槽位名），返回按槽位名对应回来，成功/失败是两个分开的数组。

### 计费与额度（重要）

同一个账号下，不同模型的扣费来源不一样，实测差异很大：

| 能力 | 结果 |
|---|---|
| 图片生成 / TTS / ASR / 音乐 | 正常出结果（成本低，走各自的额度池） |
| `MiniMax-Hailuo-2.3` 视频 | 提交需 **280 credits**，额度不足时 `402` |
| `MiniMax-H3` 视频 | 提交需 **416 credits**，且只走 M Plan 周额度 |

**402 是限额触顶，不是余额不足。** 上游原文写的是 "plan balance has run out …
need to top up"，但控制台实际显示的是：

```
5 小时限额   已用 100%   4 小时 21 分后重置
周限额       已用 10%    6 天 23 小时后重置
图片生成     已用 1/10   （限时免费额度）
积分         0
```

也就是**按窗口重置的滚动限额**，不是要你去充值。网关在 402 的错误 message 后面
统一追加了一句提示，避免运维照上游原文跑去充值。调试期的正确姿势：降频等窗口重置，
或者把重活分散到多个账号。
网关对 402 的映射是 `HTTP 402 + code=insufficient_quota`，且**不重试、不换号**
（换号也一样扣不到钱，重试只会放大失败）。

### 实测耗时（供超时配置参考）

| 操作 | 耗时 |
|---|---|
| 1 张 1K 图 | ≈15s |
| 1 张 2K 图 | ≈42s |
| 批量 2 张 1K | ≈109s |
| 音乐 1 首 | ≈25s |
| TTS 一句 | <1s |
| 视频 Hailuo 6s（提交→succeeded） | ≈235s（queued 167s 后转 running） |
| 视频下发准备 | 本次因额度不足一直 `503`，未拿到最终 mp4 |

默认 `upstream.timeout_seconds=600`，跑批量图与视频轮询够用；要跑 4K 多图建议调到 900。

## 图片接口（OpenAI 图像协议）

只有一个入口：`POST /v1/images/generations`（JSON）。图片引用用字符串
（URL / data URI / 裸 base64），`image` 支持 `string` / `string[]` / `null`。

> 刻意不做 `/v1/images/edits`：那是 multipart 载体前门，收文件就要替客户端托管字节、
> 还多一套"哪些字段能传"的歧义。i2i 直接用 `generations` 传 `image` 即可。

### 请求与归一化

```json
{"prompt":"把背景改成纯黑","image":["https://…/a.png"],"n":2,
 "size":"1024x1024","response_format":"b64_json"}
```

- `prompt` 只在**无图时必填**；
- `image` 三种写法等价；`mask` 有值必须同时有 `image`；
- `n` 的非法值（`0` / `2.5` / `2.0` / `"3"` / `true` / `null`）**回落 1**，不报错
  （`2.0` 也算非法：JSON 数据模型里它是整数，但服务端按 `isinstance(int)` 判定）；
- `response_format` 只认 `url` / `b64_json`，其余**删键**（= 不干预上游形态）；
- 其余字段一律透传，`usage` 之类不吞。

另外支持两个 **MiniMax 原生扩展**（不属于 OpenAI 协议，但显式声明后优先于
`size`/`quality` 推导）：

| 字段 | 取值 | 说明 |
|---|---|---|
| `aspect_ratio` | `21:9` `16:9` `4:3` `1:1` `3:4` `9:16` | 直接写进上游槽位 |
| `resolution` | `1K` `2K` `4K` | 直接写进上游槽位 |

### 吸附规则（实测出来的，不是猜的）

上游会吸附，但方式**不可靠**：`abc` / `8K` 会静默回落成 16:9 / 1K（悄悄改档位＝
悄悄改账单），而 `2:1` / `7:3` 这类"看着合法"的比例会直接生成失败。所以网关
**先吸附成合法值再下发**，并把吸附结果放进响应的 `warnings`：

| 客户端写 | 网关吸附为 | 实测输出 |
|---|---|---|
| `2:1` | `16:9` | 1376×768 |
| `7:3` | `21:9` | 1584×672 |
| `3:2` | `4:3` | 1200×896 |
| `8K` | `4K` | 5504×3072 |
| `1440p` | `2K` | （单测覆盖，2K 档 ≈2752×1536） |
| `1080p` | `1K` | 1K 档 16:9 本来就是 1376×768 |

比例用**对数距离**取最近六种之一；档位按后缀语义分：`k` 乘 1024、`p` 按高度、
裸数字按长边。完全无法解析（如 `abc`）才 400。

档位阈值只有**一张表**（`resolutionFromLongest`）：长边 ≤1400 → 1K、≤2800 → 2K、
其余 → 4K。`size` 推导与 `resolution` 吸附共用它——三档本质是**像素预算**而不是
固定边长（实测 1K≈1.05MP、2K≈4.2MP、4K≈16.9MP，各比例下面积恒定），所以用长边
分档就够，不必逐档算面积。

> 这处曾经分叉过：`size=1536x1024` 走一套阈值判 2K，`resolution="1536"` 走另一套
> 判 1K，同一个数字两个答案。现在有 `TestSizeAndResolutionAgree` 盯着。

**响应侧也会吸附**：所有实测尺寸都是 **16 的整数倍**（1584=99×16、672=42×16…），
且比例是六种之一（1584/672=2.357 而非 2.333）。所以别指望拿到数学精确的
21:9 —— 要精确像素就自己裁。

### 实测尺寸矩阵（1K 档，真调上游）

| 请求比例 | 实际输出 | 请求比例 | 实际输出 |
|---|---|---|---|
| 21:9 | 1584×672 | 3:4 | 896×1200 |
| 16:9 | 1376×768 | 9:16 | 768×1376 |
| 4:3 | 1200×896 | 1:1 | 1024×1024 |
| 2K + 16:9 | 2752×1536 | 4K + 16:9 | **5504×3072**（≈9MB） |
| 4K + 1:1 | 4096×4096（≈10MB） | size 1536×1024 | 2400×1792 |

> 这些数字是**真调上游**测的。注意 `aspect_ratio`/`resolution` 必须落到
> `requests[]` 槽位里；曾经因为按协议重写时把它们当成 vendor extras 透传到
> body 顶层，导致所有比例请求静默回 1024×1024 —— 已修为显式扩展并加了用例。

`size` 只映射到「比例 + 档位」—— 上游是**矩阵不是自由 WxH**：
`1280x720→1K/16:9`、`1536x1024→2K/4:3`、`4096x4096→4K/1:1`；`quality=hd` 显式优先。
批量 `n=3` 实测三张各不相同且槽位对应正确。

### 三处登记偏差（渠道限制，不是实现偷懒）

| 偏差 | 原因 | 表现 |
|---|---|---|
| 有 `image` 也必须给 `prompt` | 上游强制非空 prompt（空串直接 400） | 400 `unsupported_parameter` / `param=prompt` |
| `mask` 不支持 | 上游没有局部重绘 | 400 `unsupported_parameter` / `param=mask` |
| `n>10` 钳到 10 | 上游单次最多 10 个槽位 | 响应带 `warnings` |

### 错误归因

四键信封 `{message, type, param, code}`，按「谁的错」翻：参数错 400；审核拒答
`400 content_filter`（重试无用）；上游账号失效 `502 upstream_auth`；
限额触顶 `402 insufficient_quota`（等窗口重置）；网络/5xx `502 upstream_error`。

### 验证

- **12 项端到端**：真网关二进制 + 假上游 + 官方 `image-protocol.schema.json` 机器校验，
  覆盖文生图 / 批量 / b64 / image 数组 / image 单串 / 五类拒绝 —— 全绿；
- Go 侧 9 个用例：`n` 归一化（含 `2` vs `2.0`）、`image` 三写法、`mask` 无图、
  `size→档位`、extras 透传、data URI 按 magic bytes 判 mime。

## 视频接口（异步视频生成协议）

视频是 Job 型任务：**受理 ≠ 成功**。本网关按 `video-adapter` 的 canonical 契约实现，
路径与火山方舟一致：

| 方法 | 路径 | 回包 |
|---|---|---|
| `POST` | `/v1/contents/generations/tasks` | 只有 `{"id": "…"}` + `Location` 头 |
| `GET` | `/v1/contents/generations/tasks/{id}` | canonical 任务对象 |
| `GET` | `/v1/contents/generations/tasks` | `{items[], total}`（真实数据） |
| `DELETE` | `/v1/contents/generations/tasks/{id}` | 终态删记录；非终态 `501` |

`/api/v3/contents/generations/tasks` 是同一组端点的别名 —— Ark SDK 只改 `base_url` 就能直接用。

### 请求长什么样

```json
{
  "model": "MiniMax-H3",
  "content": [
    {"type": "text", "text": "海边灯塔，黄昏，镜头缓慢推近"},
    {"type": "image_url", "image_url": {"url": "https://…/first.png"}, "role": "first_frame"}
  ],
  "duration": 6, "resolution": "720p", "ratio": "16:9", "watermark": true
}
```

`model` 支持 `MiniMax-H3`（原生音频 + 多模态参考，需 416 credits）与
`MiniMax-Hailuo-2.3`（需 280 credits），别名 `h3` / `h2.3`。

### 六态与三条硬规矩

状态：`queued` / `running` / `succeeded` / `failed` / `expired` / `cancelled`。

1. **创建响应只有 `id`**。POST 200 只代表受理，成功判据是
   `status=succeeded` 且 `content.video_url` 非空。
2. **`video_url` 只在 succeeded 有值**，其余状态一律 `null`。上游会返回
   `status=succeeded` + `code=503`（"已就绪但还没准备好下发"），网关把这种情况
   收敛成 `running`，免得客户端拿着一个空 url 当成功。
3. **未知状态收敛为 `running`**（非终态）并打 `status_unnormalized` 标记，
   绝不会被当成 succeeded 提前落库。

### 参数双载体

body 字段是强校验，prompt 后缀是弱校验：

```json
{"content":[{"type":"text","text":"小猫打哈欠 --rs 720p --rt 16:9 --dur 6 --wm true"}]}
```

网关会**剥离后缀 → 回填字段 → 只发强校验给上游**；body 与后缀冲突时 body 优先，
并记 `warnings`（在 GET 的任务对象里可见）。这不是洁癖：后缀留在提示词里会被模型
当画面文字读出来，而拼错的后缀会被上游静默忽略 —— 请求 200、任务成功、就是时长不对。

### 校验与拒绝

判定顺序固定：`content → model → 场景互斥 → 参数取值域`。只拒四类：

- `content` 缺失 / 空数组 / 元素缺 url；
- `model` 缺失或不在支持列表；
- **首帧/首尾帧与全模态参考混用**（"首尾帧变成参考图"事故的根源）；
- 计费相关字段非法（`resolution` / `ratio` / `duration` / `frames`）。

`resolution` 是**档位不是像素**，上游只有 768P/1080P/2K：`720p→768P`；
`1080p` 在 H3 上换算成 `2K`；`4k` 换算成该模型最高档并给 warning；
`480p` 直接 400（没有对应档位，静默升档会改账单）。`frames` 与 `duration=-1`
上游都不支持 → 400。

### 错误归因

四键信封 `{message, type, param, code}`，按「谁的错」翻译而不是照抄上游码：

| 场景 | HTTP | code |
|---|---|---|
| 参数错 | 400 | `invalid_request` / `unsupported_parameter` |
| 运行期内容审核（在任务对象 `error` 里） | 400 | `content_filter` |
| 上游账号失效 | 502 | `upstream_auth` |
| 额度不足 | **402** | `insufficient_quota` |
| 网络 / 5xx | 502 | `upstream_error` |

> 402 是**有意的偏离**：协议把渠道问题归到 5xx，但对自建网关来说"限额触顶"和
> "上游挂了"是两种完全不同的处置，保留 402 才排得动障。

### 任务对象

```json
{
  "id": "462168147714259", "model": "MiniMax-Hailuo-2.3", "status": "running",
  "content": {"video_url": null, "last_frame_url": null},
  "error": null, "created_at": 1794386542, "updated_at": 1794388710,
  "duration": 6, "resolution": "720p", "ratio": "16:9"
}
```

参数回显不丢（计费对账要用）。`usage` 只有上游给出时才带 —— 当前媒体接口不返回
token 用量，所以通常没有这一项，这是**已知缺口**而不是省略。

任务快照落盘在 `data/videotasks.json`，重启后仍可查：上游查询必须带 model，
而协议客户端轮询只带 id，所以提交那一刻就得记下来。

### 已知边界

- **不做回调**：`callback_url` 接受但记 warning，请用 GET 轮询（上游本身也不支持）；
- **不做取消**：上游没有取消接口，DELETE 只删本地记录，非终态返回 501；
- 参考视频要额外给 `reference_video_duration_seconds`（上游拿它校验，网关无法可靠探测远程时长）；
- 参考图 / 参考视频 / 参考音频只支持 `MiniMax-H3`。

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
- 不做「视觉理解 / 联网搜索」的独立路由：视觉走 `/v1/chat/completions` 的图片输入即可
  （上游 `images_understand`、`web_search` 这些工具能调通，但属于 agent 内部工具，没做成对外 API）；
- 视频不做回调、不做取消（上游两者都不支持，见上一节）；
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
internal/media/      媒体网关客户端（/mavis/api/v1/mcp/*，业务码在 body 里）
internal/convert/    OpenAI ⇄ Anthropic 协议转换（请求/响应/流式）
internal/imageproto/ OpenAI 图像协议：canonical 折叠 + 上游槽位映射（纯函数）
internal/videoproto/ 异步视频协议：canonical 折叠 + 六态收敛（纯函数）
internal/videotask/  视频任务快照（落盘，重启可查）
internal/pool/       账号池调度、冷却、熔断
internal/server/     HTTP 路由、SSE、重试与换号
                     └ images.go / videos.go / audio.go：媒体路由与产物下载
internal/panel/      管理台 API + 内嵌 UI（go:embed）
internal/usage/      按天/模型/账号用量聚合
internal/reqlog/     请求日志环形缓冲
```

## 测试

```bash
go test ./... -count=1
```

覆盖的是易回归的点：坏 token 换号、400 不换号、全账号 401 → 下游 502、流式工具调用拼装、
`[hidden]`/信封字段清理、forced_adaptive 绝不传 `thinking`、小 `max_tokens` 下的思考预算、
首次启动的环境变量注入、地域切换、以及媒体层的四类边界：

- 402 计费错误**只打一次上游**（不重试、不换号）且映射成 `insufficient_quota`；
- `code=503 + status=succeeded` 的视频轮询对外报 `running`，不能当终态；
- `b64_json` 按 magic bytes 判类型（假上游故意把 JPEG 标成 `image/png`）；
- multipart 转写要把文件转成 base64 内联，且 `mime_type` 按嗅探结果填。

## 声明

仅用于个人学习与自用接入，非官方项目。使用前请自行确认符合 MiniMax 服务条款；
账号风控、封禁等后果由使用者承担。
