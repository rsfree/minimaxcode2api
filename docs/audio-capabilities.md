# 语音能力声明（本部署）

> 本文件是 **audio adapter 的「实现声明」**：北向只固定字段骨架，
> 枚举、默认值、数值区间以本文件为准；未在本文件声明的取值一律 400。
>
> 同源实现：`internal/audioproto/proto.go`；机器可读版：`GET /v1/audio/capabilities`。
> 版本 **0.7.0**，核对日期 **2026-11-24**。

## 1. 上游事实（探测证据）

| 能力 | 事实 | 证据 |
|---|---|---|
| TTS `synthesize_speech` | schema 只有 `text / voice_id / speed / volume / pitch / emotion`，**没有 format** | 客户端 `app.asar` 的 `tool-defs.js` + 2026-11-21 实测 |
| TTS 输出格式 | 恒为 **MP3**；`format` / `audio_setting` / `sample_rate` / `bitrate` 全部被忽略 | 4 组参数探测，产物一律 `ID3/MP3` |
| TTS 语速 | `speed` 声明区间 `[0.5, 2.0]` | `tool-defs.js` |
| TTS 文本 | 官方上限 **10000 字符** | MiniMax T2A 文档（未在 10000 长度实测） |
| ASR `listen_audio` | 入参只有 `audio_info{url|data, mime_type, prompt}`；响应只有 `transcription` | 实测原始响应：`code/message/transcription/error/base_resp` |
| ASR 时间戳/语言 | **不返回**；`language` 字段被静默忽略 | 2026-11-21 实测 |
| ASR `prompt` | 被当作 **分析指令**，会输出“音频分析”段落，不是逐字稿 | 实测：传“这是一段中文语音”后返回带分析的文本 |
| 音色 | `get_voice_list` 返回 **327** 个预置音色 | 生产 `/v1/voices` 实测 |
| 非幂等 | 合成/转写/音乐可能已生成并计费；网络错误/5xx/超时**不自动重试** | `callMedia` 的 `nonIdempotentTool` 策略 |

## 2. `POST /v1/audio/speech`

| 字段 | 骨架 | 本部署声明 |
|---|---|---|
| `model` | 必填非空 | 任意非空字符串；上游固定 `speech-2.8-hd`（model 被忽略） |
| `input` | 必填字符串 | **1–10000 字符**；超长 400 `param=input` |
| `voice` | 必填非空 | 13 个 OpenAI 别名（见下）+ **任意原生 `voice_id` 直传**；空值 400 |
| `response_format` | 非空字符串，参考 mp3 | **只支持 `mp3`**；其余（含 wav/pcm/opus/aac/flac）400 `param=response_format` |
| `speed` | 数字 | **0.5–2.0**；不传 = 1.0；超出 **400** `param=speed`，不钳位 |
| `instructions` | 可选字符串 | **不支持**；传入非空值 400 `param=instructions` |
| `stream_format` | 非空字符串 | `audio`（默认）/ `sse`；其他 400 |
| `volume`（扩展） | 数字 | 0.0–10.0，越界 400 |
| `pitch`（扩展） | 整数 | -12–12，越界 400 |
| `emotion`（扩展） | 字符串 | `happy / sad / angry / fearful / disgusted / surprised / neutral` |
| 其他 JSON 字段 | — | **400 `param=<字段名>`**（`DisallowUnknownFields`） |

响应：原始 MP3 字节，`Content-Type: audio/mpeg`；`stream_format=sse` 时按
`speech.audio.delta` → `speech.audio.done` → `[DONE]` 下发（网关扩展，上游一次给完整音频）。

诊断头：`X-MiniMax-Format: mp3`、`X-MiniMax-Voice`、`X-MiniMax-Voice-Alias`（命中别名时）。

## 3. `POST /v1/audio/transcriptions`

