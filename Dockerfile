# ---- build stage ----
FROM golang:1.26 AS build
WORKDIR /src

# Cache module downloads
COPY go.mod go.sum ./
RUN go mod download

# Build the static single binary (UI is embedded via go:embed)
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/kubemind ./cmd/kubemind

# ---- runtime stage ----
# distroless/static:nonroot ships CA certs (for the Anthropic API + in-cluster
# TLS) and runs as an unprivileged user by default.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/kubemind /kubemind
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/kubemind"]
