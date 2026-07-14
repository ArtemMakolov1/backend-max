# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine3.23@sha256:622e56dbc11a8cfe87cafa2331e9a201877271cbff918af53d3be315f3da88cc AS build

ARG TARGETARCH
ARG TARGETOS
ARG SOURCE_COMMIT=2a70ffdb35b6fbd3413ac5abf07c4ddf6dde3067
ARG SOURCE_SHA256=db1009b30eadf7ecfb73d8a9213460799da5e4edc7b36b2a981255550bc42781

WORKDIR /src
RUN wget -q "https://github.com/prometheus-community/pgbouncer_exporter/archive/${SOURCE_COMMIT}.tar.gz" -O /tmp/source.tar.gz \
    && echo "${SOURCE_SHA256}  /tmp/source.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/source.tar.gz --strip-components=1 \
    && rm /tmp/source.tar.gz
# The upstream release predates the coordinated Go/x-crypto security update.
# Keep the source tag fixed and override only the vulnerable dependency.
RUN go get golang.org/x/crypto@v0.52.0 \
    && go mod tidy \
    && go mod download \
    && go mod verify \
    && go test ./...
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/prometheus/common/version.Version=0.12.1-maxposty.1 -X github.com/prometheus/common/version.Revision=${SOURCE_COMMIT} -X github.com/prometheus/common/version.Branch=v0.12.1" \
      -o /out/pgbouncer_exporter .

FROM busybox:1.37.0-uclibc@sha256:39e0df8c4d65953b55c344f017e1ff2e0031a7454b3c24e6b76d402f207e315a

LABEL org.opencontainers.image.source="https://github.com/prometheus-community/pgbouncer_exporter" \
      org.opencontainers.image.version="0.12.1-maxposty.1" \
      org.opencontainers.image.revision="2a70ffdb35b6fbd3413ac5abf07c4ddf6dde3067"

COPY --from=build /out/pgbouncer_exporter /bin/pgbouncer_exporter

EXPOSE 9127
USER nobody
ENTRYPOINT ["/bin/pgbouncer_exporter"]
