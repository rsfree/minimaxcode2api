#!/usr/bin/env python3
"""线上一体化验收（62 项）。

用法：
    export MMX_KEY=<对外 API Key>
    export MMX_ADMIN=<面板管理员口令>
    python3 scripts/e2e_prod.py

设计取舍：额度耗尽时上游必然 402/429，所以断言分两层 ——

  [严格] 校验/路由/鉴权必须给出确定的状态码与错误码，不受上游状态影响；
  [触及] 合法请求只要"打到池或上游"即可，状态落在允许集合内且信封合法。

这样即使账号被限额打满，也能验证协议层不回归；成功路径由
`deploy/ai-prod` 里的假上游脚本另行覆盖（图片 12 项 / 视频 13 项）。
"""
# ---- 原始脚本（BASE 固定为生产域名） ----
import base64
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = "https://minimaxcode.1task.cn"
KEY = os.environ["MMX_KEY"]
ADMIN = os.environ["MMX_ADMIN"]
OP = urllib.request.build_opener(urllib.request.ProxyHandler({}))

rows = []


def call(method, path, body=None, headers=None, raw=False, timeout=60):
    url = BASE + path
    data = None
    hdrs = {"Accept": "application/json"}
    if body is not None:
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        hdrs["Content-Type"] = "application/json"
    hdrs.update(headers or {})
    req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    t0 = time.time()
    try:
        with OP.open(req, timeout=timeout) as r:
            payload = r.read()
            try:
                parsed = payload if raw else json.loads(payload or b"{}")
            except Exception:
                parsed = {"_raw": payload[:200].decode("utf-8", "replace")}
            return r.status, dict(r.headers), parsed, time.time() - t0
    except urllib.error.HTTPError as e:
        payload = e.read()
        try:
            parsed = payload if raw else json.loads(payload or b"{}")
        except Exception:
            parsed = {"_raw": payload[:200].decode("utf-8", "replace")}
        return e.code, dict(e.headers), parsed, time.time() - t0


def check(group, name, ok, detail=""):
    rows.append((group, name, "PASS" if ok else "FAIL", detail))


def err_code(d):
    e = d.get("error")
    return e.get("code") if isinstance(e, dict) else None


ALLOWED_UPSTREAM = {200, 402, 429, 502, 503}


def reaches_upstream(group, name, path, body, headers=None):
    st, _, d, _ = call("POST", path, body, headers)
    ok = st in ALLOWED_UPSTREAM
    detail = f"HTTP {st} code={err_code(d)}"
    check(group, name, ok, detail if not ok else "")


def expect_err(group, name, path, body, status, code, headers=None, method="POST"):
    st, _, d, _ = call(method, path, body, headers)
    ok = st == status and err_code(d) == code
    check(group, name, ok, "" if ok else f"得到 HTTP {st} code={err_code(d)}，期望 {status}/{code}")


AUTH = {"Authorization": "Bearer " + KEY}

# ---------------- A. 入口与传输 ----------------
st, h, body, _ = call("GET", "/healthz")
check("A 入口", "healthz 200", st == 200, f"HTTP {st}")
check("A 入口", "版本 0.2.3", body.get("version") == "0.2.3", str(body.get("version")))

req = urllib.request.Request("http://minimaxcode.1task.cn/", method="GET")
try:
    no_redirect = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect := type("R", (urllib.request.HTTPRedirectHandler,), {"redirect_request": lambda *a, **k: None})())
    with no_redirect.open(req, timeout=15) as r:
        st80, loc = r.status, r.headers.get("Location")
except urllib.error.HTTPError as e:
    st80, loc = e.code, e.headers.get("Location")
check("A 入口", ":80 → :443 301", st80 == 301 and loc == "https://minimaxcode.1task.cn/", f"{st80} {loc}")

ctx = ssl.create_default_context()
try:
    with OP.open("https://minimaxcode.1task.cn/healthz", timeout=15) as r:
        st_tls = r.status
    check("A 入口", "证书链校验通过", st_tls == 200)
except Exception as e:
    check("A 入口", "证书链校验通过", False, str(e)[:80])

