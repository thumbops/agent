# Immagine minima: binario statico su distroless, utente non root.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=0.1.0-dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/thumbops-agent ./cmd/thumbops-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/thumbops-agent /thumbops-agent
USER 65532:65532
ENTRYPOINT ["/thumbops-agent"]
