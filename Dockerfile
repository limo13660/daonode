# Build go
FROM golang:1.26.1-alpine AS builder
WORKDIR /app
COPY . .
ENV CGO_ENABLED=0 GOEXPERIMENT=jsonv2 GOTOOLCHAIN=local
RUN go mod download
RUN go build -tags with_quic -v -o daonode

# Release
FROM  alpine
# 安装必要的工具包
RUN  apk --update --no-cache add tzdata ca-certificates \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
RUN mkdir /etc/daonode/
COPY --from=builder /app/daonode /usr/local/bin

ENTRYPOINT [ "daonode", "server", "--config", "/etc/daonode/config.json"]
