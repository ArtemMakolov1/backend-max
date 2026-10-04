# syntax=docker/dockerfile:1.7

# Both upstream manifests have the same Grafana core build. Only the configured
# Prometheus datasource is required for this monitoring stack.
FROM grafana/grafana:nightly@sha256:2f44cf7323832a564c2cea01855d851d0772ed00b60cbcc812d47cdee3c740f1 AS plugins

FROM grafana/grafana:nightly-slim@sha256:5c1a2935a24ddfc328cb387939ae669ad1405338d38596b7ef55bc36ec90f2e8

USER root
COPY --from=plugins --chown=root:root --chmod=0755 \
    /usr/share/grafana/data/plugins-bundled/prometheus \
    /usr/share/grafana/data/plugins-bundled/prometheus
RUN chown root:root /usr/share/grafana/data/plugins-bundled \
    && chmod 0755 /usr/share/grafana/data/plugins-bundled \
    && mkdir -p /usr/share/grafana/empty-plugins \
    && chmod 0755 /usr/share/grafana/empty-plugins
USER 472

# Resolve all plugin content at build time. Production's internal monitoring
# network must not depend on mutable downloads or background plugin updates.
ENV GF_PLUGINS_PREINSTALL_DISABLED=true \
    GF_PLUGINS_PREINSTALL_AUTO_UPDATE=false \
    GF_PLUGINS_PLUGIN_ADMIN_ENABLED=false \
    GF_PATHS_PLUGINS=/usr/share/grafana/empty-plugins

LABEL org.opencontainers.image.title="MaxPosty Grafana with pinned Prometheus datasource"
