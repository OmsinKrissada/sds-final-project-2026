# syntax=docker/dockerfile:1
# Build context: REPO ROOT.  docker buildx build -f services/frontend/Dockerfile .

# Stage 1: single-file Vite build. Output is ../../services/frontend/dist/index.html
# relative to frontend/web, i.e. /src/services/frontend/dist/index.html here.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/frontend/web
COPY frontend/web/package.json frontend/web/package-lock.json ./
RUN npm ci
COPY frontend/web/ ./
RUN mkdir -p /src/services/frontend/dist && npm run build

# Stage 2: Go server embedding dist/index.html
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY services ./services
COPY --from=web /src/services/frontend/dist/index.html ./services/frontend/dist/index.html
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/app ./services/frontend

# Stage 3: runtime
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
