FROM golang:1.25.12-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/maxpilot ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
    && addgroup -S maxpilot && adduser -S -G maxpilot maxpilot \
    && mkdir -p /app/data /app/media \
    && chown -R maxpilot:maxpilot /app
WORKDIR /app
COPY --from=build /out/maxpilot /usr/local/bin/maxpilot
USER maxpilot
ENV HOST=0.0.0.0
EXPOSE 8080
VOLUME ["/app/data", "/app/media"]
ENTRYPOINT ["/usr/local/bin/maxpilot"]
