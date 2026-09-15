package main

import (
	"log"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dodoyu-sama/mcp-arc/internal/config"
	"github.com/dodoyu-sama/mcp-arc/internal/proxy"
)

func main() {
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
				log.Printf("warn: could not load config (%v), using defaults", err)
				cfg = config.Default()
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

			p := proxy.New(proxy.Options{
				UpstreamCmd: upstreamCmd,
				Config:      cfg,
			})
			return p.Run()
		},
	}

	rootCmd.Flags().StringVar(&configPath, "config", "config.yaml", "path to YAML config file")
	rootCmd.Flags().StringVar(&upstream, "upstream", "", "upstream command, e.g. 'node server.js'")
	rootCmd.Flags().StringVar(&clientTransport, "client-transport", "", "client transport: stdio | sse")
	rootCmd.Flags().StringVar(&listen, "listen", "", "listen address for SSE client transport, e.g. :8081")
	rootCmd.Flags().StringVar(&upstreamTrans, "upstream-transport", "", "upstream transport: stdio | sse")
	rootCmd.Flags().StringVar(&upstreamURL, "upstream-url", "", "upstream /sse URL when upstream transport = sse")

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}
