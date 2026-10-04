# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine3.23@sha256:a8fa79c5bd40d880b52bd3b6d7669ecdcfd00e85facdd427d279efb5ddd79cb1 AS build

ARG SOURCE_COMMIT=73c6bfe7393929211294c1954f30d8ed78e4d0ad
ARG SOURCE_SHA256=5c5c72ec6b4c4746d523f2ed1994dfec4b94f7457e530ee6fb16cfec158e545e
ARG UI_SHA256=881fe0f7beb9573f35be47dd233af6220e72c8db72c03bcd1780402eb8dc1307

WORKDIR /src
RUN wget -q "https://github.com/prometheus/alertmanager/archive/${SOURCE_COMMIT}.tar.gz" -O /tmp/source.tar.gz \
    && echo "${SOURCE_SHA256}  /tmp/source.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/source.tar.gz --strip-components=1 \
    && rm /tmp/source.tar.gz
# Upstream publishes these exact release assets separately from the source.
# Keep the UI while rebuilding both vulnerable Go binaries from source.
RUN wget -q "https://github.com/prometheus/alertmanager/releases/download/v0.34.1/alertmanager-web-ui-0.34.1.tar.gz" -O /tmp/ui.tar.gz \
    && echo "${UI_SHA256}  /tmp/ui.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/ui.tar.gz -C ui/app \
    && rm /tmp/ui.tar.gz

# Force the regression's contenders to load the destroyed group before any CAS.
# Only the upstream test fixture changes; all recovery/delivery assertions and
# production source remain intact. Verify the exact upstream file before patching.
COPY docker/monitoring/alertmanager-cas-test.patch /tmp/alertmanager-cas-test.patch
RUN apk add --no-cache patch \
    && echo "ef140355e47e73401552e329a0df5df9fa1caf13bdfde071eaa52032645ced6d  dispatch/dispatch_test.go" | sha256sum -c - \
    && patch --batch --fuzz=0 -p1 < /tmp/alertmanager-cas-test.patch \
    && rm /tmp/alertmanager-cas-test.patch

# The latest release still embeds affected gRPC in both alertmanager and amtool.
# Keep this native test stage shared by both target architectures. The upstream
# regression checks the contention metric only when GOMAXPROCS is above one.
# Set it explicitly for tests without changing the production runtime setting.
RUN go get google.golang.org/grpc@v1.83.2 \
    && go mod tidy \
    && go mod download \
    && go mod verify \
    && go build -trimpath \
      -ldflags="-s -w -X github.com/prometheus/common/version.Version=0.34.1-maxposty.2 -X github.com/prometheus/common/version.Revision=${SOURCE_COMMIT} -X github.com/prometheus/common/version.Branch=v0.34.1" \
      -o ./ ./cmd/alertmanager ./cmd/amtool \
    && GOMAXPROCS=4 go test -count=1 ./...

ARG TARGETARCH
ARG TARGETOS
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/prometheus/common/version.Version=0.34.1-maxposty.2 -X github.com/prometheus/common/version.Revision=${SOURCE_COMMIT} -X github.com/prometheus/common/version.Branch=v0.34.1" \
      -o /out/ ./cmd/alertmanager ./cmd/amtool \
    && mkdir -p /out/runtime/alertmanager \
    && chmod g+w /out/runtime/alertmanager

FROM busybox:1.37.0-uclibc@sha256:39e0df8c4d65953b55c344f017e1ff2e0031a7454b3c24e6b76d402f207e315a

LABEL org.opencontainers.image.source="https://github.com/prometheus/alertmanager" \
      org.opencontainers.image.version="0.34.1-maxposty.2" \
      org.opencontainers.image.revision="73c6bfe7393929211294c1954f30d8ed78e4d0ad"

COPY --from=build /out/alertmanager /out/amtool /bin/
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=nobody:nobody /src/examples/ha/alertmanager.yml /etc/alertmanager/alertmanager.yml
COPY --from=build --chown=nobody:nobody /out/runtime/ /

EXPOSE 9093
USER nobody
WORKDIR /alertmanager
ENTRYPOINT ["/bin/alertmanager"]
CMD ["--config.file=/etc/alertmanager/alertmanager.yml", "--storage.path=/alertmanager"]
