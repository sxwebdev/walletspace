package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/space"
)

// RFC 8032 §7.1 test 2 as a Solana keypair, in both export formats
// (tweetnacl 1.0.3 + bs58 6.0.0).
const (
	solanaKeypair = "2Y4QjyJVZf9tTmTPP1SY9ACpFYTo7brW9iCQ8SunQht5yQ2r1U9KsVv5aMsCGnzj3NR8KG9P3NY7FKBiYbbTJ2no"
	solanaAddress = "586Z7H2vpX9qNhN2T4e9Utugie3ogjbxzGaMtM3E6HR5"
	solanaCLIJSON = "[76,205,8,155,40,255,150,218,157,182,195,70,236,17,78,15,91,138,49,159,53,171,166,36,218,140,246,237,79,184,166,251," +
		"61,64,23,195,232,67,137,90,146,183,10,167,77,27,126,188,156,152,44,207,46,196,150,140,192,205,85,241,42,244,102,12]"
)

func TestPlatformImportsAndExportsSolanaKeys(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	created, err := fixture.spaces.Create(space.CreateRequest{
		Password: "test-vault-password",
	})
	if err != nil {
		t.Fatalf("spaces.Create() error = %v", err)
	}
	importPath := "/api/spaces/" + created.Space.ID + "/accounts/import"

	imported := platformRequest(t, fixture.handler, http.MethodPost, importPath, map[string]any{
		"curve": "ed25519", "private_key": solanaCLIJSON, "label": "keygen", "network_id": "solana-devnet",
	})
	if imported.Code != http.StatusCreated {
		t.Fatalf("import(json) = %d %s", imported.Code, imported.Body.String())
	}
	var payload struct {
		Account struct {
			ID        string            `json:"id"`
			Addresses map[string]string `json:"addresses"`
		} `json:"account"`
	}
	if err := json.Unmarshal(imported.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode import: %v", err)
	}
	if len(payload.Account.Addresses) != 1 || payload.Account.Addresses["solana"] != solanaAddress {
		t.Fatalf("imported addresses = %v, want solana %s", payload.Account.Addresses, solanaAddress)
	}
	if strings.Contains(imported.Body.String(), solanaKeypair) || strings.Contains(imported.Body.String(), "205,8,155") {
		t.Fatal("the import answer carries key material")
	}
	// The base58 form of the same key, without a stated curve, binds it to
	// mainnet.
	bound := platformRequest(t, fixture.handler, http.MethodPost, importPath, map[string]any{
		"private_key": solanaKeypair, "network_id": "solana-mainnet",
	})
	if bound.Code != http.StatusCreated || !strings.Contains(bound.Body.String(), payload.Account.ID) {
		t.Fatalf("import(base58) = %d %s", bound.Code, bound.Body.String())
	}

	exportPath := "/api/spaces/" + created.Space.ID + "/accounts/" + payload.Account.ID + "/private-key"
	exported := platformRequest(t, fixture.handler, http.MethodPost, exportPath,
		map[string]string{"family": "solana", "password": "test-vault-password"})
	if exported.Code != http.StatusOK {
		t.Fatalf("export = %d %s", exported.Code, exported.Body.String())
	}
	var secret struct {
		PrivateKey string `json:"private_key"`
		Family     string `json:"family"`
	}
	if err := json.Unmarshal(exported.Body.Bytes(), &secret); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if secret.PrivateKey != solanaKeypair || secret.Family != "solana" {
		t.Fatalf("export = %+v, want the base58 keypair", secret)
	}
	if exported.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("export Cache-Control = %q", exported.Header().Get("Cache-Control"))
	}
	wrongFamily := platformRequest(t, fixture.handler, http.MethodPost, exportPath,
		map[string]string{"family": "evm", "password": "test-vault-password"})
	if wrongFamily.Code != http.StatusNotFound || strings.Contains(wrongFamily.Body.String(), "private_key") {
		t.Fatalf("export(ed25519 as evm) = %d %s", wrongFamily.Code, wrongFamily.Body.String())
	}
}

func TestPlatformImportCurveMustMatchTheNetwork(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	created, err := fixture.spaces.Create(space.CreateRequest{
		Password: "test-vault-password",
	})
	if err != nil {
		t.Fatalf("spaces.Create() error = %v", err)
	}
	importPath := "/api/spaces/" + created.Space.ID + "/accounts/import"
	secp := "0000000000000000000000000000000000000000000000000000000000000001"

	tests := []struct {
		name      string
		curve     string
		key       string
		networkID string
		want      int
	}{
		{"ed25519 on evm", "ed25519", solanaKeypair, "ethereum-mainnet", http.StatusBadRequest},
		{"ed25519 on tron", "ed25519", solanaKeypair, "tron-nile", http.StatusBadRequest},
		{"secp256k1 on solana", "secp256k1", secp, "solana-devnet", http.StatusBadRequest},
		// A stated curve that disagrees with the network is refused even
		// when the key would parse for the network's own curve.
		{"ed25519 stated for a valid evm key", "ed25519", secp, "ethereum-mainnet", http.StatusBadRequest},
		{"secp256k1 stated for a valid keypair", "secp256k1", solanaKeypair, "solana-devnet", http.StatusBadRequest},
		{"unknown curve", "sr25519", solanaKeypair, "solana-devnet", http.StatusBadRequest},
		{"keypair on evm without a curve", "", solanaKeypair, "ethereum-mainnet", http.StatusBadRequest},
		{"hex on solana without a curve", "", secp, "solana-devnet", http.StatusBadRequest},
		{"mismatched halves", "ed25519", solanaKeypair[:len(solanaKeypair)-1] + "3", "solana-devnet", http.StatusBadRequest},
		{"unknown network", "ed25519", solanaKeypair, "solana-testnet", http.StatusNotFound},
		{"secp256k1 on evm", "secp256k1", secp, "ethereum-mainnet", http.StatusCreated},
		{"secp256k1 on tron without a curve", "", secp, "tron-nile", http.StatusCreated},
	}
	for _, tt := range tests {
		response := platformRequest(t, fixture.handler, http.MethodPost, importPath, map[string]any{
			"curve": tt.curve, "private_key": tt.key, "network_id": tt.networkID,
		})
		if response.Code != tt.want {
			t.Errorf("%s: import = %d %s, want %d", tt.name, response.Code, response.Body.String(), tt.want)
		}
		if strings.Contains(response.Body.String(), tt.key) {
			t.Errorf("%s: the answer quotes the key", tt.name)
		}
	}
	accounts, err := fixture.spaces.Accounts(created.Space.ID)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts = %d, %v; want only the one secp256k1 key", len(accounts), err)
	}
}
