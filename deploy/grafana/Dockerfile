# Grafana with the starter dashboards and the PostgreSQL data source baked in
# (F07). Build from this directory:
#   docker build --build-arg VERSION=$(git describe --tags --always) -t monitoring-grafana deploy/grafana
# Settings at run time (see README.md): MONITOR_GRAFANA_DB_HOST,
# MONITOR_GRAFANA_DB_SSLMODE, MONITOR_GRAFANA_PASSWORD.
ARG GRAFANA_VERSION=13.0.2
FROM grafana/grafana-oss:${GRAFANA_VERSION}
ARG VERSION=dev
LABEL org.opencontainers.image.title="monitoring.server Grafana" \
      org.opencontainers.image.description="Grafana with the monitoring.server dashboards and data source" \
      org.opencontainers.image.source="https://github.com/plusclouds/monitoring.server" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
ENV MONITOR_GRAFANA_DB_HOST=postgres:5432 \
    MONITOR_GRAFANA_DB_SSLMODE=disable \
    GF_AUTH_ANONYMOUS_ENABLED=false \
    GF_ANALYTICS_REPORTING_ENABLED=false \
    GF_ANALYTICS_CHECK_FOR_UPDATES=false \
    GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/var/lib/grafana/dashboards/monitor/device-overview.json
COPY --chown=472:0 provisioning /etc/grafana/provisioning
COPY --chown=472:0 dashboards /var/lib/grafana/dashboards/monitor
