# Hub + agent download blobs for /install.sh.
# Build: docker compose build   (or docker build -t takan .)
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags='-s -w' -o /out/takan ./cmd/takan
RUN mkdir -p /out/agents \
 && GOOS=linux  GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/agents/takan-agent-linux-amd64  ./cmd/takan-agent \
 && GOOS=linux  GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o /out/agents/takan-agent-linux-arm64  ./cmd/takan-agent \
 && GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/agents/takan-agent-darwin-amd64 ./cmd/takan-agent \
 && GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o /out/agents/takan-agent-darwin-arm64 ./cmd/takan-agent

FROM debian:bookworm-slim
ARG TARGETARCH=amd64
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata wget \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 1000 --create-home --home-dir /home/takan --shell /usr/sbin/nologin takan \
 && mkdir -p /data /opt/takan/agents \
 && chown -R takan:takan /data /opt/takan /home/takan
COPY --from=build --chown=takan:takan /out/takan /usr/local/bin/takan
COPY --from=build --chown=takan:takan /out/agents/ /opt/takan/agents/
COPY --from=build --chown=takan:takan /out/agents/takan-agent-linux-${TARGETARCH} /usr/local/bin/takan-agent
COPY --chown=root:root deploy/docker-entrypoint.sh /usr/local/bin/takan-entrypoint
RUN chmod 755 /usr/local/bin/takan /usr/local/bin/takan-agent /usr/local/bin/takan-entrypoint /opt/takan/agents/*
USER takan
ENV TAKAN_LISTEN=0.0.0.0:8090 \
    TAKAN_PUBLIC_URL=http://localhost:8090 \
    TAKAN_DATA_DIR=/data \
    TAKAN_AGENT_BIN_DIR=/opt/takan/agents
EXPOSE 8090
VOLUME /data
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/takan-entrypoint"]
