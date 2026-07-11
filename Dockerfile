# Stage 1: 构建阶段
ARG GO_BUILD_FLAGS='-ldflags="-s -w" -trimpath'

FROM golang:1.25-alpine AS builder

ENV GOPROXY=https://goproxy.cn,direct

# 设置工作目录
WORKDIR /build

# 复制 go.mod 和 go.sum 并下载依赖
COPY go.mod go.sum ./
RUN go mod download

# 复制源代码
COPY . .

# 构建应用
RUN CGO_ENABLED=0 go build ${GO_BUILD_FLAGS} -o /out/hubp ./cmd/hubp


# Stage 2: 运行阶段
FROM alpine:3.23

# （可选）切换至清华镜像源加速下载
RUN sed -i 's#https\?://dl-cdn.alpinelinux.org/alpine#https://mirrors.tuna.tsinghua.edu.cn/alpine#g' /etc/apk/repositories

RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -G app app \
    && mkdir -p /var/lib/hubp/cache \
    && chown -R app:app /var/lib/hubp

COPY --from=builder /out/hubp /usr/local/bin/hubp
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/hubp"]
CMD ["-config", "/etc/hubp/config.json"]
