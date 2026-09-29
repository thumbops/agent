# Minimal image: static binary on distroless, non-root user.
# The build stage runs on the build platform and cross-compiles, so
# multi-arch images (docker buildx --platform) need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=0.1.0-dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/thumbops-agent ./cmd/thumbops-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/thumbops-agent /thumbops-agent
USER 65532:65532
ENTRYPOINT ["/thumbops-agent"]
