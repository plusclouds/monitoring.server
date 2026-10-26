# Any image: copy the agent in, make it the entrypoint, keep the original command.
FROM nginx:1.27-alpine
COPY --from=registry.plusclouds.com/monitoring-agent:latest /agent /usr/local/bin/agent
# stub_status for the nginx collector (listen on loopback only).
RUN printf 'server { listen 127.0.0.1:8081; location /nginx_status { stub_status; } }\n' > /etc/nginx/conf.d/status.conf
ENV AGENT_COLLECTORS=host,nginx,process \
    AGENT_NGINX_URL=http://127.0.0.1:8081/nginx_status \
    AGENT_PROCESS_MATCH=nginx
# AGENT_URL and AGENT_TOKEN_FILE come from the platform at run time.
ENTRYPOINT ["/usr/local/bin/agent", "run", "--"]
CMD ["nginx", "-g", "daemon off;"]
