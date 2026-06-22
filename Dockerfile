# ---- builder ----
FROM golang:1.26-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
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
