package config

import (
	"testing"
)

func TestParseNodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		nodes   string
		network string
		want    []Node
		wantErr bool
	}{
		{
			name:    "empty falls back to network defaults",
			nodes:   "",
			network: NetworkNile,
			want:    defaultsByNetwork[NetworkNile].nodes,
		},
		{
			name:    "grpc plaintext",
			nodes:   "grpc://127.0.0.1:50051",
			network: NetworkMainnet,
			want:    []Node{{Address: "127.0.0.1:50051"}},
		},
		{
			name:    "grpcs enables TLS and keeps the host only",
			nodes:   "grpcs://grpc.trongrid.io:50051",
			network: NetworkMainnet,
			want:    []Node{{Address: "grpc.trongrid.io:50051", TLS: true}},
		},
		{
			name:    "http keeps the full URL",
			nodes:   "https://api.trongrid.io",
			network: NetworkMainnet,
			want:    []Node{{Address: "https://api.trongrid.io", HTTP: true}},
		},
		{
			name:    "tier suffix and whitespace",
			nodes:   " grpcs://a:1|0 , https://b|2 ",
			network: NetworkMainnet,
			want: []Node{
				{Address: "a:1", TLS: true, Tier: 0},
				{Address: "https://b", HTTP: true, Tier: 2},
			},
		},
		{
			name:    "unknown scheme",
			nodes:   "ws://node",
			network: NetworkMainnet,
			wantErr: true,
		},
		{
			name:    "missing scheme",
			nodes:   "grpc.trongrid.io:50051",
			network: NetworkMainnet,
			wantErr: true,
		},
		{
			name:    "bad tier",
			nodes:   "grpc://a:1|x",
			network: NetworkMainnet,
			wantErr: true,
		},
		{
			// Sscanf used to stop at the first non-digit and report success, so
			// a typo silently produced a different failover topology.
			name:    "tier with trailing garbage",
			nodes:   "grpc://a:1|1x",
			network: NetworkMainnet,
			wantErr: true,
		},
		{
			name:    "negative tier",
			nodes:   "grpc://a:1|-1",
			network: NetworkMainnet,
			wantErr: true,
		},
		{
			name:    "only separators",
			nodes:   " , ",
			network: NetworkMainnet,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{Network: tt.network, Nodes: tt.nodes}

			got, err := cfg.ParseNodes()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseNodes() = %+v, want an error", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseNodes() error = %v", err)
			}

			if len(got) != len(tt.want) {
				t.Fatalf("ParseNodes() returned %d nodes, want %d: %+v", len(got), len(tt.want), got)
			}

			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("node %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cfg      Config
		wantErr  bool
		wantUSDT string
	}{
		{
			name:     "mainnet fills the default contract",
			cfg:      Config{Network: "MainNet ", FeeLimitTRX: 50},
			wantUSDT: defaultsByNetwork[NetworkMainnet].usdtContract,
		},
		{
			name:     "nile fills its own contract",
			cfg:      Config{Network: NetworkNile, FeeLimitTRX: 50},
			wantUSDT: defaultsByNetwork[NetworkNile].usdtContract,
		},
		{
			name:     "explicit contract is kept",
			cfg:      Config{Network: NetworkMainnet, FeeLimitTRX: 50, USDTContract: "TCustom"},
			wantUSDT: "TCustom",
		},
		{
			name:    "unknown network",
			cfg:     Config{Network: "shasta", FeeLimitTRX: 50},
			wantErr: true,
		},
		{
			name:    "zero fee limit",
			cfg:     Config{Network: NetworkMainnet, FeeLimitTRX: 0},
			wantErr: true,
		},
		{
			name:    "broken node list",
			cfg:     Config{Network: NetworkMainnet, FeeLimitTRX: 50, Nodes: "ws://x"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := tt.cfg

			err := cfg.normalize()
			if tt.wantErr {
				if err == nil {
					t.Fatal("normalize() = nil, want an error")
				}
				return
			}

			if err != nil {
				t.Fatalf("normalize() error = %v", err)
			}

			if cfg.USDTContract != tt.wantUSDT {
				t.Errorf("USDTContract = %q, want %q", cfg.USDTContract, tt.wantUSDT)
			}

			if cfg.ExplorerURL() == "" {
				t.Error("ExplorerURL() is empty after normalize()")
			}
		})
	}
}
