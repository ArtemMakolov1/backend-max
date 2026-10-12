# syntax=docker/dockerfile:1.7

# Both upstream manifests have the same Grafana core build. Only the configured
# Prometheus datasource is required for this monitoring stack.
FROM grafana/grafana:nightly@sha256:50afcb1c55336041015d887c7c3039de7aaef56b2d795232030a72d397d384b0 AS plugins

FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine3.23@sha256:9e45f0eb4a63ed37ad6e604950407558b7c738e34e015ae77a5d4ad369cde079 AS plugin-build

ARG SOURCE_COMMIT=6d21418acde6ce31c9f7a6418a88758842a881bb
ARG SOURCE_SHA256=24b1f2ed395438a5574102b166314be621259431fbae3f04ae66b55a143f1c13
ENV GOTOOLCHAIN=local
WORKDIR /src
RUN wget -q "https://github.com/grafana/grafana-prometheus-datasource/archive/${SOURCE_COMMIT}.tar.gz" -O /tmp/source.tar.gz \
    && echo "${SOURCE_SHA256}  /tmp/source.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/source.tar.gz --strip-components=1 \
    && rm /tmp/source.tar.gz
# The official slim core is patched, but its bundled datasource binary still
# contains affected HTTP/2 code. Rebuild only the configured Prometheus backend.
# promlib is a local module replacement and needs the same dependency update.
RUN go -C pkg/promlib get golang.org/x/net@v0.60.0 \
    && go -C pkg/promlib mod tidy \
    && go -C pkg/promlib mod verify \
    && go get golang.org/x/net@v0.60.0 \
    && go mod tidy \
    && go mod download \
    && go mod verify \
    && go test ./pkg/... \
    && go -C pkg/promlib test ./...
ARG TARGETARCH
ARG TARGETOS
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags="-s -w" \
      -o "/out/gpx_grafana-prometheus-datasource_${TARGETOS}_${TARGETARCH}" ./pkg

FROM grafana/grafana:nightly-slim@sha256:e377ff7bc5b53a495201ad3e2ae5ec4a9e6593397c502c330359b4a5acc6fb6c

USER root
COPY --from=plugins --chown=root:root --chmod=0755 \
    /usr/share/grafana/data/plugins-bundled/prometheus \
    /usr/share/grafana/public/app/plugins/datasource/prometheus/dist
COPY --from=plugins --chown=root:root --chmod=0755 \
    /usr/share/grafana/data/plugins-bundled/prometheus \
    /usr/share/grafana/public/plugins/prometheus
# This distribution compiles Prometheus as an internal core datasource from
# checksum-verified official source. Core discovery uses the datasource tree;
# its dist layout selects the public/plugins module URL for standalone assets.
# Keep the signed catalog copy out of external discovery, and install only the
# newly compiled backend. No unsigned-plugin or development-mode exception.
RUN rm -f /usr/share/grafana/public/app/plugins/datasource/prometheus/dist/gpx_grafana-prometheus-datasource_* \
    && rm -f /usr/share/grafana/public/plugins/prometheus/gpx_grafana-prometheus-datasource_* \
    && rm -rf /usr/share/grafana/data/plugins-bundled/prometheus \
    && chown root:root /usr/share/grafana/data/plugins-bundled \
    && chmod 0755 /usr/share/grafana/data/plugins-bundled \
    && mkdir -p /usr/share/grafana/empty-plugins \
    && chmod 0755 /usr/share/grafana/empty-plugins
COPY --from=plugin-build --chown=root:root --chmod=0755 \
    /out/ /usr/share/grafana/public/app/plugins/datasource/prometheus/dist/
USER 472

# Resolve all plugin content at build time. Production's internal monitoring
# network must not depend on mutable downloads or background plugin updates.
ENV GF_PLUGINS_PREINSTALL_DISABLED=true \
    GF_PLUGINS_PREINSTALL_AUTO_UPDATE=false \
    GF_PLUGINS_PLUGIN_ADMIN_ENABLED=false \
    GF_PATHS_PLUGINS=/usr/share/grafana/empty-plugins

LABEL org.opencontainers.image.title="MaxPosty Grafana with source-built internal Prometheus datasource"
