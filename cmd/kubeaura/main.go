// Command kubeaura is the KubeAura server: a single binary that reads your
// existing kubeconfig and serves an AI-assisted web UI for your clusters.
//
// Quick start:
//
//	kubeaura            # uses your current kube context, opens the browser
//	kubeaura --help     # every flag and environment variable
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/devganeshg/kubeaura/internal/ai"
	"github.com/devganeshg/kubeaura/internal/api"
	"github.com/devganeshg/kubeaura/internal/config"
	"github.com/devganeshg/kubeaura/internal/k8s"
	"github.com/devganeshg/kubeaura/internal/rag"
	"github.com/devganeshg/kubeaura/web"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// KUBEAURA_DESKTOP=1 is the env form of --desktop, so app bundles (macOS
	// .app, .desktop launchers) can enable it without passing arguments.
	desktopEnv := os.Getenv("KUBEAURA_DESKTOP") == "1"

	fs := flag.NewFlagSet("kubeaura", flag.ExitOnError)
	fs.Usage = func() { usage(fs) }
	var (
		f           config.Flags
		desktopFlag = fs.Bool("desktop", false, "open a desktop window instead of a browser tab")
		showVersion = fs.Bool("version", false, "print the version and exit")
	)
	fs.StringVar(&f.Addr, "addr", "", "listen address (default "+config.DefaultAddr+")")
	fs.StringVar(&f.Kubeconfig, "kubeconfig", "", "path to kubeconfig (default $KUBECONFIG, then ~/.kube/config)")
	fs.StringVar(&f.Context, "context", "", "kube context to start on (default: your current-context)")
	fs.StringVar(&f.AIProvider, "ai-provider", "", "anthropic | ollama | openai (default: auto-detect)")
	fs.StringVar(&f.AIModel, "ai-model", "", "model id override")
	fs.BoolVar(&f.NoBrowser, "no-browser", false, "do not open a browser on start")
	fs.BoolVar(&f.AllowRemote, "allow-remote", false, "serve non-loopback hosts (requires your own auth in front)")

	// `kubeaura config init` writes a starter config file; it must run before
	// flag parsing so the subcommand word is not read as a flag.
	if len(os.Args) > 1 && os.Args[1] == "config" {
		runConfigCmd(os.Args[2:])
		return
	}
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Printf("kubeaura %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}
	desktop := desktopEnv || *desktopFlag

	cfg, err := config.Load(f)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	mgr, err := k8s.NewManager(cfg.Kubeconfig)
	if err != nil {
		log.Fatalf("could not read your kubeconfig / connect to a cluster: %v\n\n"+
			"KubeAura uses your existing kube context, the same one kubectl uses.\n"+
			"  • check it resolves:  kubectl config current-context\n"+
			"  • point at another:   kubeaura --kubeconfig /path/to/config\n"+
			"  • no cluster yet?     kind create cluster   (or minikube start)", err)
	}
	if cfg.Context != "" {
		if err := mgr.SetActive(cfg.Context); err != nil {
			log.Fatalf("context %q not found in your kubeconfig (%v).\nAvailable: %s",
				cfg.Context, err, strings.Join(mgr.Contexts(), ", "))
		}
	}

	assistant := ai.Configure(ai.Settings{
		Provider:      cfg.AI.Provider,
		Model:         cfg.AI.Model,
		AnthropicKey:  cfg.AI.AnthropicKey,
		OllamaHost:    cfg.AI.OllamaHost,
		OpenAIBaseURL: cfg.AI.OpenAIBaseURL,
		OpenAIKey:     cfg.AI.OpenAIKey,
	})

	docsRAG := initDocsRetriever(cfg)

	srv := &api.Server{
		Mgr:         mgr,
		AI:          assistant,
		Static:      web.Handler(),
		Version:     version,
		DocsRAG:     docsRAG,
		DocsTopK:    cfg.RAG.DocsTopK,
		AllowRemote: cfg.AllowRemote,
	}

	// Bind the listener up front so we only open the browser once we know the
	// port is actually serving.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil && !addrWasChosen(cfg) {
		// Nobody asked for this specific port, so a collision is our problem,
		// not the operator's — step to the next free one instead of dying. A
		// desktop app launched from Finder has no terminal to show the error
		// in anyway. Fixed ports are tried first so the origin (and the
		// browser's remembered microphone permission) stays stable across
		// launches; a random port is the last resort.
		host, _, _ := net.SplitHostPort(cfg.Addr)
		if host == "" {
			host = "127.0.0.1"
		}
		base := portOf(cfg.Addr)
		for p := base + 1; p <= base+20 && err != nil; p++ {
			ln, err = net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		}
		if err != nil {
			ln, err = net.Listen("tcp", net.JoinHostPort(host, "0"))
		}
		if err == nil && ln.Addr().String() != cfg.Addr {
			fmt.Printf("  note: %s was busy, using %s instead\n", cfg.Addr, ln.Addr())
		}
	}
	if err != nil {
		log.Fatalf("could not listen on %s: %v\n"+
			"Something else is using that port. Pick another with --addr, e.g.\n"+
			"  kubeaura --addr 127.0.0.1:7655", cfg.Addr, err)
	}
	url := "http://localhost" + normalizeAddr(ln.Addr().String())

	// Binding beyond loopback publishes an unauthenticated UI that can apply,
	// delete, and exec. The guard in internal/api still refuses non-loopback
	// Host headers unless AllowRemote is set, but the operator should hear it.
	if !isLoopbackAddr(ln.Addr().String()) && !inContainer() {
		// Suppressed in a container: there, loopback is the container itself,
		// so binding all interfaces is correct and "bind 127.0.0.1 instead"
		// would be actively wrong advice.
		msg := "  ⚠  Listening on %s — beyond this machine.\n" +
			"     KubeAura has no login of its own and can apply/delete/exec with your\n" +
			"     credentials. Put authentication in front of it, or bind %s.\n\n"
		if !cfg.AllowRemote {
			msg += "     Non-loopback requests are currently REFUSED. Pass --allow-remote\n" +
				"     (or KUBEAURA_ALLOW_REMOTE=1) once a proxy is in place.\n\n"
		}
		fmt.Printf(msg, ln.Addr().String(), config.DefaultAddr)
	}

	banner(url, mgr, assistant, cfg)

	// Desktop mode: serve in the background and open the UI in its own
	// window instead of a browser tab. Preference order:
	//   1. Chrome/Edge app mode — chromeless window WITH working voice input
	//      (the Web Speech API only exists in Chromium browsers).
	//   2. The native system webview (build tag `desktop`; voice output only).
	// Set KUBEAURA_WEBVIEW=1 to skip Chrome and force the native webview.
	// Closing the window exits the app either way.
	if desktop {
		go func() {
			if err := http.Serve(ln, srv.Routes()); err != nil {
				log.Fatalf("server error: %v", err)
			}
		}()
		if chromeAppWindow(url) {
			return
		}
		if !runDesktop(url) {
			log.Fatalf("no Chrome/Edge/Chromium found for the app window, and this binary was built without the native webview — install Chrome, or rebuild with `make desktop`")
		}
		return
	}

	if !cfg.NoBrowser {
		go openBrowser(url)
	}

	if err := http.Serve(ln, srv.Routes()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// banner prints a short, friendly startup summary.
func banner(url string, mgr *k8s.Manager, assistant *ai.Assistant, cfg config.Config) {
	aiLine := "off — run `kubeaura config init` for setup, or add a model in the UI (✨ → ⚙)"
	if assistant.Enabled() {
		aiLine = fmt.Sprintf("on — %s (%s)", assistant.ProviderName(), assistant.ModelName())
	}
	src := "defaults + environment"
	if cfg.Source != "" {
		src = cfg.Source
	}
	fmt.Printf(`
  KubeAura %s
  ────────────────────────────────────────────────
  Cluster    %s  (%d context(s) in your kubeconfig)
  Assistant  %s
  Config     %s
  UI         %s

  Press Ctrl+C to stop.  (--no-browser to skip opening a browser)

`, version, mgr.Active(), len(mgr.Contexts()), aiLine, src, url)
}

// usage prints help in the shape people expect from a CLI: what it does, how
// to run it, then flags and the environment variables that mirror them.
func usage(fs *flag.FlagSet) {
	fmt.Fprintf(os.Stderr, `KubeAura %s — an AI-assisted Kubernetes cockpit you run yourself.

Reads your existing kubeconfig and serves a web UI on %s. It can do
exactly what your credentials can do with kubectl, and nothing more.

USAGE
  kubeaura [flags]
  kubeaura config init     write a starter config file
  kubeaura config path     print where the config file is read from

FLAGS
`, version, config.DefaultAddr)
	fs.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
ENVIRONMENT
  KUBECONFIG              path to kubeconfig
  KUBEAURA_CONFIG        config file path (default %s)
  KUBEAURA_ADDR          listen address
  KUBEAURA_CONTEXT       kube context to start on
  KUBEAURA_ALLOW_REMOTE  set to 1 to serve non-loopback hosts
  KUBEAURA_NO_BROWSER    set to 1 to skip opening a browser
  KUBEAURA_DESKTOP       set to 1 for the desktop window
  KUBEAURA_AI_PROVIDER   anthropic | ollama | openai
  KUBEAURA_AI_MODEL      model id override
  ANTHROPIC_API_KEY       enables the Anthropic backend
  OPENAI_BASE_URL         OpenAI-compatible endpoint (LM Studio, vLLM, …)
  OPENAI_API_KEY          bearer token for that endpoint (optional locally)
  OLLAMA_HOST             Ollama server URL (default http://localhost:11434)

  Precedence: flags > environment > config file > defaults.

DOCS
  https://github.com/devganeshg/kubeaura
`, config.Path())
}

// runConfigCmd implements the `config` subcommand.
func runConfigCmd(args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	p := config.Path()
	switch sub {
	case "path":
		fmt.Println(p)
	case "init":
		if p == "" {
			log.Fatal("could not determine a config location; set KUBEAURA_CONFIG")
		}
		if _, err := os.Stat(p); err == nil {
			fmt.Printf("config already exists: %s\n(delete it first, or edit it in place)\n", p)
			return
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			log.Fatalf("could not create %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(config.Example()), 0o600); err != nil {
			log.Fatalf("could not write %s: %v", p, err)
		}
		fmt.Printf("wrote %s\n\nEvery key is optional and commented. Edit it, then run `kubeaura`.\n", p)
	default:
		fmt.Fprintf(os.Stderr, "usage: kubeaura config <init|path>\n")
		os.Exit(2)
	}
}

// addrWasChosen reports whether the operator explicitly asked for this listen
// address. If they did, silently moving to a different port would break
// whatever they pointed at it, so a collision should fail loudly instead.
func addrWasChosen(cfg config.Config) bool {
	return cfg.Addr != config.DefaultAddr
}

// portOf extracts the port from a listen address, or 0 if it has none.
func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// inContainer reports whether we are running inside a container, where the
// container boundary — not the loopback interface — is the isolation.
func inContainer() bool {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true // a Pod
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// isLoopbackAddr reports whether a resolved listen address is reachable only
// from this machine.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func initDocsRetriever(cfg config.Config) *rag.Retriever {
	if !cfg.RAG.DocsEnabled {
		return nil
	}
	if cfg.RAG.DocsURL != "" {
		r, err := rag.NewFromURL(cfg.RAG.DocsURL)
		if err != nil {
			log.Printf("docs-rag remote index unavailable (%s): %v", cfg.RAG.DocsURL, err)
		} else {
			return r
		}
	}
	r, err := rag.NewFromDir(cfg.RAG.DocsPath)
	if err != nil {
		log.Printf("docs-rag disabled: %v", err)
		return nil
	}
	return r
}

// openBrowser best-effort opens the default browser once the server is up.
func openBrowser(url string) {
	// Give the listener a moment to start accepting.
	time.Sleep(400 * time.Millisecond)
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default: // linux, bsd, …
		cmd, args = "xdg-open", []string{url}
	}
	_ = exec.Command(cmd, args...).Start() // best effort; ignore failures (headless/SSH)
}

// normalizeAddr turns a bind address like ":8080" into ":8080" and "0.0.0.0:8080"
// into ":8080" for a clean localhost URL.
func normalizeAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ":" + addr
}
