# syntax=docker/dockerfile:1
# ---------------------------------------------------------------------------
# Stage 1: build ubersdr_lightning Go binary
# ---------------------------------------------------------------------------
FROM golang:1.24-bookworm AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /out/ubersdr_lightning ./...

# ---------------------------------------------------------------------------
# Stage 2: minimal runtime image
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        wget \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -s /bin/false lightning

COPY --from=go-builder /out/ubersdr_lightning /usr/local/bin/ubersdr_lightning

# Copy entrypoint script (translates env vars to ubersdr_lightning flags)
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

# Create the default data directory and ensure the lightning user owns it.
# Note: no VOLUME declaration — the docker-compose.yml bind mount handles persistence.
# A VOLUME declaration would cause Docker to create a root-owned anonymous volume
# that overwrites the chown, preventing the lightning user from writing to /data.
RUN chmod +x /usr/local/bin/entrypoint.sh \
    && mkdir -p /data \
    && chown lightning:lightning /data \
    && chmod 755 /data

USER lightning

# Expose the web UI port (default; override with WEB_PORT env var)
EXPOSE 6097

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/bin/wget", "-q", "-O", "/dev/null", "http://localhost:6097/"]

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
