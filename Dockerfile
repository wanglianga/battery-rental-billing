FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

ENV GOPROXY=https://goproxy.cn,https://mirrors.aliyun.com/goproxy/,direct
ENV GO111MODULE=on
ENV CGO_ENABLED=0

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN GOOS=linux go build -ldflags="-s -w" -o /app/battery-server ./cmd/server

FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai

WORKDIR /app

COPY --from=builder /app/battery-server /app/battery-server
COPY --from=builder /app/.env.example /app/.env

EXPOSE 8080

CMD ["/app/battery-server"]
