# minimaxcode2api @ ai-prod

生产实例：**https://minimaxcode.1task.cn**

| 项 | 值 |
|---|---|
| 机器 | `210.121.44.245`（首尔 AS9848），root + `~/.ssh/id_ed25519_ops` |
| 服务目录 | `/opt/minimaxcode2api/`（`src/` 源码、`data/` 运行时、`nginx/` 脚本与片段） |
| 容器 | `minimaxcode2api:0.4.0`，`restart: unless-stopped`，非 root（uid 10001） |
| 监听 | **仅回环** `127.0.0.1:39350` → 容器 `7864` |
| 入口 | 宝塔 nginx：`:80` 一律 301 → `:443` 反代回环 |
| 证书 | `/www/server/panel/vhost/cert/minimaxcode.1task.cn/`（acme.sh，到期 2027-02-09，自动续期） |
| 密钥 | `/opt/minimaxcode2api/.env`（600）：`MMX2A_API_KEY` / `MMX2A_ADMIN_PASSWORD` |

## 组成

```
/opt/minimaxcode2api/
├── docker-compose.yml        # 本目录同名文件
├── .env                      # 生产密钥（600，不入库）
├── data/                     # config.json / accounts.json / usage.json / videotasks.json
├── src/                      # 构建上下文（git archive 解包）
└── nginx/
    ├── minimaxcode.1task.cn.conf      # :80 → 301（含 ACME 通道）
    ├── minimaxcode.1task.cn-ssl.conf  # :443 反代（已投到 vhost 目录）
    ├── minimaxcode-proxy.inc          # 反代片段（已投到 vhost 目录）
    └── apply-https.sh                 # 幂等签发/装证书
```

nginx 侧真正生效的位置是 `/www/server/panel/vhost/nginx/`，`/opt` 下只是仓库副本。

## 首次部署（按序执行）

```bash
# 1) 本机打包（只打 git 跟踪的文件，天然排除 data/ 与 .env）
git archive --format=tar.gz --prefix=src/ -o /tmp/mmx2a-src.tgz HEAD

# 2) 上传并解包
ssh -i ~/.ssh/id_ed25519_ops root@210.121.44.245 \
  'mkdir -p /opt/minimaxcode2api/{data,src,nginx} && chown -R 10001:10001 /opt/minimaxcode2api/data'
scp -i ~/.ssh/id_ed25519_ops /tmp/mmx2a-src.tgz root@210.121.44.245:/tmp/
ssh ... 'tar -xzf /tmp/mmx2a-src.tgz -C /opt/minimaxcode2api/src --strip-components=1'

# 3) 构建镜像（2-3 分钟；用 nohup 免得 ssh 断连掐死）
ssh ... 'cd /opt/minimaxcode2api/src && nohup docker build -t minimaxcode2api:0.2.0 . > /tmp/mmx2a-build.log 2>&1 &'

# 4) 投 compose + .env 并起容器
scp ... docker-compose.yml .env root@210.121.44.245:/opt/minimaxcode2api/
ssh ... 'chmod 600 /opt/minimaxcode2api/.env && cd /opt/minimaxcode2api && docker compose up -d'

# 5) nginx :80 vhost（先让 ACME 通道通），再 reload
scp ... nginx/minimaxcode.1task.cn.conf nginx/minimaxcode-proxy.inc \
  root@210.121.44.245:/www/server/panel/vhost/nginx/
ssh ... '/www/server/nginx/sbin/nginx -t'
ssh ... '/www/server/nginx/sbin/nginx -s reload'

# 6) DNS A 记录（阿里云 API，profile default，幂等）
#    RR=minimaxcode  DOMAIN=1task.cn  → 210.121.44.245  TTL=600

# 7) 签证书 + 投 :443 + 验证矩阵（3-4 分钟，后台跑）
ssh ... 'cd /opt/minimaxcode2api/nginx && nohup env HOST=minimaxcode.1task.cn bash apply-https.sh > /tmp/mmx2a-https.log 2>&1 &'
```

