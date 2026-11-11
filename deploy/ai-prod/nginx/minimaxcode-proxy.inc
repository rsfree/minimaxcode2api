# minimaxcode2api 反代（回环 127.0.0.1:39350）
proxy_pass http://127.0.0.1:39350;
proxy_http_version 1.1;
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;

# /v1/chat/completions 与 /v1/messages 都是 SSE 流式 ⇒ 必须关缓冲，
# 否则 token 被攒成一坨一次性吐出，客户端看起来像"卡住然后突然全出"。
proxy_buffering off;
proxy_cache off;
proxy_request_buffering off;

# 长连接超时：
#  - 图片 4K 单张实测 43s、批量 2 张 109s；
#  - 视频同步折叠 ?wait=1&wait_timeout=300 最多在服务端等 300s；
#  - 上游偶发慢首包。
# 读超时给到 660s（> 300s 折叠预算 + 余量），别让 nginx 把长请求截成 504。
proxy_read_timeout 660s;
proxy_send_timeout 660s;
proxy_connect_timeout 15s;

# 图生图可以带多张参考图（base64 内联），ASR 也要传音频字节 ⇒ 放宽到 64m
client_max_body_size 64m;

# 清掉客户端带来的 Connection 头（可能是 close / 非法的 keep-alive 值），
# 避免它被原样转给上游导致连接行为不受控
proxy_set_header Connection "";

# 媒体接口的产物是二进制（mp3 / mp4），别让 nginx 改写 Content-Type
proxy_pass_header Content-Type;
proxy_pass_header Content-Length;
