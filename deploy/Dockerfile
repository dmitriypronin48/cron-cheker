# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cron-checker-server ./cmd/server \
 && mkdir -p /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/cron-checker-server /usr/local/bin/cron-checker-server
# /data is the SQLite volume: pre-create it owned by the nonroot user (65532),
# since the distroless image has no chown/chmod binaries.
COPY --from=builder --chown=65532:65532 /data /data
USER nonroot
EXPOSE 8090
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/cron-checker-server"]
