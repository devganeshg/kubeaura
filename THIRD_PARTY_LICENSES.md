# Third-party licenses

KubeAura is distributed as a single statically linked binary. The binary
embeds the Go modules listed below. Each dependency remains under its own
license and copyright; this file is provided to satisfy the attribution
requirements of those licenses (notably Apache-2.0 section 4).

Full license texts live in each module's source repository and in your local
Go module cache under `$(go env GOMODCACHE)/<module>@<version>/LICENSE`.

**Weak-copyleft notice (MPL-2.0):** `github.com/hashicorp/go-cleanhttp` and
`github.com/hashicorp/go-retryablehttp` are licensed under the Mozilla Public
License 2.0. Their source is available unmodified at the URLs below; KubeAura
makes no modifications to either module.

| Module | Version | License |
| ------ | ------- | ------- |
| [github.com/davecgh/go-spew](https://github.com/davecgh/go-spew) | v1.1.2-0.20180830191138-d8f796af33cc | ISC |
| [github.com/emicklei/go-restful/v3](https://github.com/emicklei/go-restful/v3) | v3.13.0 | MIT |
| [github.com/fxamacker/cbor/v2](https://github.com/fxamacker/cbor/v2) | v2.9.0 | MIT |
| [github.com/go-logr/logr](https://github.com/go-logr/logr) | v1.4.3 | Apache-2.0 |
| [github.com/go-openapi/jsonpointer](https://github.com/go-openapi/jsonpointer) | v0.21.0 | Apache-2.0 |
| [github.com/go-openapi/jsonreference](https://github.com/go-openapi/jsonreference) | v0.20.2 | Apache-2.0 |
| [github.com/go-openapi/swag](https://github.com/go-openapi/swag) | v0.23.0 | Apache-2.0 |
| [github.com/google/gnostic-models](https://github.com/google/gnostic-models) | v0.7.0 | Apache-2.0 |
| [github.com/google/go-querystring](https://github.com/google/go-querystring) | v1.1.0 | BSD-3-Clause |
| [github.com/google/uuid](https://github.com/google/uuid) | v1.6.0 | BSD-3-Clause |
| [github.com/gorilla/websocket](https://github.com/gorilla/websocket) | v1.5.4-0.20250319132907-e064f32e3674 | BSD-2-Clause |
| [github.com/hashicorp/go-cleanhttp](https://github.com/hashicorp/go-cleanhttp) | v0.5.2 | MPL-2.0 |
| [github.com/hashicorp/go-retryablehttp](https://github.com/hashicorp/go-retryablehttp) | v0.7.7 | MPL-2.0 |
| [github.com/josharian/intern](https://github.com/josharian/intern) | v1.0.0 | MIT |
| [github.com/json-iterator/go](https://github.com/json-iterator/go) | v1.1.12 | MIT |
| [github.com/mailru/easyjson](https://github.com/mailru/easyjson) | v0.7.7 | MIT |
| [github.com/moby/spdystream](https://github.com/moby/spdystream) | v0.5.1 | Apache-2.0 |
| [github.com/modern-go/concurrent](https://github.com/modern-go/concurrent) | v0.0.0-20180306012644-bacd9c7ef1dd | Apache-2.0 |
| [github.com/modern-go/reflect2](https://github.com/modern-go/reflect2) | v1.0.3-0.20250322232337-35a7c28c31ee | Apache-2.0 |
| [github.com/munnerz/goautoneg](https://github.com/munnerz/goautoneg) | v0.0.0-20191010083416-a7dc8b61c822 | BSD-3-Clause |
| [github.com/spf13/pflag](https://github.com/spf13/pflag) | v1.0.9 | BSD-3-Clause |
| [github.com/x448/float16](https://github.com/x448/float16) | v0.8.4 | MIT |
| [github.com/xanzy/go-gitlab](https://github.com/xanzy/go-gitlab) | v0.115.0 | Apache-2.0 |
| [go.yaml.in/yaml/v2](https://go.yaml.in/yaml/v2) | v2.4.3 | Apache-2.0 |
| [go.yaml.in/yaml/v3](https://go.yaml.in/yaml/v3) | v3.0.4 | MIT |
| [golang.org/x/net](https://golang.org/x/net) | v0.55.0 | BSD-3-Clause |
| [golang.org/x/oauth2](https://golang.org/x/oauth2) | v0.34.0 | BSD-3-Clause |
| [golang.org/x/sys](https://golang.org/x/sys) | v0.45.0 | BSD-3-Clause |
| [golang.org/x/term](https://golang.org/x/term) | v0.43.0 | BSD-3-Clause |
| [golang.org/x/text](https://golang.org/x/text) | v0.39.0 | BSD-3-Clause |
| [golang.org/x/time](https://golang.org/x/time) | v0.14.0 | BSD-3-Clause |
| [google.golang.org/protobuf](https://google.golang.org/protobuf) | v1.36.12-0.20260120151049-f2248ac996af | BSD-3-Clause |
| [gopkg.in/evanphx/json-patch.v4](https://gopkg.in/evanphx/json-patch.v4) | v4.13.0 | BSD-3-Clause |
| [gopkg.in/inf.v0](https://gopkg.in/inf.v0) | v0.9.1 | BSD-3-Clause |
| [gopkg.in/yaml.v3](https://gopkg.in/yaml.v3) | v3.0.1 | MIT |
| [k8s.io/api](https://k8s.io/api) | v0.36.2 | Apache-2.0 |
| [k8s.io/apimachinery](https://k8s.io/apimachinery) | v0.36.2 | Apache-2.0 |
| [k8s.io/client-go](https://k8s.io/client-go) | v0.36.2 | Apache-2.0 |
| [k8s.io/klog/v2](https://k8s.io/klog/v2) | v2.140.0 | Apache-2.0 |
| [k8s.io/kube-openapi](https://k8s.io/kube-openapi) | v0.0.0-20260317180543-43fb72c5454a | Apache-2.0 |
| [k8s.io/metrics](https://k8s.io/metrics) | v0.36.2 | Apache-2.0 |
| [k8s.io/streaming](https://k8s.io/streaming) | v0.36.2 | Apache-2.0 |
| [k8s.io/utils](https://k8s.io/utils) | v0.0.0-20260210185600-b8788abfbbc2 | Apache-2.0 |
| [sigs.k8s.io/json](https://sigs.k8s.io/json) | v0.0.0-20250730193827-2d320260d730 | Apache-2.0 |
| [sigs.k8s.io/randfill](https://sigs.k8s.io/randfill) | v1.0.0 | Apache-2.0 |
| [sigs.k8s.io/structured-merge-diff/v6](https://sigs.k8s.io/structured-merge-diff/v6) | v6.3.2 | Apache-2.0 |
| [sigs.k8s.io/yaml](https://sigs.k8s.io/yaml) | v1.6.0 | MIT |

## Runtime assets loaded by the web UI

The topology "galaxy" view lazy-loads
[3d-force-graph](https://github.com/vasturiano/3d-force-graph) (MIT, Copyright
Vasco Asturiano) from the unpkg CDN on first use. It is **not** bundled in the
binary, and no other view makes an outbound request. Every other asset in the
UI is embedded via `go:embed`.

Regenerate this file with `sh scripts/gen-licenses.sh`.
