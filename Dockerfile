# syntax=docker/dockerfile:1
# golang:1.27-alpine (multi-arch index digest, bumped by Dependabot)
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS base
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .

# `docker build --target test .` runs vet + unit tests; the build fails if they fail.
FROM base AS test
ENV CGO_ENABLED=0
RUN test -z "$(gofmt -l .)" && go vet ./... && go test ./...

FROM base AS build
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/docker-image-updater ./cmd/updater

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/docker-image-updater /docker-image-updater
LABEL org.opencontainers.image.title="docker-image-updater" \
      org.opencontainers.image.description="Automatically updates running Docker containers when a newer image is available" \
      org.opencontainers.image.source="https://github.com/bilyboy785/docker-image-updater" \
      org.opencontainers.image.licenses="MIT" \
      docker-image-updater.self="true"
# Probes /healthz of the built-in web server (succeeds at once when the web server is disabled).
HEALTHCHECK --interval=60s --timeout=5s --start-period=30s --retries=3 CMD ["/docker-image-updater","-healthcheck"]
ENTRYPOINT ["/docker-image-updater"]
