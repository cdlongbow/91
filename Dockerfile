# ---- Stage 1: Build frontend ----
FROM node:20-slim AS frontend

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

COPY tsconfig.json vite.config.ts index.html ./
COPY public/ public/
COPY src/ src/
RUN npm run build

# ---- Stage 2: Build backend ----
FROM golang:1.23-bookworm AS backend

WORKDIR /app/backend

COPY backend/go.mod backend/go.sum ./
COPY backend/vendor/ vendor/
COPY backend/cmd/ cmd/
COPY backend/internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- Stage 3: Runtime ----
FROM debian:bookworm-slim AS runtime

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    ffmpeg \
    openssl \
    python3 \
    python3-bs4 \
    python3-lxml \
    python3-requests \
    python3-socks \
    tar \
    tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/video-site-91

COPY --from=backend /out/server ./server
COPY --from=frontend /app/dist ./dist
COPY backend/config.example.yaml ./config.example.yaml
COPY 91VideoSpider/ ./91VideoSpider/
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

ARG VERSION=dev

ENV VIDEO_CONFIG=/opt/video-site-91/data/config.yaml \
    VIDEO_FRONTEND_DIR=/opt/video-site-91/dist \
    VIDEO_GITHUB_REPO=nianzhibai/91 \
    VIDEO_IMAGE_VERSION=${VERSION} \
    VIDEO_LISTEN_PORT=9191 \
    VIDEO_VERSION_FILE=/opt/video-site-91/data/.version

RUN chmod +x ./server /usr/local/bin/docker-entrypoint.sh

VOLUME ["/opt/video-site-91/data"]
EXPOSE 9191

ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["./server"]
