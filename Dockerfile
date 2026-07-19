FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/maxpilot ./cmd/server \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/setup-max-webhook ./cmd/setup-max-webhook

FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40
RUN apk add --no-cache ca-certificates \
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
