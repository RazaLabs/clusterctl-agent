# Go cross-compiles, so the build runs natively on the builder for every
# target platform; only the final stage is per-platform.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X github.com/RazaLabs/kubctl-agent/internal/agent.Version=${VERSION}" \
    -o /out/kubctl-agent ./cmd/kubctl-agent

# Static binary plus CA certificates, running as nonroot (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kubctl-agent /kubctl-agent
ENTRYPOINT ["/kubctl-agent"]
