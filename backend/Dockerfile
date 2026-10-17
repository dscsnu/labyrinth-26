FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /labyrinth .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /labyrinth /labyrinth
COPY config.toml /config.toml
ENV CONFIG_FILE=/config.toml
EXPOSE 3000
ENTRYPOINT ["/labyrinth"]
