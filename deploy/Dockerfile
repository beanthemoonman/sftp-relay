# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETARCH=arm64
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/relay ./cmd/relay

FROM alpine:3.21
RUN apk add --no-cache nginx libcap ca-certificates \
 && adduser -D -u 1000 relay \
 && mkdir -p /data /var/lib/nginx/tmp /var/log/nginx /run/nginx \
 && chown -R relay:relay /data /var/lib/nginx /var/log/nginx /run/nginx \
 && setcap 'cap_net_bind_service=+ep' /usr/sbin/nginx
COPY deploy/nginx.conf /etc/nginx/nginx.conf
COPY deploy/entrypoint.sh /entrypoint.sh
COPY --from=build /out/relay /usr/local/bin/relay
USER relay
WORKDIR /data
ENV DB_PATH=/data/app.db PORT=8080 GOGC=50 GOMEMLIMIT=48MiB
EXPOSE 80
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:80/api/health || exit 1
ENTRYPOINT ["/entrypoint.sh"]