## 日常运维

```bash
# 状态 / 日志
docker ps --filter name=minimaxcode2api
docker logs -f --tail 100 minimaxcode2api
journalctl 不用看，容器日志已限到 3×20MB

# 改配置（热生效，不用重启）：改 data/config.json 后 PUT 面板设置，或直接编辑文件后重启容器
docker restart minimaxcode2api

# 升级：改完代码 push → 服务器重新拉包重建
git archive --format=tar.gz --prefix=src/ -o /tmp/mmx2a-src.tgz HEAD
scp ... ; ssh ... 'tar -xzf ... && cd /opt/minimaxcode2api/src && docker build -t minimaxcode2api:<新版本> .'
# 改 compose 里的 image tag → docker compose up -d（会重建容器，数据在 ./data 挂载里不受影响）
```

## 🔴 两个只有实操才会发现的坑

1. **换 `accounts.json` 必须先 `docker stop`**。
   服务在优雅退出时会用**内存状态**回写 `accounts.json`。运行中直接覆盖文件，
   再 `docker restart` 会把你的文件冲成空表（本次实测：697 字节被写成 37 字节，
   启动日志还显示"账号 0 个"）。顺序必须是 `stop → 覆盖 → chown 10001 → start`。

2. **面板口令不等于 `api_key`**。
   设了 `MMX2A_ADMIN_PASSWORD` 时，`/panel/api/*` 的 Bearer 只认它；拿 `api_key` 会得到
   `401 invalid_key`。面板登录页同理。生产两者分开设，泄露面不重叠。

## 已知状态与边界

- **额度**：账号是 MiniMax Code 个人订阅（`mcode-public` / `agent.default`）。5 小时窗口 +
  周窗口是滚动限额，**触顶时所有生成类接口一起 429/402** —— 不是网关故障。
  上游原文写 "plan balance has run out … need to top up"，实际是限额重置，别去充值。
- **单账号**：目前池里 1 个账号。多账号在面板里「添加账号 → 设备授权」，别复用桌面客户端
  的 refresh_token（会触发 reuse detection 把整个 token family 吊销）。
- **视频**：协议与状态机已验，但 mp4 下发从未在额度充足时跑通过（提交需 280/416 credits）。

## 实测验证矩阵（2026-11-11 23:33）

| 检查 | 结果 |
|---|---|
| `:80` → `:443` | 301 |
| `https://minimaxcode.1task.cn/healthz` | 200 `{"ok":true,…,"version":"0.3.0"}` |
| `/panel/`（SNI） | 200 |
| `/v1/models` 无 key | 401 |
| `/v1/models` 带 key | 200 `['MiniMax-M3.1-Flash','MiniMax-M3']` |
| `POST /v1/chat/completions` | 打通到上游（凭据有效），当前 **429 限额** |
| 面板登录（管理员口令） | 200 `{"ok":true}` |
| 账号视图 | 1 个，`mmoat_TL••••••AR4w`，剩余 17 462s |
| 证书链校验（不加 `-k`） | 200，`ssl_verify=0` |
| 未登记 Host | 404 |

### 浏览器端到端（2026-11-11 23:40，真机 Chrome）

用 bsk 在线上实点，覆盖登录门 + 五个视图 + 三个运维动作：

| 步骤 | 结果 |
|---|---|
| 管理口令登录 | 通过，跳管理台 |
| 概览 | 账号 1/1、今日请求 1、接入信息与上游地址正确 |
| 账号 | 异常态红色徽标 + 错误原文、掩码 token、剩余 4.8h、19 次用量 |
| 模型 | 两个模型与别名映射正确 |
| 请求日志 | 429 那条可见（IP 112.86.208.244 · curl/8.7.1 · 2.7s） |
| 设置 | 各分区渲染正常 |
| 「测试」按钮 | 弹出红色 toast：`失败：upstream 429 …（2056）` |
| 「清冷却」按钮 | toast「已清除冷却与错误」，状态转绿；随后 `/v1/voices` 立刻 200（327 个） |