| 字段 | 骨架 | 本部署声明 |
|---|---|---|
| `file` | 必填单个文件 | multipart；**重复 file 400**；≤ **25 MiB**；空文件 400 |
| `model` | 必填非空 | 任意非空字符串；上游固定 `asr-1.0`（model 被忽略） |
| `response_format` | 非空字符串，参考 json | `json` / `text` / `verbose_json` |
| `stream`（扩展） | 布尔 | `true` 回 `transcript.text.delta` / `.done` 事件（上游一次给全文） |
| `language` / `languages` | 可选 | **400**：上游没有语言参数 |
| `prompt` | 可选 | **400**：上游把它当分析指令，与 OpenAI 词汇提示语义不同 |
| `temperature` | 数字 | **400**：上游没有采样温度 |
| `timestamp_granularities[]` | 可重复 | **400**：上游不返回时间戳 |
| `include[]` | 可重复 | **400**：上游不返回 logprobs |
| `chunking_strategy` | 字符串/对象 | **400**：上游没有分块策略 |
| `keywords[]` / `known_speaker_*` | — | **400**：上游不支持关键词/说话人注册 |
| 其他字段 | — | **400 `param=<字段名>`** |

不支持的 `response_format`：

| 取值 | 原因 |
|---|---|
| `srt` / `vtt` | 需要真实分段，上游不返回时间戳 |
| `diarized_json` | 需要说话人分离，上游不提供 |

`verbose_json` 只输出 **有真实数据**的字段：`{"task":"transcribe","text":"..."}`；
不附 `segments` / `language` / `duration` / `words`。

JSON 入参是网关扩展（便于 relay）：`{"audio_url"|"url"|"data","mime_type"}`；
其他 Content-Type 一律 **415**。

## 4. 音色映射（13 个 OpenAI 别名）

| OpenAI | MiniMax | OpenAI | MiniMax |
|---|---|---|---|
| alloy | male-qn-qingse | nova | female-shaonv |
| echo | male-qn-jingying | shimmer | female-tianmei |
| fable | male-qn-jingying-jingpin | sage | female-yujie |
| onyx | male-qn-badao | coral | female-chengshu |
| ash | male-qn-daxuesheng | ballad | male-qn-badao-jingpin |
| verse | male-qn-qingse-jingpin | marin | female-shaonv-jingpin |
| cedar | male-qn-jingying-jingpin | | |

原生音色：`GET /v1/voices`（当前 327 个），`voice` 直接传 `id`。
映射是**近似**（性别/语调），不承诺与 OpenAI 听感一致。

## 5. 错误映射

| 场景 | HTTP | `error.type` | `error.code` |
|---|---|---|---|
| 参数/取值未声明 | 400 | `invalid_request_error` | `invalid_request` |
| 网关 API Key 错误 | 401 | `authentication_error` | `invalid_api_key` |
| 额度触顶 | 402 | `api_error` | `insufficient_quota` |
| 体积超限 | 413 | `invalid_request_error` | `request_too_large` |
| Content-Type 不支持 | 415 | `invalid_request_error` | `null` |
| 池满/全冷却 | 429 | `rate_limit_error` | `all_accounts_busy` |
| 上游 401/403 | **502** | `api_error` | `upstream_auth` |
| 上游其他 5xx / 网络 | 502 | `api_error` | `upstream_error` |

上游 401/403 映射为 502 是刻意的：失效的是网关自己的上游账号，不是调用方的 key。

## 6. 资源边界

| 项 | 上限 |
|---|---|
| speech JSON body | 1 MiB（`Content-Length`/读取双限） |
| speech input | 10000 字符 |
| transcriptions 文件 | 25 MiB |
| transcriptions JSON body | 25 MiB × 4/3 + 1 MiB（base64 膨胀） |
| multipart 文件数 | 1（`file`） |
| 并发/限流 | 由账号池与上游频控决定（TTS ≤8 并发 / ≤6 rps，ASR ≤10 并发） |

## 7. 明确不支持

- `/v1/audio/translations`、Realtime / WebSocket、自定义声音与同意管理
- wav/pcm/opus/aac/flac 输出（需真实转码；上游只出 mp3）
- 逐句/逐词时间戳、说话人分离、`logprobs`
- 语言提示、采样温度、关键词提示、prompt 提示（上游无等价能力）
- 非幂等请求的自动重试（可能重复计费）
