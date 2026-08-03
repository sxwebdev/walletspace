package tron

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/sxwebdev/walletspace/internal/config"
)

// The Tron client used to be handed nothing but an address, so it built its own
// HTTP client and dialled gRPC itself — the guarded transport that resolves the
// host and refuses private addresses was given to the endpoint probe and to
// nothing else. Every request after that check left the filtered path.
func TestNodeConfigsCarryTheGuardedTransport(t *testing.T) {
	t.Parallel()

	guarded := &http.Client{}
	refused := errors.New("RPC resolved only to private or unsafe addresses")
	dial := func(context.Context, string) (net.Conn, error) { return nil, refused }

	got, keyed := nodeConfigs(&config.Config{}, []config.Node{
		{Address: "https://http.example", HTTP: true, HTTPClient: guarded, DialContext: dial},
		{Address: "grpc.example:50051", DialContext: dial},
	})
	if len(got) != 2 {
		t.Fatalf("nodeConfigs() returned %d nodes", len(got))
	}
	if keyed {
		t.Error("nodeConfigs() reported an authenticated node where none is configured")
	}
	if got[0].HTTPClient != guarded {
		t.Error("HTTP node did not receive the guarded client")
	}
	if len(got[1].DialOptions) != 1 {
		t.Fatalf("gRPC node dial options = %#v, want the guarded dialer", got[1].DialOptions)
	}
}

// A provider key belongs to the node it was configured for. Before endpoints
// carried their own credentials there was one key per network and it went to
// every node in the list, including official fallbacks and discovered nodes.
func TestNodeConfigsKeepCredentialsOnTheirOwnNode(t *testing.T) {
	t.Parallel()

	got, keyed := nodeConfigs(&config.Config{}, []config.Node{
		{
			Address: "https://paid.example", HTTP: true,
			Headers: map[string]string{"TRON-PRO-API-KEY": "secret"},
		},
		{Address: "https://public.example", HTTP: true},
	})
	if !keyed {
		t.Error("nodeConfigs() did not notice the authenticated node")
	}
	if got[0].Headers["TRON-PRO-API-KEY"] != "secret" {
		t.Errorf("configured node headers = %#v", got[0].Headers)
	}
	if len(got[1].Headers) != 0 {
		t.Errorf("credential spread to an unconfigured node: %#v", got[1].Headers)
	}
}

// The standalone CLI has one network and one key, so there the key does belong
// to every node it was given.
func TestNodeConfigsStillApplyTheCLIAPIKey(t *testing.T) {
	t.Parallel()

	got, keyed := nodeConfigs(&config.Config{APIKey: "cli-key"}, []config.Node{
		{Address: "https://one.example", HTTP: true},
		{Address: "https://two.example", HTTP: true},
	})
	if !keyed {
		t.Error("nodeConfigs() did not report the CLI key")
	}
	for i, node := range got {
		if node.Headers["TRON-PRO-API-KEY"] != "cli-key" {
			t.Errorf("node %d headers = %#v", i, node.Headers)
		}
	}
}