**这次 E2E 抓到一个真 bug**：`#toast` 元素带 `hidden` 属性，JS 只改 `className`
从没移除它 —— 配合样式里的 `[hidden]{display:none!important}`，提示条**永远是隐藏的**。
表现是面板任何操作（测试/保存/删除）都没有反馈，只能靠列表重载去猜结果。
已在 0.2.1 修复（`t.hidden=false` + `.toast{pointer-events:none}` 防挡按钮）。
同时把「恢复」改名「清冷却」—— 它清的是冷却与错误，不是启用账号，紧挨着「停用」
时语义容易误解。

## 全方位测试（2026-11-11 23:55，62 项全绿）

脚本：`/tmp/full_e2e.py`（62 项，分组 A–J），对公网域名跑，不依赖额度。

| 组 | 覆盖 | 结果 |
|---|---|---|
| A 入口 | healthz / 版本 / :80→301 / 证书链 / CSP / XFO / nosniff / **HSTS** / 404 / 预检 | 9/9 |
| B 鉴权 | 无 key / 错 key / Bearer / x-api-key / 面板拒 api_key / /status | 7/7 |
| C 模型 | 列表 / 两个别名 / 未知模型 404 | 5/5 |
| D chat | 坏 JSON / 缺 messages / 类型错 / 合法 / 流式 / 工具 | 6/6 |
| E anthropic | 缺 max_tokens（透传语义）/ count_tokens / 未知模型错误形状 | 3/3 |
| F 图片 | 五类拒绝 + 合法请求触及上游 | 7/7 |
| G 视频 | content 缺失 / 场景互斥 / 480p / frames / 列表 / 404 / /api/v3 / 同步门 | 9/9 |
| H 音频 | speech 缺参 / music 过短 / 转写载体 / 音色 | 4/4 |
| I 面板 | 错口令 / 对口令 / 六个只读接口 / 导入空体 / 路径穿越 | 10/10 |
| J 并发 | 20 并发 /v1/models 全 200；10 并发 chat 无 5xx | 2/2 |

另外：`go test ./... -race` 干净；图片成功路径 12/12（假上游）；50 并发 chat 2.3s 内全部
返回（429，零 5xx、零超时）；`docker restart` 后账号文件 md5 不变、健康检查恢复。

### 这一轮修掉的四个真问题

1. **`/v1/models/{alias}` 返回 404** —— 详情接口只比原始 id，认不了 `M3` / `m3.1-flash`。
   客户端拿别名探测就会以为模型不存在。已改为走 `resolveModel`。
2. **媒体链路的容量错误口径与文本链路不一致** —— 池空/全冷却在媒体接口报
   `502 upstream_error`，而文本链路报 `503 no_account` / `429 all_accounts_busy`。
   客户端会把"冷却"当上游故障重试，形成重试风暴。现在两条链路同口径，且都带
   `Retry-After: 5`。
3. **缺 HSTS** —— 公开 HTTPS 端点没有 `Strict-Transport-Security`，已加
   `max-age=31536000`（不带 includeSubDomains，避免波及 1task.cn 其他子域）。
4. **字段类型错被归成 `invalid_json`** —— `{"messages":"hi"}` 的 JSON 语法本身合法，
   报 invalid_json 会让调用方去查转义。现在用 `json.UnmarshalTypeError` 区分：
   语法坏 → `invalid_json`，类型错 → `invalid_request`。

> 顺带纠正我自己写错的两条断言：`/status` 传了合法 key 当然是 200（鉴权是对的）；
> `480p` 报 `unsupported_parameter` 比 `invalid_request` 更准确（是取值域不支持）。
