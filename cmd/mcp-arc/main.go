package main

import (
	"fmt"
	"log"
	"net"
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
		upstreamConnMs  int
	)

	rootCmd := &cobra.Command{
		Use:   "mcp-arc",
		Short: "MCP Arc — a lightweight MCP proxy / sidecar",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Discover the config: --config > MCP_ARC_CONFIG env > exe-dir >
			// XDG/system config dir. A binary on PATH may not sit next to its
			// config, so we search beyond the working directory.
			var cfg *config.Config
			if path, ok := findConfigPath(configPath); ok {
				loaded, err := config.Load(path)
				if err != nil {
					log.Printf("warn: could not load config %s (%v), using defaults", path, err)
				} else {
					cfg = loaded
					log.Printf("mcp-arc: loaded config %s", path)
				}
			}
			if cfg == nil {
				log.Printf("warn: no config found, using defaults")
				cfg = config.Default()
				// Safety net: make the console reachable out of the box so the
				// audit / masking / replay UI is available. We deliberately do NOT
				// auto-open the browser here: an MCP client spawns mcp-arc as a
				// stdio child, and popping a tab on every launch would be noise.
				// The console URL is logged instead.
				cfg.Admin.Enabled = true
			}

			// Config hot-reload (v0.7): wrap the loaded config in a Manager that
			// watches the file and atomically swaps in new values. CLI overrides
			// below apply to this initial snapshot; a later file change makes the
			// file the source of truth.
			var cfgMgr *config.Manager
			if path, ok := findConfigPath(configPath); ok {
				cfgMgr = config.NewWatcherFromFile(path, cfg)
			} else {
				cfgMgr = config.NewManager(cfg)
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
			if upstreamConnMs != 0 {
				cfg.Server.UpstreamConnectTimeoutMs = upstreamConnMs
			}

			var upstreamCmd []string
			switch {
			case upstream != "":
				// Quote-aware split so upstream paths containing spaces survive.
				upstreamCmd = splitCommand(upstream)
			case len(cfg.Server.Upstream) > 0:
				upstreamCmd = cfg.Server.Upstream
			default:
				upstreamCmd = args
			}

			// 8080/8081 are heavily used, so never fail just because they are
			// taken: move to the next free port and tell the user the real one.
			// The admin port is reserved atomically (held listener) so multiple
			// instances can run side by side without a :8080 collision.
			consoleURL, sseURL, adminLn := resolvePorts(cfg)

			// Open the console once on startup in the default browser if
			// requested (best-effort; failures are only logged).
			if cfg.Admin.OpenBrowser && consoleURL != "" {
				go func() {
					time.Sleep(600 * time.Millisecond)
					openBrowser(consoleURL)
				}()
			}

			p := proxy.New(proxy.Options{
				UpstreamCmd:   upstreamCmd,
				Config:        cfg,
				ConfigMgr:     cfgMgr,
				ConfigDir:     cfg.ConfigDir,
				SSEURL:        sseURL,
				ConsoleURL:    consoleURL,
				AdminListener: adminLn,
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
	rootCmd.Flags().IntVar(&upstreamConnMs, "upstream-connect-timeout-ms", 0, "wait (ms) for the upstream's first connection; 0 = config default (30000). Raise for slow npx cold starts.")

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

// resolvePorts picks free ports for the web console and the SSE endpoint,
// mutating cfg so the proxy binds exactly what we report. It returns the URLs
// to surface to the user (console, and the SSE address to paste into their MCP
// client) plus a held listener for the admin console. The admin port is reserved
// atomically: we bind and KEEP the socket so no second instance launched at the
// same time can probe 8080 free and then collide on the real bind. This is what
// lets several mcp-arc instances coexist (each on its own port) on one machine.
func resolvePorts(cfg *config.Config) (consoleURL, sseURL string, adminLn net.Listener) {
	if cfg.Admin.Enabled {
		ln, port, moved := netutil.ReserveTCPPort("", cfg.Admin.Port)
		if moved {
			log.Printf("warn: admin port %d is already in use, using %d instead", cfg.Admin.Port, port)
		}
		if ln == nil {
			log.Printf("warn: could not reserve any admin port near %d; console may fail to start", cfg.Admin.Port)
		}
		cfg.Admin.Port = port
		adminLn = ln
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
	return consoleURL, sseURL, adminLn
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

// findConfigPath resolves the config file to load. It honours an explicit path
// (--config) first, then the MCP_ARC_CONFIG env var, then a config next to the
// executable, then the platform config directory (XDG on Linux, %APPDATA% on
// Windows, ~/Library/Application Support on macOS). Returns ("", false) if none
// exist, in which case the caller falls back to built-in defaults.
func findConfigPath(explicit string) (string, bool) {
	var candidates []string
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if env := os.Getenv("MCP_ARC_CONFIG"); env != "" {
		candidates = append(candidates, env)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "config.yaml"))
	}
	if xdg := xdgConfigDir(); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "mcp-arc", "config.yaml"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, true
		}
	}
	return "", false
}

// xdgConfigDir returns the base directory for application config on the current
// platform, matching the XDG base directory spec where applicable.
func xdgConfigDir() string {
	switch runtime.GOOS {
	case "windows":
		if p := os.Getenv("APPDATA"); p != "" {
			return p
		}
	case "darwin":
		if h := os.Getenv("HOME"); h != "" {
			return filepath.Join(h, "Library", "Application Support")
		}
	default:
		if p := os.Getenv("XDG_CONFIG_HOME"); p != "" {
			return p
		}
		if h := os.Getenv("HOME"); h != "" {
			return filepath.Join(h, ".config")
		}
	}
	return ""
}

// splitCommand splits a command line into argv tokens. It respects single and
// double quotes so that paths containing spaces (e.g. `npx -y server /path/with
// spaces`) stay intact, while ordinary spaces still separate tokens. This
// replaces the previous strings.Fields, which broke any upstream path with a
// space.
func splitCommand(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := byte(0)
	has := false
	flush := func() {
		if has {
			out = append(out, cur.String())
			cur.Reset()
			has = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			} else {
				cur.WriteByte(c)
			}
			has = true
			continue
		}
		switch c {
		case ' ', '\t':
			flush()
		case '\'', '"':
			inQuote = c
			has = true
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	flush()
	return out
}
