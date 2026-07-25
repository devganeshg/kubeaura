VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/devganeshg/kubeaura
ADDR    ?= 127.0.0.1:8080

.PHONY: build run desktop run-desktop app app-windows app-linux app-all install install-app test vet fmt licenses check docker kind-load deploy undeploy tidy

## Build the single binary into ./bin/kubeaura
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/kubeaura ./cmd/kubeaura

## Run locally against your current kubeconfig context
run: build
	KUBEAURA_ADDR=$(ADDR) ./bin/kubeaura

## Build with the native desktop window (needs cgo / Xcode CLT on macOS)
desktop:
	CGO_ENABLED=1 go build -tags desktop -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/kubeaura ./cmd/kubeaura

## Run as a desktop app (native window instead of the browser)
run-desktop: desktop
	KUBEAURA_ADDR=$(ADDR) ./bin/kubeaura --desktop

## Package a double-clickable macOS app bundle into dist/KubeAura.app
app: desktop
	sh scripts/package-macos.sh

## Package the Windows desktop app zip (cross-compiled; window via Chrome/Edge on the target)
app-windows:
	VERSION=$(VERSION) sh scripts/package-windows.sh

## Package the Linux desktop app tarball (binary + .desktop entry + installer)
app-linux:
	VERSION=$(VERSION) sh scripts/package-linux.sh

## Package desktop apps for all three OSes into dist/
app-all: app app-windows app-linux

## Install the macOS app bundle into /Applications (`make install` works too)
install: install-app
install-app: app
	rm -rf /Applications/KubeAura.app
	cp -R dist/KubeAura.app /Applications/KubeAura.app
	@echo "installed /Applications/KubeAura.app — launch it from Spotlight or Launchpad"

test:
	go test ./... -race -count=1

## Everything CI runs, before you open a PR
check: fmt vet test licenses
	@git diff --quiet THIRD_PARTY_LICENSES.md || \
		echo "note: THIRD_PARTY_LICENSES.md changed — commit it"

## Regenerate attribution for the dependencies we ship (Apache-2.0 §4)
licenses:
	sh scripts/gen-licenses.sh

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

## Build the container image
docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

## Load the image into a local kind cluster (KIND_CLUSTER=name)
KIND_CLUSTER ?= kubeaura
kind-load: docker
	kind load docker-image $(IMAGE):latest --name $(KIND_CLUSTER)

## Deploy to the current cluster with the all-in-one manifest
deploy:
	kubectl apply -f deploy/kubernetes/install.yaml

undeploy:
	kubectl delete -f deploy/kubernetes/install.yaml --ignore-not-found
