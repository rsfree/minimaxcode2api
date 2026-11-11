# 构建阶段
FROM golang:1.25-alpine AS build

WORKDIR /src

# 先只拷依赖清单，命中缓存时省掉整层重编译。
COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
    -o /out/minimaxcode2api ./cmd/server

# 运行阶段
FROM alpine:3.20

# ca-certificates：访问 https 上游；tzdata：面板/日志按本地时区显示。
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 -h /data app

COPY --from=build /out/minimaxcode2api /usr/local/bin/minimaxcode2api

# 单个卷装下 config.json / accounts.json / usage.json，备份一个目录就够。
WORKDIR /data
ENV MMX2A_LISTEN=:7864 \
    MMX2A_DATA_DIR=/data \
    TZ=Asia/Shanghai

RUN chown -R app:app /data
USER app

VOLUME ["/data"]
EXPOSE 7864

ENTRYPOINT ["minimaxcode2api"]
CMD ["-config", "/data/config.json"]
