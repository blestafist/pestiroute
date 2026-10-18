FROM golang:1.27.1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-buildid=' -o /out/pestiroute ./cmd/gateway

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 pestiroute \
    && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin pestiroute \
    && install -d -o 10001 -g 10001 -m 0700 /var/lib/pestiroute \
    && install -d -o 10001 -g 10001 -m 0750 /etc/pestiroute
COPY --from=build --chown=10001:10001 /out/pestiroute /pestiroute
USER 10001:10001
EXPOSE 8080
VOLUME ["/etc/pestiroute", "/var/lib/pestiroute"]
ENTRYPOINT ["/pestiroute"]
