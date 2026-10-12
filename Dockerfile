FROM golang:1.26.9-alpine@sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/maxpilot ./cmd/server \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/setup-max-webhook ./cmd/setup-max-webhook

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
# The pinned base predates the OpenSSL QUIC security fix. Require patched
# runtime libraries from this Alpine branch rather than retaining the base's versions.
RUN apk add --no-cache ca-certificates ffmpeg 'libcrypto3>=3.5.8-r0' 'libssl3>=3.5.8-r0' \
    && addgroup -S maxpilot \
    && adduser -S -G maxpilot maxpilot \
    && mkdir -p /app/media \
    && chown -R maxpilot:maxpilot /app

WORKDIR /app
COPY --from=build --chown=maxpilot:maxpilot /out/maxpilot /usr/local/bin/maxpilot
COPY --from=build --chown=maxpilot:maxpilot /out/migrate /app/migrate
COPY --from=build --chown=maxpilot:maxpilot /out/setup-max-webhook /app/setup-max-webhook

USER maxpilot
ENV HOST=0.0.0.0
EXPOSE 8080
VOLUME ["/app/media"]
ENTRYPOINT ["/usr/local/bin/maxpilot"]
