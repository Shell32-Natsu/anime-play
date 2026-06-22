# ---- builder ----
# 多架构构建：builder 固定跑在构建机原生平台（$BUILDPLATFORM），用 Go 交叉编译产出
# 目标平台（$TARGETOS/$TARGETARCH）的二进制，避免在 QEMU 模拟下跑编译。
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/anime-play .

# ---- runtime ----
# 用 alpine 而非 scratch：需要写映射文件、需要时区数据、便于进容器排查
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
    && mkdir -p /data

COPY --from=builder /out/anime-play /usr/local/bin/anime-play

ENV LISTEN_PORT=8080 \
    MAPPING_FILE=/data/mapping.json \
    REFRESH_INTERVAL=30m \
    RAWURL_CACHE_TTL=1h

VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/anime-play"]
