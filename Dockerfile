# Server and tribunalctl image: embedded web console, git, and license texts.
# Base tags were checked on Docker Hub on 2026-10-22:
#   node:22.20.0-bookworm-slim
#   golang:1.26.10-bookworm
#   debian:bookworm-slim
# Keep these tags pinned; do not replace them with unverified tags.

FROM node:22.20.0-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json web/.npmrc ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26.10-bookworm AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# web/dist is excluded from the build context. Use the fresh frontend build
# from the web stage so the binary always embeds a matching console.
COPY --from=web /src/web/dist ./web/dist
RUN go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/server ./cmd/server \
    && go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/tribunalctl ./cmd/tribunalctl

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git passwd \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 tribunal \
    && useradd --uid 10001 --gid tribunal --create-home --home-dir /home/tribunal --shell /bin/false tribunal

WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /out/tribunalctl /app/tribunalctl
# Keep the old path for scripts written against the previous image.
RUN ln -s tribunalctl /app/reviewctl
COPY LICENSE THIRD_PARTY_LICENSES /licenses/
COPY licenses/ /licenses/
COPY config/server.example.yaml /app/config/
RUN install -d -o tribunal -g tribunal -m 0700 /app/data

USER 10001:10001
ENV HOME=/home/tribunal
EXPOSE 8080
VOLUME ["/app/data"]
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/readyz >/dev/null || exit 1

ENTRYPOINT ["/app/server"]
CMD ["--config", "/app/config/server.example.yaml"]
