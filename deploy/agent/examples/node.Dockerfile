FROM node:22-alpine
WORKDIR /app
COPY package*.json ./
RUN npm ci --omit=dev
COPY . .
COPY --from=registry.plusclouds.com/monitoring-agent:latest /agent /usr/local/bin/agent
# /healthz and /metrics (prom-client collectDefaultMetrics) are served by the app.
ENV AGENT_COLLECTORS=host,nodejs \
    AGENT_NODEJS_URL=http://127.0.0.1:3000/healthz \
    AGENT_NODEJS_METRICS_URL=http://127.0.0.1:3000/metrics
ENTRYPOINT ["/usr/local/bin/agent", "run", "--"]
CMD ["node", "server.js"]
