# minimaxcode2api @ ai-prod

生产实例：**https://minimaxcode.1task.cn**

| 项 | 值 |
|---|---|
| 机器 | `210.121.44.245`（首尔 AS9848），root + `~/.ssh/id_ed25519_ops` |
| 服务目录 | `/opt/minimaxcode2api/`（`src/` 源码、`data/` 运行时、`nginx/` 脚本与片段） |
| 容器 | `minimaxcode2api:0.2.0`，`restart: unless-stopped`，非 root（uid 10001） |
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
| `https://minimaxcode.1task.cn/healthz` | 200 `{"ok":true,…,"version":"0.2.0"}` |
| `/panel/`（SNI） | 200 |
| `/v1/models` 无 key | 401 |
| `/v1/models` 带 key | 200 `['MiniMax-M3.1-Flash','MiniMax-M3']` |
| `POST /v1/chat/completions` | 打通到上游（凭据有效），当前 **429 限额** |
| 面板登录（管理员口令） | 200 `{"ok":true}` |
| 账号视图 | 1 个，`mmoat_TL••••••AR4w`，剩余 17 462s |
| 证书链校验（不加 `-k`） | 200，`ssl_verify=0` |
| 未登记 Host | 404 |
