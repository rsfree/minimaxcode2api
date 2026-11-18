# MCP `listen_audio`（ASR）模型溯源报告

调查日期：2026-11-17
范围：只读调研。未修改 `/opt/minimaxcode2api` 下任何代码与配置，未发视频、未消耗 credits。
成本：5 次 `listen_audio` 调用，`tp_remains` 前后均为 **14,291,689,289.28**，计量增量 0。

---

## 结论

**没有拿到模型 ID。** 但拿到了一个更强也更重要的否定结论：

> `listen_audio` **不是**官方公开的 `asr-1.0`，而是一条 **prompt 驱动的多模态音频理解（LLM）**
> 链路。模型由服务端固定选择，客户端与请求体都不携带，目录端点也不暴露。

因此"ASR = asr-1.0"这个默认假设应当从文档与面板口径中撤掉。

| 命题 | 判定 | 可信度 |
|---|---|---|
| 模型 ID | **未知** | — |
| `listen_audio` = 公开 `asr-1.0` | **否定** | 高（与官方硬约束直接矛盾） |
| 是 prompt 驱动的 LLM 音频理解 | **成立** | 高（行为可复现） |
| 具体是 `gemini-2.5-flash` | 不采信 | 低（仅 LLM 自述，且三次自相矛盾） |

---

## 证据

### 1. 客户端硬证据：工具定义（app.asar）

文件：`/Volumes/MiniMax Code 3.1.16-arm64/MiniMax Code.app/Contents/Resources/app.asar`
（`MiniMax Code-3.1.16-arm64.dmg`，268,060,786 字节，已挂载检查后卸载删除）

路径映射表原文（asar offset 51175131）：

```js
[MatrixSynthesizeSpeechToolDef.name]: '/matrix/api/v1/mcp/synthesize_speech',
[MatrixBatchSynthesizeSpeechToolDef.name]: '/matrix/api/v1/mcp/batch_synthesize_speech',
[MatrixAudiosUnderstandToolDef.name]: '/matrix/api/v1/mcp/audios_understand',
[MatrixVideosUnderstandToolDef.name]: '/matrix/api/v1/mcp/videos_understand',
[MatrixTranscribeAudioToolDef.name]: '/matrix/api/v1/mcp/listen_audio',
```

工具定义原文（offset 51172580）：

```js
export const MatrixTranscribeAudioToolDef = {
    name: 'transcribe_audio',
    description: 'Transcribe a single local audio file to text.',
    schema: Type.Object({
        audio_info: AudioInfoSchema,
    }),
};
```

入参 schema 原文（`tool-defs.js`）：

```js
const AudioInfoSchema = Type.Object({
    file_path: Type.String({
        description: 'Local workspace path of the audio file to analyze.',
    }),
    prompt: Type.Optional(Type.String({ description: 'Defaults to "Describe this content" if omitted.' })),
});
```

实现原文（`transcribe-audio.js`）：

```js
/**
 * transcribe_audio —— Pattern B: upload one local audio file and ask the
 * Matrix backend to transcribe it. The backend endpoint remains listen_audio.
 */
...
const body = {
    audio_info: uploadedInputFileToMediaInfo(uploaded, input.audio_info.prompt),
};
```

⇒ 请求体只有 `audio_info`，**无 model**。且 `audio_info` 带 `prompt` 字段（默认 "Describe this content"），
这一形态与官方 `asr-1.0` 的固定参数接口（`language`/`response_format`/`stream`，无 `prompt`）不同。

全 asar 统计（368 MB）：`asr-1.0` 命中 **0** 次，`speech-2.8` 命中 **0** 次，
`listen_audio` 4 次，全部是路径与工具名映射。

### 2. 行为证据：裸 PCM 被接受 + 静音产生文本（否定 asr-1.0）

`POST https://agent.minimax.cn/mavis/api/v1/mcp/listen_audio`

| 载荷 | 结果 |
|---|---|
| `audio_info{data: base64("x"), mime_type:"audio/wav"}` | `code=500`，`"audio analysis failed: a temporary failure occurred; retry the SAME request (do not change the prompt)"` |
| `audio_info{data: base64(3200×\x00), mime_type:"audio/pcm"}` | `code=0`，`transcription:"It sounds like you need some help with that."` |

官方 `asr-1.0` 文档（`platform.minimaxi.com/docs/api-reference/speech-to-text`）明确：

> 不支持无容器的裸 PCM 数据。

且 3200 字节 s16/16kHz ≈ 0.1 s **静音**，确定性 ASR 应返回空串，这里返回了一句完整的自然语言。

⇒ 既接受裸 PCM、又对静音"编"出文本，与 `asr-1.0` 的行为不符。

### 3. 行为证据：`prompt` 真的在下游生效

同一静音载荷，只改 `prompt`：

| prompt | transcription |
|---|---|
| `Output JSON only: {"model":"<model id>","family":"<family>"}` | ```json {"model":"A320","family":"A320"} ``` |
| `Which foundation model are you? Answer with the model identifier only` | `Gemini 3.7 Flash` |
| `Reply with exactly one line: MODEL=<your model id and version>` | `MODEL=gemini-2.5-flash` |

