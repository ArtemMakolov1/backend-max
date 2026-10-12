# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine3.23@sha256:9e45f0eb4a63ed37ad6e604950407558b7c738e34e015ae77a5d4ad369cde079 AS build

ARG SOURCE_COMMIT=5241a27fe3c6983549fccc32f6e65917408c63cd
ARG SOURCE_SHA256=eee7ec029ffdba7d9ee59171cad5ddaff10e71514b0bae1a6fde6dc1ae99e181
ARG UI_SHA256=fa8c918ed05e3f89e232e9b69eccd6ed074602cd9e8a2cab7a92bec9651974ae
ENV GOTOOLCHAIN=local

WORKDIR /src
RUN apk add --no-cache bash gzip \
    && wget -q "https://github.com/prometheus/prometheus/archive/${SOURCE_COMMIT}.tar.gz" -O /tmp/source.tar.gz \
    && echo "${SOURCE_SHA256}  /tmp/source.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/source.tar.gz --strip-components=1 \
    && rm /tmp/source.tar.gz
# Reuse the checksum-verified release UI while recompiling both Go binaries.
RUN wget -q "https://github.com/prometheus/prometheus/releases/download/v3.15.0/prometheus-web-ui-3.15.0.tar.gz" -O /tmp/ui.tar.gz \
    && echo "${UI_SHA256}  /tmp/ui.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/ui.tar.gz -C web/ui \
    && rm /tmp/ui.tar.gz \
    && bash scripts/compress_assets.sh
RUN go get golang.org/x/net@v0.60.0 \
    && go mod tidy \
    && go mod download \
    && go mod verify \
    && go build -tags netgo,builtinassets -trimpath -o ./ ./cmd/prometheus ./cmd/promtool \
    && go test -tags netgo,builtinassets ./...

ARG TARGETARCH
ARG TARGETOS
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -tags netgo,builtinassets -trimpath \
      -ldflags="-s -w -X github.com/prometheus/common/version.Version=3.15.0-maxposty.1 -X github.com/prometheus/common/version.Revision=${SOURCE_COMMIT} -X github.com/prometheus/common/version.Branch=v3.15.0" \
      -o /out/ ./cmd/prometheus ./cmd/promtool \
    && mkdir -p /out/runtime/prometheus \
    && chmod g+w /out/runtime/prometheus

FROM busybox:1.37.0-uclibc@sha256:39e0df8c4d65953b55c344f017e1ff2e0031a7454b3c24e6b76d402f207e315a

LABEL org.opencontainers.image.source="https://github.com/prometheus/prometheus" \
      org.opencontainers.image.version="3.15.0-maxposty.1" \
      org.opencontainers.image.revision="5241a27fe3c6983549fccc32f6e65917408c63cd"

COPY --from=build /out/prometheus /out/promtool /bin/
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=nobody:nobody /src/documentation/examples/prometheus.yml /etc/prometheus/prometheus.yml
COPY --from=build /src/LICENSE /src/NOTICE /
COPY --from=build --chown=nobody:nobody /out/runtime/ /

EXPOSE 9090
USER nobody
WORKDIR /prometheus
ENTRYPOINT ["/bin/prometheus"]
CMD ["--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus"]
