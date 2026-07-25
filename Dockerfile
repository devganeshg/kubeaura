# ---- build stage ----
# Pinned to the patch release go.mod requires: the official images set
# GOTOOLCHAIN=local, so a 1.26.4 base cannot auto-fetch 1.26.5 and the build
# fails outright.
FROM golang:1.26.5 AS build
WORKDIR /src

# Cache module downloads
COPY go.mod go.sum ./
RUN go mod download

# Build the static single binary (UI is embedded via go:embed)
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/kubeaura ./cmd/kubeaura

# ---- runtime stage ----
# distroless/static:nonroot ships CA certs (for the Anthropic API + in-cluster
# TLS) and runs as an unprivileged user by default.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/kubeaura /kubeaura

# The binary binds loopback by default, which is right on a workstation and
# wrong in a container: loopback here is the container itself, so `-p` would
# publish a port nothing is listening on. The container boundary is the
# isolation, so bind all interfaces inside it.
ENV KUBEAURA_ADDR=:7654
EXPOSE 7654

USER nonroot:nonroot
ENTRYPOINT ["/kubeaura"]