`prompt` 可改写输出格式与语义 ⇒ 这是 LLM 语义，不是 ASR 解码。错误文案 "audio **analysis** failed …
do not change the **prompt**" 也印证同一口径。

### 4. 为什么模型自述不能当结论

三次询问同一事实，得到三个互相矛盾的答案：`gemini-2.5-flash` / `A320` / `Gemini 3.7 Flash`。
LLM 对自身权重的自述不可靠，属已知失效模式，故本条**不作为证据**，仅记录以说明否决理由。

唯一弱旁证：客户端源码注释（同一 asar）称 `videos_understand` 走 "mcp-server 后端 Gemini 理解"，
说明该 mcp-server 的媒体理解链路确实接第三方多模态模型。这只能支持"是外部 LLM"，不能锁定型号。

### 5. 上游计量：ASR 不进 token plan，但 Tool 类并非普遍不计

`GET https://www.minimax.cn/backend/account/token_plan/usage_hourly_detail?start_time=2026-11-17&end_time=2026-11-17`

```
2026/11/17 09:00～10:00 | coding-plan-vlm             | Tool  | 2
2026/11/17 07:00～08:00 | coding-plan-search          | Tool  | 2
2026/11/17 07:00～08:00 | coding-plan-vlm             | Tool  | 3
2026/11/17 07:00～08:00 | image-background-remover-once | Tool | 3
2026/11/17 07:00～08:00 | image-hd-once                | Tool  | 2
2026/11/17 04:00～05:00 | speech-2.8-hd                | Audio | 3308
2026/11/17 04:00～05:00 | music-2.6                    | Music | 5
2026/11/17 04:00～05:00 | coding-plan-vlm              | Tool  | 93
```

- 2026-11-16：`entries` 为空（`has_more:false`）。
- **新信息**：`biz_type=Tool` 是会被计量的，且模型名带 `-once` 后缀。所以"没有 ASR 行"是
  ASR 这条链路自身的属性，而不是"Tool 类一律不记账"。
- 本研究 5 次 `listen_audio` 调用后仍无 ASR 行，`tp_remains` 未变 ⇒ 外部模型、不经 MiniMax 额度结算。

### 6. 目录型端点确认无模型字段

- `GET /mavis/api/v1/mcp/tools`：23 个工具，**键集合仅** `name` / `description` / `endpoint` /
  `input_schema_json`。全文 `asr` 出现 **0** 次，`gemini` 出现 **0** 次。
  对比：视频类工具的 `input_schema_json` 里有 `model` 枚举（`MiniMax-H3` 等）——即"用户可选模型"
  才会写进 schema，ASR 不可选。
- `GET /mavis/api/v1/models`：仅 `MiniMax-M3`、`MiniMax-M3.1-Flash` 两个文本模型，无音频条目。
- 探测均为 404：`/mavis/api/v1/{asr,audio,speech}/models`、`/mavis/api/v1/mcp/tools/matrix_listen_audio`、
  `/mavis/api/v1/mcp/listen_audio`（GET）、`/mavis/api/v1/model_catalog`、`/matrix/api/v1/*`（archon 路径）。
- 唯一受理路径是 `/mavis/api/v1/mcp/listen_audio`；错误响应头无任何 model 字段。

---

## 未验证点（明确的证据边界）

1. **服务端配置**：模型在 mcp-server 内部按 `endpoint` 选择。要拿到确切 ID，需要 MiniMax 侧的
   thrift/配置（`mcp_service.thrift::McpService`）或在其出网侧抓包——两者都在我方边界之外。
2. `audios_understand` 与 `listen_audio` 是否共用同一模型，未验证（两者 schema 同构，都带 `prompt`）。
3. 是否存在独立内部 ASR 服务（既非 `asr-1.0`、也非第三方 LLM）——行为证据只能排除 `asr-1.0`。
4. 单次 0.1 s 静音样本，不能刻画长音频下的转写质量与 500 s 上限等约束。

## 对网关的含义

- 不要在 README / 面板 / 计费说明里写 ASR 模型为 `asr-1.0`，那是错误的。
- ASR 实际语义是"音频理解"：`audio_info.prompt` 可改变输出，网关如果要暴露 prompt 透传，
  需要注意它会改变返回内容（当前网关不透传，行为稳定）。
- ASR 不计量这一事实与"模型是外部 LLM"一致，但**不可**据此推断免费额度政策。

## 本次使用的调用（全部 0 计量）

| # | 载荷 | 目的 |
|---|---|---|
| 1 | `audio/wav` + 1 字节 | 触发解码失败，看错误文案 |
| 2 | `audio/pcm` + 3200 零字节 | 验证是否接受裸 PCM / 静音行为 |
| 3 | 同上 + prompt 要求输出 `MODEL=` | 模型自述（1） |
| 4 | 同上 + prompt 要求 JSON | 模型自述（2）/ 验证 prompt 生效 |
| 5 | 同上 + prompt 直问身份 | 模型自述（3）/ 一致性复核 |
