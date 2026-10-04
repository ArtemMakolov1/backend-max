# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine3.23@sha256:a8fa79c5bd40d880b52bd3b6d7669ecdcfd00e85facdd427d279efb5ddd79cb1 AS build

ARG TARGETARCH
ARG TARGETOS
ARG SOURCE_COMMIT=867fbcac31cd18c143e244190ea9168cca069827
ARG SOURCE_SHA256=68a1bbc1a83ec17996d7d49bbad1457bb4b248e8071ccd497db48e8346564501

WORKDIR /src
RUN wget -q "https://github.com/prometheus-community/postgres_exporter/archive/${SOURCE_COMMIT}.tar.gz" -O /tmp/source.tar.gz \
    && echo "${SOURCE_SHA256}  /tmp/source.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/source.tar.gz --strip-components=1 \
    && rm /tmp/source.tar.gz
# Keep the release source immutable while rebuilding the vulnerable standard
# library and applying the coordinated x/crypto, x/net and x/text fixes.
RUN go get golang.org/x/crypto@v0.55.0 golang.org/x/net@v0.57.0 golang.org/x/text@v0.41.0 \
    && go mod tidy \
    && go mod download \
    && go mod verify \
    && go test ./...
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/prometheus/common/version.Version=0.20.1-maxposty.3 -X github.com/prometheus/common/version.Revision=${SOURCE_COMMIT} -X github.com/prometheus/common/version.Branch=v0.20.1" \
      -o /out/postgres_exporter ./cmd/postgres_exporter

FROM busybox:1.37.0-uclibc@sha256:39e0df8c4d65953b55c344f017e1ff2e0031a7454b3c24e6b76d402f207e315a

LABEL org.opencontainers.image.source="https://github.com/prometheus-community/postgres_exporter" \
      org.opencontainers.image.version="0.20.1-maxposty.3" \
      org.opencontainers.image.revision="867fbcac31cd18c143e244190ea9168cca069827"

COPY --from=build /out/postgres_exporter /bin/postgres_exporter

EXPOSE 9187
USER nobody
ENTRYPOINT ["/bin/postgres_exporter"]