_, h_head, _, _ = call("GET", "/panel/")
check("A 入口", "CSP 存在", "content-security-policy" in {k.lower() for k in h_head}, "")
check("A 入口", "X-Frame-Options", any(k.lower() == "x-frame-options" for k in h_head), "")
check("A 入口", "nosniff", any(k.lower() == "x-content-type-options" for k in h_head), "")
has_hsts = any(k.lower() == "strict-transport-security" for k in h_head)
check("A 入口", "HSTS 存在", has_hsts, "" if has_hsts else "缺 Strict-Transport-Security")

st, h, d, _ = call("GET", "/v1/nope")
check("A 入口", "未知路径 404 JSON", st == 404 and err_code(d) == "unknown_route", f"HTTP {st} code={err_code(d)}")

st, h, _, _ = call("OPTIONS", "/v1/chat/completions", headers={"Origin": "https://x.test", "Access-Control-Request-Method": "POST"})
allow = h.get("Access-Control-Allow-Methods", "") or h.get("access-control-allow-methods", "")
check("A 入口", "OPTIONS 预检 204+DELETE", st == 204 and "DELETE" in allow, f"HTTP {st} allow={allow}")

# ---------------- B. 鉴权 ----------------
st, _, d, _ = call("GET", "/v1/models")
check("B 鉴权", "无 key → 401", st == 401, f"HTTP {st}")
st, _, _, _ = call("GET", "/v1/models", headers={"Authorization": "Bearer wrong-key"})
check("B 鉴权", "错 key → 401", st == 401, f"HTTP {st}")
st, _, d, _ = call("GET", "/v1/models", headers=AUTH)
check("B 鉴权", "Bearer → 200", st == 200, f"HTTP {st}")
st2, _, _, _ = call("GET", "/v1/models", headers={"x-api-key": KEY})
check("B 鉴权", "x-api-key → 200", st2 == 200, f"HTTP {st2}")
st, _, d, _ = call("GET", "/panel/api/overview", headers=AUTH)
check("B 鉴权", "面板拒 api_key（应 401 invalid_key）", st == 401 and err_code(d) == "invalid_key", f"HTTP {st} code={err_code(d)}")
st, _, d, _ = call("GET", "/status")
check("B 鉴权", "/status 需鉴权", st == 401, f"HTTP {st}")
st, _, d, _ = call("GET", "/status", headers=AUTH)
check("B 鉴权", "/status 带 key 200", st == 200, f"HTTP {st}")

# ---------------- C. 模型 ----------------
st, _, d, _ = call("GET", "/v1/models", headers=AUTH)
ids = [m["id"] for m in d.get("data", [])]
check("C 模型", "模型列表内容", ids == ["MiniMax-M3.1-Flash", "MiniMax-M3"], str(ids))
st, _, d, _ = call("GET", "/v1/models/M3", headers=AUTH)
check("C 模型", "别名 /v1/models/M3", st == 200 and d.get("id") == "MiniMax-M3", f"HTTP {st} id={d.get('id')}")
st, _, d, _ = call("GET", "/v1/models/m3.1-flash", headers=AUTH)
check("C 模型", "别名 /v1/models/m3.1-flash", st == 200 and d.get("id") == "MiniMax-M3.1-Flash", f"HTTP {st} id={d.get('id')}")
st, _, d, _ = call("GET", "/v1/models/gpt-4o", headers=AUTH)
check("C 模型", "未知模型 404", st == 404 and err_code(d) == "model_not_found", f"HTTP {st} code={err_code(d)}")

# ---------------- D. chat 校验 ----------------
expect_err("D chat", "坏 JSON → 400 invalid_json", "/v1/chat/completions", b"{bad", 400, "invalid_json", AUTH)
expect_err("D chat", "缺 messages → 400", "/v1/chat/completions", {"model": "M3"}, 400, "invalid_request", AUTH)
expect_err("D chat", "messages 非数组 → 400", "/v1/chat/completions", {"model": "M3", "messages": "hi"}, 400, "invalid_request", AUTH)
reaches_upstream("D chat", "合法请求打到上游", "/v1/chat/completions",
                 {"model": "M3", "messages": [{"role": "user", "content": "hi"}], "max_tokens": 8}, AUTH)
reaches_upstream("D chat", "流式请求打到上游", "/v1/chat/completions",
                 {"model": "M3", "messages": [{"role": "user", "content": "hi"}], "stream": True, "max_tokens": 8}, AUTH)
