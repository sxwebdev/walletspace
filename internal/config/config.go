// Package config defines the service configuration and its network defaults.
package config

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/sxwebdev/xconfig"
	"github.com/sxwebdev/xconfig/decoders/xconfigyaml"
	"github.com/sxwebdev/xconfig/plugins/loader"
)

// Network names supported by the service.
const (
	NetworkMainnet = "mainnet"
	NetworkNile    = "nile"
)

// Config holds every knob of the service. Values come from defaults, an
// optional config.yaml and environment variables, in that order.
type Config struct {
	Addr        string `yaml:"addr" default:"127.0.0.1:8080" env:"ADDR" usage:"HTTP listen address"`
	DataDir     string `yaml:"data_dir" default:"./data" env:"DATA_DIR" usage:"directory for wallets.json and mnemonic.txt"`
	OpenBrowser bool   `yaml:"open_browser" default:"true" env:"OPEN_BROWSER" usage:"open the UI in a browser on start"`

	Mnemonic   string `yaml:"mnemonic" env:"MNEMONIC" secret:"true" usage:"BIP39 mnemonic; generated and stored in DataDir when empty"`
	Passphrase string `yaml:"passphrase" env:"PASSPHRASE" secret:"true" usage:"BIP39 passphrase; changing it changes every address"`

	Network      string `yaml:"network" default:"mainnet" env:"NETWORK" usage:"mainnet or nile"`
	Nodes        string `yaml:"nodes" env:"NODES" usage:"comma-separated node list, e.g. https://tron-rpc.publicnode.com; empty uses network defaults"`
	APIKey       string `yaml:"api_key" env:"TRON_PRO_API_KEY" secret:"true" usage:"TronGrid API key, optional"`
	USDTContract string `yaml:"usdt_contract" env:"USDT_CONTRACT" usage:"TRC20 contract shown as USDT; empty uses network default"`
	FeeLimitTRX  int64  `yaml:"fee_limit_trx" default:"50" env:"FEE_LIMIT_TRX" usage:"fee limit for TRC20 transfers, in TRX"`
}

// Node is one parsed RPC endpoint together with how it is reached.
//
// Headers, HTTPClient and DialContext are not part of the endpoint string and
// are never parsed from one: they are attached by the caller that resolved the
// credentials for this node and owns the guarded transport. A node built by
// ParseNode alone carries none of them.
type Node struct {
	Address string
	HTTP    bool
	TLS     bool
	Tier    int

	// Headers are the credentials for this node and this node only.
	Headers map[string]string
	// HTTPClient carries the HTTP node's requests. Nil falls back to the Tron
	// client's own default, which does no address filtering.
	HTTPClient *http.Client
	// DialContext is the guarded dialer for a gRPC node.
	DialContext func(context.Context, string) (net.Conn, error)
}

type networkDefaults struct {
	usdtContract string
	explorer     string
	nodes        []Node
}

var defaultsByNetwork = map[string]networkDefaults{
	NetworkMainnet: {
		usdtContract: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		explorer:     "https://tronscan.org",
		nodes: []Node{
			{Address: "https://tron-rpc.publicnode.com", HTTP: true},
		},
	},
	NetworkNile: {
		usdtContract: "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf",
		explorer:     "https://nile.tronscan.org",
		nodes: []Node{
			{Address: "grpc.nile.trongrid.io:50051", Tier: 0},
			{Address: "https://nile.trongrid.io", HTTP: true, Tier: 0},
		},
	},
}

// Load reads the configuration from ./config.yaml (optional) and the
// environment, then validates and fills in network-derived defaults.
func Load() (*Config, error) {
	l, err := loader.NewLoader(map[string]loader.Unmarshal{
		"yaml": xconfigyaml.New().Unmarshal,
	})
	if err != nil {
		return nil, fmt.Errorf("create config loader: %w", err)
	}

	if err := l.AddFile("config.yaml", true); err != nil {
		return nil, fmt.Errorf("add config.yaml: %w", err)
	}

	cfg := &Config{}
	if _, err := xconfig.Load(cfg, xconfig.WithLoader(l)); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if err := cfg.normalize(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) normalize() error {
	c.Network = strings.ToLower(strings.TrimSpace(c.Network))

	def, ok := defaultsByNetwork[c.Network]
	if !ok {
		return fmt.Errorf("unknown network %q: expected %s or %s", c.Network, NetworkMainnet, NetworkNile)
	}

	if c.USDTContract == "" {
		c.USDTContract = def.usdtContract
	}

	if c.FeeLimitTRX <= 0 {
		return fmt.Errorf("fee_limit_trx must be greater than zero, got %d", c.FeeLimitTRX)
	}

	if _, err := c.ParseNodes(); err != nil {
		return err
	}

	return nil
}

// ExplorerURL returns the block explorer base URL for the configured network.
func (c *Config) ExplorerURL() string {
	return defaultsByNetwork[c.Network].explorer
}

// ParseNodes returns the configured nodes, falling back to the network
// defaults when Nodes is empty.
//
// Each entry is "scheme://host[:port]" where the scheme selects the transport:
// grpc (plaintext), grpcs (TLS), http, https. A "|N" suffix sets the tier,
// e.g. "https://api.trongrid.io|1".
func (c *Config) ParseNodes() ([]Node, error) {
	raw := strings.TrimSpace(c.Nodes)
	if raw == "" {
		return defaultsByNetwork[c.Network].nodes, nil
	}

	var nodes []Node
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		node, err := ParseNode(entry)
		if err != nil {
			return nil, err
		}

		nodes = append(nodes, node)
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("nodes %q contains no usable entries", c.Nodes)
	}

	return nodes, nil
}

// ParseNode turns one endpoint into a Node.
//
// The entry must describe exactly one node. A comma is refused rather than
// treated as part of the host: callers that validated an endpoint as a single
// URL would otherwise hand over a string that expands into several nodes here,
// and every node after the first would reach the network without ever having
// been checked. That is the whole of the "scheme://host, scheme://host"
// smuggling trick, so it is refused at the one place that can see it.
func ParseNode(entry string) (Node, error) {
	if strings.ContainsRune(entry, ',') {
		return Node{}, fmt.Errorf("node %q: a single endpoint must not contain a comma", entry)
	}
	tier := 0
	if idx := strings.LastIndex(entry, "|"); idx >= 0 {
		// strconv, not Sscanf: Sscanf stops at the first non-digit and reports
		// no error, so "|1x" would silently parse as tier 1.
		parsed, err := strconv.Atoi(entry[idx+1:])
		if err != nil {
			return Node{}, fmt.Errorf("node %q: invalid tier suffix: %w", entry, err)
		}

		if parsed < 0 {
			return Node{}, fmt.Errorf("node %q: tier must not be negative, got %d", entry, parsed)
		}

		tier = parsed
		entry = entry[:idx]
	}

	scheme, host, ok := strings.Cut(entry, "://")
	if !ok || host == "" {
		return Node{}, fmt.Errorf("node %q: expected scheme://host, e.g. grpcs://grpc.trongrid.io:50051", entry)
	}

	switch strings.ToLower(scheme) {
	case "grpc":
		return Node{Address: host, Tier: tier}, nil
	case "grpcs":
		return Node{Address: host, TLS: true, Tier: tier}, nil
	case "http", "https":
		return Node{Address: entry, HTTP: true, Tier: tier}, nil
	default:
		return Node{}, fmt.Errorf("node %q: unknown scheme %q: expected grpc, grpcs, http or https", entry, scheme)
	}
}
