FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /labyrinth .

FROM alpine:3.21
# An unprivileged user to run as, with no home or login. The binary and
# config stay root-owned: readable and runnable, never writable by the
# backend. It writes no files (logs go to stdout, state to Postgres), and
# listens above 1024, so it needs no privileges.
RUN apk add --no-cache ca-certificates \
 && addgroup -S -g 10001 labyrinth \
 && adduser -S -D -H -u 10001 -G labyrinth labyrinth
COPY --from=builder /labyrinth /labyrinth
COPY config.toml /config.toml
ENV CONFIG_FILE=/config.toml
EXPOSE 3000
# Numeric, so a policy can tell it's not root without reading /etc/passwd.
USER 10001:10001
ENTRYPOINT ["/labyrinth"]