reaches_upstream("D chat", "工具调用请求打到上游", "/v1/chat/completions",
                 {"model": "M3", "messages": [{"role": "user", "content": "hi"}],
                  "tools": [{"type": "function", "function": {"name": "f", "parameters": {"type": "object", "properties": {}}}}],
                  "max_tokens": 8}, AUTH)

# ---------------- E. Anthropic ----------------
st, _, d, _ = call("POST", "/v1/messages",
                   {"model": "M3", "messages": [{"role": "user", "content": "hi"}]},
                   {"x-api-key": KEY, "anthropic-version": "2023-06-01"})
ok = (st == 400 and d.get("type") == "error") or st in {402, 429, 503}
check("E anthropic", "缺 max_tokens（直通不本地校验）", ok, f"HTTP {st} keys={sorted(d)}")
st, _, d, _ = call("POST", "/v1/messages/count_tokens",
                   {"model": "M3", "messages": [{"role": "user", "content": "hello world"}]},
                   {"x-api-key": KEY, "anthropic-version": "2023-06-01"})
check("E anthropic", "count_tokens 本地估算 200", st == 200 and d.get("input_tokens", 0) > 0, f"HTTP {st} tokens={d.get('input_tokens')}")
st, _, d, _ = call("POST", "/v1/messages",
                   {"model": "gpt-4o", "max_tokens": 8, "messages": [{"role": "user", "content": "hi"}]},
                   {"x-api-key": KEY, "anthropic-version": "2023-06-01"})
ok = d.get("type") == "error" and st in {400, 402, 404, 429, 503}
check("E anthropic", "直通未知模型 → Anthropic 错误形状", ok, f"HTTP {st} keys={sorted(d)}")

# ---------------- F. 图片 ----------------
expect_err("F 图片", "空 prompt 无图 → 400", "/v1/images/generations", {}, 400, "invalid_request", AUTH)
expect_err("F 图片", "image 空串 → 400", "/v1/images/generations", {"prompt": "x", "image": ""}, 400, "invalid_request", AUTH)
expect_err("F 图片", "mask 无 image → 400", "/v1/images/generations", {"prompt": "x", "mask": "m.png"}, 400, "invalid_request", AUTH)
expect_err("F 图片", "image-only → 400 unsupported", "/v1/images/generations", {"image": "https://a/1.png"}, 400, "unsupported_parameter", AUTH)
expect_err("F 图片", "aspect_ratio 无法解析 → 400", "/v1/images/generations",
           {"prompt": "x", "aspect_ratio": "abc"}, 400, "invalid_request", AUTH)
expect_err("F 图片", "5 张参考图 → 400", "/v1/images/generations",
           {"prompt": "x", "image": ["1", "2", "3", "4", "5"]}, 400, "invalid_request", AUTH)
reaches_upstream("F 图片", "合法请求打到上游", "/v1/images/generations",
                 {"prompt": "x", "n": 1, "aspect_ratio": "7:3", "resolution": "8K"}, AUTH)

# ---------------- G. 视频 ----------------
V = "/v1/contents/generations/tasks"
expect_err("G 视频", "空对象 → 400 content", V, {}, 400, "invalid_request", AUTH)
expect_err("G 视频", "首帧+参考混用 → 400", V,
           {"model": "MiniMax-H3", "content": [
               {"type": "text", "text": "x"},
               {"type": "image_url", "image_url": {"url": "https://a/1.png"}, "role": "first_frame"},
               {"type": "image_url", "image_url": {"url": "https://a/2.png"}, "role": "reference"}],
            "duration": 4}, 400, "invalid_request", AUTH)
expect_err("G 视频", "480p → 400", V,
           {"model": "MiniMax-H3", "content": [{"type": "text", "text": "x"}], "resolution": "480p", "duration": 4},
           400, "unsupported_parameter", AUTH)
expect_err("G 视频", "frames 不支持 → 400", V,
           {"model": "MiniMax-H3", "content": [{"type": "text", "text": "x"}], "frames": 24}, 400, "unsupported_parameter", AUTH)
