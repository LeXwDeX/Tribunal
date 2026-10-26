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

# codebase-memory-mcp (MIT) code graph server, bundled as a separate process.
# The -portable archives are statically linked; the glibc builds need
# GLIBC_2.38, newer than bookworm. The SHA-256 values come from the v0.12.1
# release assets and checksums.txt; the SLSA provenance of both archives was
# checked with `gh attestation verify` on 2026-10-26. Only the binary and its
# license texts are copied: the upstream installer is never run, because it
# writes agent configuration and hooks.
FROM debian:bookworm-slim AS codebase-memory
ARG TARGETARCH
ARG CODEBASE_MEMORY_MCP_VERSION=0.12.1
ARG CODEBASE_MEMORY_MCP_SHA256_AMD64=8c3d0341064301e78ecc84cf7348d4e2c6562a0f57448243ea560aaa34de36c1
ARG CODEBASE_MEMORY_MCP_SHA256_ARM64=ed0f1f548cd79b121c401c2e9f7841c189a001af26d3768368088c24f6a53533
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY third_party/codebase-memory-mcp/LICENSE /tmp/expected-LICENSE
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) sha="$CODEBASE_MEMORY_MCP_SHA256_AMD64" ;; \
      arm64) sha="$CODEBASE_MEMORY_MCP_SHA256_ARM64" ;; \
      *) echo "codebase-memory-mcp: unsupported TARGETARCH '$TARGETARCH'" >&2; exit 1 ;; \
    esac; \
    archive="codebase-memory-mcp-linux-${TARGETARCH}-portable.tar.gz"; \
    curl -fsSL --proto '=https' --tlsv1.2 -o "/tmp/$archive" \
      "https://github.com/DeusData/codebase-memory-mcp/releases/download/v${CODEBASE_MEMORY_MCP_VERSION}/$archive"; \
    echo "$sha  /tmp/$archive" | sha256sum -c -; \
    mkdir -p /out; \
    tar -xzf "/tmp/$archive" -C /out codebase-memory-mcp LICENSE THIRD_PARTY_NOTICES.md; \
    cmp /tmp/expected-LICENSE /out/LICENSE; \
    chmod 0755 /out/codebase-memory-mcp

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
COPY --from=codebase-memory /out/codebase-memory-mcp /usr/local/bin/codebase-memory-mcp
COPY --from=codebase-memory /out/LICENSE /out/THIRD_PARTY_NOTICES.md /licenses/codebase-memory-mcp/
# The wrapper confines HOME, cache and runtime to the project workspace. Keep
# the old crc- name for configurations written before the rename.
COPY scripts/codebase-memory-mcp.sh /usr/local/bin/tribunal-codebase-memory-mcp
RUN chmod 0755 /usr/local/bin/tribunal-codebase-memory-mcp \
    && ln -s tribunal-codebase-memory-mcp /usr/local/bin/crc-codebase-memory-mcp
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
