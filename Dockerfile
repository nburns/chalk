FROM golang:1.27-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o chalk .

FROM alpine:3.21
RUN adduser -D -u 1000 chalk \
    && mkdir -p /data \
    && chown chalk:chalk /data
USER chalk
COPY --from=builder /build/chalk /usr/local/bin/chalk
EXPOSE 8080
VOLUME ["/data"]
ENV BLACKBOARD_DB=/data/board.db
ENV BLACKBOARD_HOST=0.0.0.0
ENTRYPOINT ["chalk"]