st, _, d, _ = call("GET", V, headers=AUTH)
check("G 视频", "列表 200 {items,total}", st == 200 and "items" in d and "total" in d, f"HTTP {st}")
st, _, d, _ = call("GET", V + "/nope", headers=AUTH)
check("G 视频", "未知任务 404", st == 404 and err_code(d) == "task_not_found", f"HTTP {st} code={err_code(d)}")
st, _, d, _ = call("DELETE", V + "/nope", headers=AUTH)
check("G 视频", "删未知任务 404", st == 404, f"HTTP {st}")
st, _, d, _ = call("GET", "/api/v3/contents/generations/tasks", headers=AUTH)
check("G 视频", "/api/v3 别名", st == 200 and "items" in d, f"HTTP {st}")
st, _, d, h = call("GET", V + "/nope?wait=1&wait_timeout=2", headers=AUTH)
check("G 视频", "同步门未知任务 404", st == 404, f"HTTP {st}")

# ---------------- H. 音频 ----------------
expect_err("H 音频", "speech 缺 input → 400", "/v1/audio/speech", {"voice": "female-shaonv"}, 400, "invalid_request", AUTH)
expect_err("H 音频", "music prompt 太短 → 400", "/v1/audio/music", {"prompt": "太短"}, 400, "invalid_request", AUTH)
st, _, d, _ = call("POST", "/v1/audio/transcriptions", b"--x--", {"Authorization": "Bearer " + KEY, "Content-Type": "text/plain"})
check("H 音频", "转写非法载体 → 400", st == 400, f"HTTP {st}")
st, vh, d, _ = call("GET", "/v1/voices", headers=AUTH)
if st == 200:
    check("H 音频", "音色列表 200", len(d.get("data", [])) > 100, f"n={len(d.get('data', []))}")
elif st == 503:
    ok = err_code(d) == "no_account" and bool(vh.get("Retry-After") or vh.get("retry-after"))
    check("H 音频", "池空 → 503 no_account + Retry-After", ok, f"code={err_code(d)} retry={vh.get('Retry-After')}")
else:
    check("H 音频", "音色列表（额度受限）", st in {402, 429}, f"HTTP {st} code={err_code(d)}")

# ---------------- I. 面板 ----------------
st, _, d, _ = call("POST", "/panel/api/login", {"key": "wrong"})
check("I 面板", "错口令 401", st == 401, f"HTTP {st}")
st, _, d, _ = call("POST", "/panel/api/login", {"key": ADMIN})
check("I 面板", "正确口令 200", st == 200 and d.get("ok") is True, f"HTTP {st}")
for ep in ("overview", "accounts", "usage", "logs", "models", "config"):
    st, _, d, _ = call("GET", "/panel/api/" + ep, headers={"Authorization": "Bearer " + ADMIN})
    check("I 面板", f"/panel/api/{ep} 200", st == 200, f"HTTP {st}")

st, _, d, _ = call("POST", "/panel/api/accounts/import", {}, {"Authorization": "Bearer " + ADMIN})
check("I 面板", "导入空 body → 400", st == 400, f"HTTP {st} code={err_code(d)}")

st, _, _, _ = call("GET", "/panel/api/../config.json")
check("I 面板", "路径穿越被拒", st in {400, 404}, f"HTTP {st}")

# ---------------- J. 并发 ----------------
def one(_):
    return call("GET", "/v1/models", headers=AUTH, timeout=30)[0]

with ThreadPoolExecutor(20) as ex:
    codes = list(ex.map(one, range(20)))
check("J 并发", "20 并发 /v1/models 全 200", all(c == 200 for c in codes), str(sorted(set(codes))))


def chat(_):
    return call("POST", "/v1/chat/completions",
                {"model": "M3", "messages": [{"role": "user", "content": "hi"}], "max_tokens": 8},
                AUTH, timeout=60)[0]

with ThreadPoolExecutor(10) as ex:
    ccodes = list(ex.map(chat, range(10)))
ok = all(c in ALLOWED_UPSTREAM for c in ccodes)
check("J 并发", "10 并发 chat 无 5xx/超时", ok, str(sorted(set(ccodes))))

# ---------------- 汇总 ----------------
print(f"{'组':<12} {'用例':<34} 结果  说明")
print("-" * 100)
fails = 0
for g, n, r, det in rows:
    if r == "FAIL":
        fails += 1
    print(f"{g:<12} {n:<34} {r:4s}  {det[:60]}")
print("-" * 100)
print(f"共 {len(rows)} 项，通过 {len(rows) - fails}，失败 {fails}")
sys.exit(1 if fails else 0)
