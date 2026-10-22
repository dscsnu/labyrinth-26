# Labyrinth's Caddy: the official image with our Caddyfile baked in, so the
# VM runs it from the registry with no files of its own. The hosts and ACME
# email it needs are environment variables (Secret Manager → .env).
FROM caddy:2.11.7-alpine
COPY Caddyfile /etc/caddy/Caddyfile
