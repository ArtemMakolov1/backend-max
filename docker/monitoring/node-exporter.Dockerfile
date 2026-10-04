# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine3.23@sha256:a8fa79c5bd40d880b52bd3b6d7669ecdcfd00e85facdd427d279efb5ddd79cb1 AS build

ARG TARGETARCH
ARG TARGETOS
ARG SOURCE_COMMIT=6044da783597cc3b57aef7580ddcdcff58a4ee99
ARG SOURCE_SHA256=cfec7478aa9bfd011f29df084584b7c7b68dfef85a24d28e9d6d5f88224f83c3

WORKDIR /src
RUN apk add --no-cache bash sed
RUN wget -q "https://github.com/prometheus/node_exporter/archive/${SOURCE_COMMIT}.tar.gz" -O /tmp/source.tar.gz \
    && echo "${SOURCE_SHA256}  /tmp/source.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/source.tar.gz --strip-components=1 \
    && rm /tmp/source.tar.gz
# The latest upstream release still embeds an affected Go standard library and
# x/crypto. Apply the crypto fix and its compatible dependencies without
# downgrading the newer upstream x/net version.
RUN go get golang.org/x/crypto@v0.55.0 \
    && go mod tidy \
    && go mod download \
    && go mod verify \
    && ./ttar -C collector/fixtures -x -f collector/fixtures/sys.ttar \
    && ./ttar -C collector/fixtures -x -f collector/fixtures/udev.ttar \
    && go test ./...
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/prometheus/common/version.Version=1.12.1-maxposty.1 -X github.com/prometheus/common/version.Revision=${SOURCE_COMMIT} -X github.com/prometheus/common/version.Branch=v1.12.1" \
      -o /out/node_exporter .

FROM busybox:1.37.0-uclibc@sha256:39e0df8c4d65953b55c344f017e1ff2e0031a7454b3c24e6b76d402f207e315a

LABEL org.opencontainers.image.source="https://github.com/prometheus/node_exporter" \
      org.opencontainers.image.version="1.12.1-maxposty.1" \
      org.opencontainers.image.revision="6044da783597cc3b57aef7580ddcdcff58a4ee99"

COPY --from=build /out/node_exporter /bin/node_exporter

EXPOSE 9100
USER nobody
ENTRYPOINT ["/bin/node_exporter"]
