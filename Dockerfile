FROM golang:1.27-trixie AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o chalk .

FROM debian:trixie-slim
RUN useradd --uid 1000 --user-group --create-home --shell /usr/sbin/nologin chalk \
    && mkdir -p /data \
    && chown chalk:chalk /data
USER chalk
COPY --from=builder /build/chalk /usr/local/bin/chalk
EXPOSE 8080
VOLUME ["/data"]
ENV BLACKBOARD_DB=/data/board.db
ENV BLACKBOARD_HOST=0.0.0.0
ENTRYPOINT ["chalk"]
