# 判题镜像：只包含 Go 工具链，编译/运行都在 --network=none 下进行。
# 构建：docker build -f deploy/docker/exam-go.Dockerfile -t exam-go:1.26 .
# 学生工作区会挂到 /src（只读），产物挂到 /out。
FROM golang:1.26-alpine

ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=mod \
    GOTOOLCHAIN=local \
    GOPATH=/go \
    GOCACHE=/tmp/gocache

WORKDIR /src
