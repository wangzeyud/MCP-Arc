package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wangzeyud/mcp-arc/internal/config"
	"github.com/wangzeyud/mcp-arc/internal/netutil"
	"github.com/wangzeyud/mcp-arc/internal/proxy"
)

func main() {
	// Cobra ships a Windows "mousetrap" that, when the binary is launched by
	// double-clicking from Explorer, prints a splash ("This is a command line
	// tool…") and exits after a few seconds. mcp-arc is meant to be double-
	// clickable (it opens the web console), so disable that behavior.
	cobra.MousetrapHelpText = ""

	var (
		configPath      string
		upstream        string
		clientTransport string
		listen          string
		upstreamTrans   string
		upstreamURL     string
	)

	rootCmd := &cobra.Command{
		Use:   "mcp-arc",
		Short: "MCP Arc — a lightweight MCP proxy / sidecar",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				// Retry next to the executable so a binary run from a different
				// working directory still finds its config.
				if exe, e2 := os.Executable(); e2 == nil {
					if alt := filepath.Join(filepath.Dir(exe), "config.yaml"); alt != configPath {
						if cfg2, e3 := config.Load(alt); e3 == nil {
							cfg, err = cfg2, nil
						}
					}
				}
			}
			if err != nil {
				log.Printf("warn: could not load config (%v), using defaults", err)
				cfg = config.Default()
				// Safety net: when no config file is present, still bring up the web
				// console and open it, so the sidecar is usable (and the audit /
				// masking / replay UI is reachable) out of the box on a double-click.
				cfg.Admin.Enabled = true
				cfg.Admin.OpenBrowser = true
			}

			// CLI overrides
			if clientTransport != "" {
				cfg.Transport.Client = clientTransport
			}
			if listen != "" {
				cfg.Transport.Listen = listen
			}
			if upstreamTrans != "" {
				cfg.Transport.Upstream = upstreamTrans
			}
			if upstreamURL != "" {
				cfg.Transport.UpstreamURL = upstreamURL
			}

			var upstreamCmd []string
			switch {
			case upstream != "":
				upstreamCmd = strings.Fields(upstream)
			case len(cfg.Server.Upstream) > 0:
				upstreamCmd = cfg.Server.Upstream
			default:
				upstreamCmd = args
			}

			// 8080/8081 are heavily used, so never fail just because they are
			// taken: move to the next free port and tell the user the real one.
			consoleURL, sseURL := resolvePorts(cfg)

			// Open the console once on startup in the default browser if
			// requested (best-effort; failures are only logged).
			if cfg.Admin.OpenBrowser && consoleURL != "" {
				go func() {
					time.Sleep(600 * time.Millisecond)
					openBrowser(consoleURL)
				}()
			}

			p := proxy.New(proxy.Options{
				UpstreamCmd: upstreamCmd,
				Config:      cfg,
				SSEURL:      sseURL,
				ConsoleURL:  consoleURL,
			})
			return p.Run()
		},
	}

	rootCmd.Flags().StringVar(&configPath, "config", "config.yaml", "path to YAML config file")
	rootCmd.Flags().StringVar(&upstream, "upstream", "", "upstream command, e.g. 'node server.js'")
	rootCmd.Flags().StringVar(&clientTransport, "client-transport", "", "client transport: stdio | sse | streamable-http")
	rootCmd.Flags().StringVar(&listen, "listen", "", "listen address for HTTP client transports, e.g. :8081")
	rootCmd.Flags().StringVar(&upstreamTrans, "upstream-transport", "", "upstream transport: stdio | sse | streamable-http")
	rootCmd.Flags().StringVar(&upstreamURL, "upstream-url", "", "upstream endpoint URL when upstream transport = sse | streamable-http")

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

// resolvePorts picks free ports for the web console and the SSE endpoint,
// mutating cfg so the proxy binds exactly what we report. It returns the URLs
// to surface to the user: the console, and the SSE address to paste into their
// MCP client. This is what makes the tool survive 8080/8081 already being taken
// on a developer's machine.
func resolvePorts(cfg *config.Config) (consoleURL, sseURL string) {
	if cfg.Admin.Enabled {
		port, moved := netutil.FreeTCPPort("", cfg.Admin.Port)
		if moved {
			log.Printf("warn: admin port %d is already in use, using %d instead", cfg.Admin.Port, port)
		}
		cfg.Admin.Port = port
		consoleURL = fmt.Sprintf("http://localhost:%d/", port)
		log.Printf("mcp-arc: console: %s", consoleURL)
	}
	if cfg.Transport.Client == "sse" || cfg.Transport.Client == "streamable-http" {
		addr, moved := netutil.FreeListenAddr(cfg.Transport.Listen)
		if moved {
			log.Printf("warn: client port %s is already in use, using %s instead", cfg.Transport.Listen, addr)
		}
		cfg.Transport.Listen = addr
		if cfg.Transport.Client == "sse" {
			sseURL = netutil.URLForListen(addr, "/sse")
			log.Printf("mcp-arc: MCP client SSE endpoint: %s", sseURL)
		} else {
			path := cfg.Transport.StreamableHTTPPath
			if path == "" {
				path = "/mcp"
			}
			sseURL = netutil.URLForListen(addr, path)
			log.Printf("mcp-arc: MCP client Streamable HTTP endpoint: %s", sseURL)
		}
	}
	return consoleURL, sseURL
}

// openBrowser opens rawURL in the OS default browser. It is a best-effort
// convenience triggered by --open / admin.open_browser; failures are only logged.
func openBrowser(rawURL string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", "", rawURL}
	case "darwin":
		cmd = "open"
		args = []string{rawURL}
	default:
		cmd = "xdg-open"
		args = []string{rawURL}
	}
	if err := exec.Command(cmd, args...).Start(); err != nil {
		log.Printf("warn: could not open browser automatically: %v", err)
	}
}
