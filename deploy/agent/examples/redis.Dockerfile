FROM redis:7-alpine
COPY --from=registry.plusclouds.com/monitoring-agent:latest /agent /usr/local/bin/agent
ENV AGENT_COLLECTORS=host,redis
# The image's original entrypoint script becomes the wrapped command.
ENTRYPOINT ["/usr/local/bin/agent", "run", "--", "docker-entrypoint.sh"]
CMD ["redis-server"]
