# Multi-stage build. The pure-Go SQLite driver lets us build a fully static
# binary (CGO_ENABLED=0) and ship it FROM scratch.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /how-hot-is-it .

FROM scratch
# scratch has no CA certificates; Telegram is HTTPS, so copy the bundle from the
# build stage or outbound TLS will fail.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /how-hot-is-it /how-hot-is-it
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/how-hot-is-it", "-config", "/data/config.json", "-db", "/data/howhot.db"]
