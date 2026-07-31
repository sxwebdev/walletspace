package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/asset"
	"github.com/sxwebdev/walletspace/internal/chain"
	evmchain "github.com/sxwebdev/walletspace/internal/chain/evm"
	tronchain "github.com/sxwebdev/walletspace/internal/chain/tron"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/doctor"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"github.com/sxwebdev/walletspace/internal/space"
)

type Platform struct {
	spaces     *space.Manager
	settings   *config.HomeManager
	networks   *network.Registry
	operations *operation.Store
	assets     *asset.Store
	evm        *evmchain.Adapter
	tron       *tronchain.Adapter
	doctor     *doctor.Doctor
	log        *slog.Logger
}

func NewPlatform(
	spaces *space.Manager,
	settings *config.HomeManager,
	networks *network.Registry,
	operations *operation.Store,
	assets *asset.Store,
	evm *evmchain.Adapter,
	tron *tronchain.Adapter,
	nodeDoctor *doctor.Doctor,
	log *slog.Logger,
) (http.Handler, error) {
	if spaces == nil || settings == nil || networks == nil || operations == nil ||
		assets == nil || evm == nil || tron == nil || nodeDoctor == nil {
		return nil, errors.New("all platform services are required")
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Platform{
		spaces: spaces, settings: settings, networks: networks,
		operations: operations, assets: assets, evm: evm, tron: tron,
		doctor: nodeDoctor, log: log,
	}
	ui, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	fileServer := http.FileServerFS(ui)
	mux.Handle("GET /", fileServer)
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(ui, "index.html")
		if err != nil {
			http.Error(w, "UI unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("GET /api/spaces", p.listSpaces)
	mux.HandleFunc("POST /api/spaces", p.createSpace)
	mux.HandleFunc("GET /api/spaces/{space_id}", p.getSpace)
	mux.HandleFunc("PATCH /api/spaces/{space_id}", p.renameSpace)
	mux.HandleFunc("POST /api/spaces/{space_id}/unlock", p.unlockSpace)
	mux.HandleFunc("POST /api/spaces/{space_id}/lock", p.lockSpace)
	mux.HandleFunc("POST /api/spaces/{space_id}/change-password", p.changePassword)
	mux.HandleFunc("POST /api/spaces/{space_id}/mnemonic", p.revealMnemonic)
	mux.HandleFunc("POST /api/spaces/{space_id}/backup", p.backupSpace)

	mux.HandleFunc("GET /api/spaces/{space_id}/accounts", p.listAccounts)
	mux.HandleFunc("POST /api/spaces/{space_id}/accounts/derive", p.deriveAccount)
	mux.HandleFunc("POST /api/spaces/{space_id}/accounts/import", p.importAccount)
	mux.HandleFunc("PATCH /api/spaces/{space_id}/accounts/{account_id}", p.renameAccount)
	mux.HandleFunc("POST /api/spaces/{space_id}/accounts/{account_id}/networks", p.bindAccountNetwork)
	mux.HandleFunc("POST /api/spaces/{space_id}/accounts/{account_id}/private-key", p.exportPrivateKey)

	mux.HandleFunc("GET /api/networks", p.listNetworks)
	mux.HandleFunc("GET /api/networks/{network_id}/health", p.networkHealth)
	mux.HandleFunc("GET /api/doctor", p.doctorHealth)

	mux.HandleFunc("GET /api/settings", p.getSettings)
	mux.HandleFunc("PATCH /api/settings/general", p.patchGeneral)
	mux.HandleFunc("PATCH /api/settings/security", p.patchSecurity)
	mux.HandleFunc("PATCH /api/settings/node-discovery", p.patchDiscovery)
	mux.HandleFunc("GET /api/settings/networks", p.getNetworkSettings)
	mux.HandleFunc("PUT /api/settings/networks/{network_id}", p.putNetworkSettings)
	mux.HandleFunc("DELETE /api/settings/networks/{network_id}/override", p.deleteNetworkSettings)
	mux.HandleFunc("POST /api/settings/networks/{network_id}/rpc/test", p.testNetworkRPC)
	mux.HandleFunc("GET /api/settings/assets", p.listAssets)
	mux.HandleFunc("POST /api/settings/assets", p.addAsset)
	mux.HandleFunc("DELETE /api/settings/assets/{asset_id}", p.deleteAsset)

	mux.HandleFunc("GET /api/spaces/{space_id}/networks/{network_id}/balances", p.balances)
	mux.HandleFunc("GET /api/spaces/{space_id}/networks/{network_id}/balances/stream", p.balanceStream)
	mux.HandleFunc("POST /api/spaces/{space_id}/networks/{network_id}/transfers/estimate", p.estimateTransfer)
	mux.HandleFunc("POST /api/spaces/{space_id}/networks/{network_id}/transfers", p.sendTransfer)
	mux.HandleFunc("GET /api/spaces/{space_id}/networks/{network_id}/transactions/{tx_id}", p.transaction)
	mux.HandleFunc("GET /api/spaces/{space_id}/networks/{network_id}/accounts/{account_id}/resources", p.tronResources)
	mux.HandleFunc("POST /api/spaces/{space_id}/networks/{network_id}/accounts/{account_id}/stake", p.tronStake)
	mux.HandleFunc("POST /api/spaces/{space_id}/networks/{network_id}/accounts/{account_id}/unstake", p.tronUnstake)
	mux.HandleFunc("POST /api/spaces/{space_id}/networks/{network_id}/accounts/{account_id}/delegate", p.tronDelegate)
	mux.HandleFunc("POST /api/spaces/{space_id}/networks/{network_id}/accounts/{account_id}/reclaim", p.tronReclaim)
	mux.HandleFunc("POST /api/spaces/{space_id}/networks/{network_id}/accounts/{account_id}/withdraw", p.tronWithdraw)
	mux.HandleFunc("POST /api/spaces/{space_id}/networks/{network_id}/accounts/{account_id}/cancel-unstakes", p.tronCancelUnstakes)
	mux.HandleFunc("POST /api/spaces/{space_id}/networks/{network_id}/accounts/{account_id}/deploy-estimate", p.tronDeployEstimate)
	mux.HandleFunc("POST /api/spaces/{space_id}/networks/{network_id}/accounts/{account_id}/deploy", p.tronDeploy)

	return (&Server{}).guard(mux), nil
}

func (p *Platform) listSpaces(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"spaces": p.spaces.List()})
}

func (p *Platform) createSpace(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name            string `json:"name"`
		Mnemonic        string `json:"mnemonic"`
		BIP39Passphrase string `json:"bip39_passphrase"`
		Password        string `json:"password"`
		ImportedOnly    bool   `json:"imported_only"`
		First           bool   `json:"first"`
		// NetworkID is accepted for compatibility with older clients, but space
		// creation never derives a wallet. Wallets are created explicitly later.
		NetworkID string `json:"network_id"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := p.spaces.Create(space.CreateRequest{
		Name: request.Name, Mnemonic: request.Mnemonic,
		BIP39Passphrase: request.BIP39Passphrase, Password: request.Password,
		ImportedOnly: request.ImportedOnly, ExpectEmpty: request.First,
	})
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	response := map[string]any{
		"space": result.Space, "accounts": result.Accounts,
		"mnemonic_generated": result.MnemonicGenerated,
	}
	if result.MnemonicGenerated {
		response["mnemonic"] = result.Mnemonic
		secretHeaders(w)
	}
	writeJSON(w, http.StatusCreated, response)
}

func (p *Platform) getSpace(w http.ResponseWriter, r *http.Request) {
	summary, accounts, err := p.spaces.Get(r.PathValue("space_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"space": summary, "accounts": accounts})
}

func (p *Platform) renameSpace(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := p.spaces.Rename(r.PathValue("space_id"), request.Name)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (p *Platform) unlockSpace(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := p.spaces.Unlock(r.PathValue("space_id"), request.Password); err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"unlocked": true})
}

func (p *Platform) lockSpace(w http.ResponseWriter, r *http.Request) {
	if err := p.spaces.Lock(r.PathValue("space_id")); err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"locked": true})
}

func (p *Platform) changePassword(w http.ResponseWriter, r *http.Request) {
	var request struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := p.spaces.ChangePassword(r.PathValue("space_id"), request.CurrentPassword, request.NewPassword); err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"changed": true})
}

func (p *Platform) revealMnemonic(w http.ResponseWriter, r *http.Request) {
	value, err := p.spaces.Mnemonic(r.PathValue("space_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	secretHeaders(w)
	writeJSON(w, http.StatusOK, map[string]string{"mnemonic": value})
}

func (p *Platform) backupSpace(w http.ResponseWriter, r *http.Request) {
	data, err := p.spaces.Backup(r.PathValue("space_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	secretHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="walletspace-backup.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (p *Platform) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := p.spaces.Accounts(r.PathValue("space_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func (p *Platform) deriveAccount(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Label     string `json:"label"`
		NetworkID string `json:"network_id"`
	}
	if err := decodeOptionalJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, err := p.enabledNetwork(request.NetworkID)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	created, err := p.spaces.Derive(
		r.PathValue("space_id"), item.ID, account.Family(item.Family), request.Label,
	)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (p *Platform) importAccount(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Curve      string `json:"curve"`
		PrivateKey string `json:"private_key"`
		Label      string `json:"label"`
		NetworkID  string `json:"network_id"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Curve != "" && request.Curve != "secp256k1" {
		writeError(w, http.StatusBadRequest, "only secp256k1 private keys are supported")
		return
	}
	item, err := p.enabledNetwork(request.NetworkID)
	if err != nil {
		request.PrivateKey = ""
		p.writePlatformError(w, err)
		return
	}
	result, err := p.spaces.Import(
		r.PathValue("space_id"), item.ID, account.Family(item.Family),
		request.Label, request.PrivateKey,
	)
	request.PrivateKey = ""
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"account": result.Account})
}

func (p *Platform) renameAccount(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Label string `json:"label"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := p.spaces.RenameAccount(r.PathValue("space_id"), r.PathValue("account_id"), request.Label)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (p *Platform) bindAccountNetwork(w http.ResponseWriter, r *http.Request) {
	var request struct {
		NetworkID string `json:"network_id"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, err := p.enabledNetwork(request.NetworkID)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	updated, err := p.spaces.BindNetwork(
		r.PathValue("space_id"), r.PathValue("account_id"),
		item.ID, account.Family(item.Family),
	)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (p *Platform) exportPrivateKey(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Family account.Family `json:"family"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := p.spaces.ExportPrivateKey(
		r.PathValue("space_id"), r.PathValue("account_id"), request.Family,
	)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	secretHeaders(w)
	writeJSON(w, http.StatusOK, map[string]string{"private_key": value, "family": string(request.Family)})
}

func (p *Platform) listNetworks(w http.ResponseWriter, _ *http.Request) {
	items := p.networks.List()
	for i := range items {
		items[i] = p.effectiveNetwork(items[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": items})
}

func (p *Platform) networkHealth(w http.ResponseWriter, r *http.Request) {
	item, err := p.networks.Get(r.PathValue("network_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	item = p.effectiveNetwork(item)
	if !item.Enabled {
		writeJSON(w, http.StatusOK, map[string]any{
			"network_id": item.ID, "status": "disabled",
		})
		return
	}
	snapshot := p.doctor.Snapshot()
	for _, status := range snapshot.Networks {
		if status.NetworkID == item.ID {
			writeJSON(w, http.StatusOK, status)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"network_id": item.ID, "status": "checking",
	})
}

func (p *Platform) doctorHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, p.doctor.Snapshot())
}

type settingsDTO struct {
	SchemaVersion int                   `json:"schema_version"`
	Server        config.ServerSettings `json:"server"`
	Security      struct {
		AutoLock string `json:"auto_lock"`
	} `json:"security"`
	NodeDiscovery struct {
		Enabled          bool   `json:"enabled"`
		URL              string `json:"url"`
		RefreshInterval  string `json:"refresh_interval"`
		RequestTimeout   string `json:"request_timeout"`
		AllowInsecureRPC bool   `json:"allow_insecure_rpc"`
	} `json:"node_discovery"`
	UI              config.UISettings `json:"ui"`
	Revision        string            `json:"revision"`
	RestartRequired []string          `json:"restart_required"`
}

func settingsResponse(snapshot config.SettingsSnapshot) settingsDTO {
	var response settingsDTO
	response.SchemaVersion = snapshot.Config.SchemaVersion
	response.Server = snapshot.Config.Server
	response.Security.AutoLock = snapshot.Config.Security.AutoLock.String()
	response.NodeDiscovery.Enabled = snapshot.Config.NodeDiscovery.Enabled
	response.NodeDiscovery.URL = snapshot.Config.NodeDiscovery.URL
	response.NodeDiscovery.RefreshInterval = snapshot.Config.NodeDiscovery.RefreshInterval.String()
	response.NodeDiscovery.RequestTimeout = snapshot.Config.NodeDiscovery.RequestTimeout.String()
	response.NodeDiscovery.AllowInsecureRPC = snapshot.Config.NodeDiscovery.AllowInsecureRPC
	response.UI = snapshot.Config.UI
	response.Revision = snapshot.Revision
	response.RestartRequired = []string{"server.addr"}
	return response
}

func (p *Platform) getSettings(w http.ResponseWriter, _ *http.Request) {
	snapshot := p.settings.Snapshot()
	w.Header().Set("ETag", `"`+snapshot.Revision+`"`)
	writeJSON(w, http.StatusOK, settingsResponse(snapshot))
}

func (p *Platform) patchGeneral(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Server config.ServerSettings `json:"server"`
		UI     config.UISettings     `json:"ui"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	snapshot := p.settings.Snapshot()
	next := snapshot.Config
	next.Server = request.Server
	next.UI = request.UI
	p.saveSettings(w, next, revisionHeader(r))
}

func (p *Platform) patchSecurity(w http.ResponseWriter, r *http.Request) {
	var request struct {
		AutoLock string `json:"auto_lock"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	duration, err := time.ParseDuration(request.AutoLock)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid auto_lock duration")
		return
	}
	snapshot := p.settings.Snapshot()
	next := snapshot.Config
	next.Security.AutoLock = duration
	saved, err := p.settings.SaveConfig(next, revisionHeader(r))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	if err := p.spaces.SetAutoLock(duration); err != nil {
		p.writePlatformError(w, err)
		return
	}
	w.Header().Set("ETag", `"`+saved.Revision+`"`)
	writeJSON(w, http.StatusOK, settingsResponse(saved))
}

func (p *Platform) patchDiscovery(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled          bool   `json:"enabled"`
		URL              string `json:"url"`
		RefreshInterval  string `json:"refresh_interval"`
		RequestTimeout   string `json:"request_timeout"`
		AllowInsecureRPC bool   `json:"allow_insecure_rpc"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	refresh, err := time.ParseDuration(request.RefreshInterval)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid refresh_interval duration")
		return
	}
	timeout, err := time.ParseDuration(request.RequestTimeout)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request_timeout duration")
		return
	}
	snapshot := p.settings.Snapshot()
	next := snapshot.Config
	next.NodeDiscovery = config.DiscoverySettings{
		Enabled: request.Enabled, URL: strings.TrimSpace(request.URL), RefreshInterval: refresh,
		RequestTimeout: timeout, AllowInsecureRPC: request.AllowInsecureRPC,
	}
	saved, err := p.settings.SaveConfig(next, revisionHeader(r))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	// A changed discovery policy must take effect immediately. Drop both the
	// adapter clients and resolver entries learned under the previous policy.
	for _, item := range p.networks.List() {
		if item.Family == network.FamilyEVM {
			p.evm.Invalidate(item.ID)
		} else {
			p.tron.Invalidate(item.ID)
		}
	}
	p.doctor.Refresh()
	w.Header().Set("ETag", `"`+saved.Revision+`"`)
	writeJSON(w, http.StatusOK, settingsResponse(saved))
}

func (p *Platform) saveSettings(w http.ResponseWriter, next config.HomeConfig, revision string) {
	snapshot, err := p.settings.SaveConfig(next, revision)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	w.Header().Set("ETag", `"`+snapshot.Revision+`"`)
	writeJSON(w, http.StatusOK, settingsResponse(snapshot))
}

func (p *Platform) getNetworkSettings(w http.ResponseWriter, _ *http.Request) {
	snapshot := p.settings.Snapshot()
	w.Header().Set("ETag", `"`+snapshot.Revision+`"`)
	writeJSON(w, http.StatusOK, map[string]any{
		"networks": snapshot.Networks, "revision": snapshot.Revision,
	})
}

func (p *Platform) putNetworkSettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled          *bool             `json:"enabled"`
		RPCURLs          []string          `json:"rpc_urls"`
		Explorer         *network.Explorer `json:"explorer"`
		DiscoveryEnabled *bool             `json:"discovery_enabled"`
		ProviderHeaders  map[string]string `json:"provider_headers"`
		ClearHeaders     bool              `json:"clear_headers"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	headers := request.ProviderHeaders
	if request.ClearHeaders {
		headers = map[string]string{}
	} else if len(headers) == 0 {
		headers = nil
	}
	override := config.NetworkOverride{
		Enabled: request.Enabled, RPCURLs: request.RPCURLs, Explorer: request.Explorer,
		Discovery: request.DiscoveryEnabled, Headers: headers,
	}
	if len(override.RPCURLs) > 0 {
		networkID := r.PathValue("network_id")
		verificationOverride := override
		if verificationOverride.Headers == nil {
			if previous, ok := p.settings.NetworkOverride(networkID); ok {
				verificationOverride.Headers = previous.Headers
			}
		}
		if err := p.settings.ValidateNetwork(networkID, verificationOverride); err != nil {
			p.writePlatformError(w, err)
			return
		}
		item, err := p.networks.Get(networkID)
		if err != nil {
			p.writePlatformError(w, err)
			return
		}
		if err := p.verifyRPCs(r.Context(), item, verificationOverride); err != nil {
			p.writePlatformError(w, err)
			return
		}
	}
	snapshot, err := p.settings.SaveNetwork(r.PathValue("network_id"), override, revisionHeader(r))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	p.evm.Invalidate(r.PathValue("network_id"))
	p.tron.Invalidate(r.PathValue("network_id"))
	p.doctor.Refresh()
	w.Header().Set("ETag", `"`+snapshot.Revision+`"`)
	writeJSON(w, http.StatusOK, map[string]any{"networks": snapshot.Networks, "revision": snapshot.Revision})
}

func (p *Platform) deleteNetworkSettings(w http.ResponseWriter, r *http.Request) {
	snapshot, err := p.settings.DeleteNetworkOverride(r.PathValue("network_id"), revisionHeader(r))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	p.evm.Invalidate(r.PathValue("network_id"))
	p.tron.Invalidate(r.PathValue("network_id"))
	p.doctor.Refresh()
	w.Header().Set("ETag", `"`+snapshot.Revision+`"`)
	writeJSON(w, http.StatusOK, map[string]any{"networks": snapshot.Networks, "revision": snapshot.Revision})
}

func (p *Platform) testNetworkRPC(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RPCURLs         []string          `json:"rpc_urls"`
		ProviderHeaders map[string]string `json:"provider_headers"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	networkID := r.PathValue("network_id")
	override := config.NetworkOverride{
		RPCURLs: request.RPCURLs, Headers: request.ProviderHeaders,
	}
	if err := p.settings.ValidateNetwork(networkID, override); err != nil {
		p.writePlatformError(w, err)
		return
	}
	item, err := p.networks.Get(networkID)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	if err := p.verifyRPCs(r.Context(), item, override); err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "network_id": item.ID})
}

func (p *Platform) verifyRPCs(ctx context.Context, item network.Network, override config.NetworkOverride) error {
	if len(override.RPCURLs) == 0 {
		return errors.New("at least one RPC URL is required")
	}
	timeout := p.settings.Snapshot().Config.NodeDiscovery.RequestTimeout
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	headers := make(http.Header, len(override.Headers))
	for name, value := range override.Headers {
		resolved, err := config.ExpandValue(value)
		if err != nil {
			return err
		}
		headers.Set(name, resolved)
	}
	for _, endpoint := range override.RPCURLs {
		endpoint, expandErr := config.ExpandValue(endpoint)
		if expandErr != nil {
			return expandErr
		}
		checkCtx, cancel := context.WithTimeout(ctx, timeout)
		var err error
		if item.Family == network.FamilyEVM {
			err = p.evm.VerifyEndpoint(checkCtx, item, endpoint, headers)
		} else {
			err = p.tron.VerifyEndpoint(
				checkCtx, item, endpoint, headers.Get("TRON-PRO-API-KEY"),
			)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("verify RPC %s: %w", endpoint, err)
		}
	}
	return nil
}

func (p *Platform) listAssets(w http.ResponseWriter, r *http.Request) {
	item, err := p.networks.Get(r.URL.Query().Get("network_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	item = p.effectiveNetwork(item)
	writeJSON(w, http.StatusOK, map[string]any{"assets": p.assets.List(item)})
}

func (p *Platform) addAsset(w http.ResponseWriter, r *http.Request) {
	var request struct {
		NetworkID string `json:"network_id"`
		Contract  string `json:"contract"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, err := p.networks.Get(request.NetworkID)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	item = p.effectiveNetwork(item)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	var symbol string
	var decimals uint8
	var kind string
	if item.Family == network.FamilyEVM {
		kind = "erc20"
		symbol, decimals, err = p.evm.TokenMetadata(ctx, item.ID, request.Contract)
	} else {
		kind = "trc20"
		symbol, decimals, err = p.tron.TokenMetadata(ctx, item.ID, request.Contract)
	}
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	token := chain.Asset{
		ID: asset.ID(item.ID, kind, request.Contract), NetworkID: item.ID, Kind: kind,
		Name: symbol, Symbol: symbol, Decimals: decimals,
		Contract: request.Contract, Configured: true,
	}
	if err := p.assets.Add(token); err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, token)
}

func (p *Platform) deleteAsset(w http.ResponseWriter, r *http.Request) {
	if err := p.assets.Delete(r.PathValue("asset_id")); err != nil {
		p.writePlatformError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *Platform) balances(w http.ResponseWriter, r *http.Request) {
	results, ok := p.balanceResults(w, r)
	if !ok {
		return
	}
	out := make([]chain.Balance, 0)
	for result := range results {
		out = append(out, result)
	}
	writeJSON(w, http.StatusOK, map[string]any{"balances": out})
}

func (p *Platform) balanceStream(w http.ResponseWriter, r *http.Request) {
	results, ok := p.balanceResults(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	encoder := json.NewEncoder(w)
	for result := range results {
		if err := encoder.Encode(result); err != nil {
			return
		}
		flusher.Flush()
	}
}

func (p *Platform) balanceResults(w http.ResponseWriter, r *http.Request) (<-chan chain.Balance, bool) {
	networkItem, holders, assets, ok := p.networkContext(w, r)
	if !ok {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), balanceStreamTimeout)
	base := p.chainBalanceStream(ctx, networkItem, holders, assets, r.URL.Query().Get("refresh") == "1")
	out := make(chan chain.Balance)
	go func() {
		defer cancel()
		defer close(out)
		for result := range base {
			select {
			case out <- result:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, true
}

type transferDTO struct {
	AccountID string `json:"account_id"`
	AssetID   string `json:"asset_id"`
	To        string `json:"to"`
	Amount    string `json:"amount"`
}

func (p *Platform) estimateTransfer(w http.ResponseWriter, r *http.Request) {
	request, networkItem, transfer, ok := p.decodePlatformTransfer(w, r)
	_ = request
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	estimate, err := p.chainEstimate(ctx, networkItem, transfer)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, estimate)
}

func (p *Platform) sendTransfer(w http.ResponseWriter, r *http.Request) {
	request, networkItem, transfer, ok := p.decodePlatformTransfer(w, r)
	if !ok {
		return
	}
	requestHash, err := operation.RequestHash(struct {
		NetworkID string
		Request   transferDTO
	}{networkItem.ID, request})
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")
	existing, repeated, err := p.operations.Begin(
		r.PathValue("space_id"), idempotencyKey, requestHash, networkItem.ID,
	)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	if repeated {
		if rejectIncompleteReplay(w, existing) {
			return
		}
		writeJSON(w, http.StatusOK, existing)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	var transaction chain.Transaction
	err = p.spaces.WithSigner(ctx, r.PathValue("space_id"), request.AccountID, networkItem.ID,
		account.Family(networkItem.Family), func(signer chain.Signer) error {
			var sendErr error
			transaction, sendErr = p.chainSend(ctx, networkItem, transfer, signer)
			return sendErr
		})
	if err != nil {
		status := "failed"
		if transaction.Hash != "" {
			status = transaction.Status
		}
		updated, updateErr := p.operations.Update(
			r.PathValue("space_id"), idempotencyKey, transaction.Hash, status,
		)
		if updateErr != nil {
			p.writePlatformError(w, updateErr)
			return
		}
		if transaction.Hash != "" && transaction.Status == "broadcast_unknown" {
			writeJSON(w, http.StatusAccepted, updated)
			return
		}
		p.writePlatformError(w, err)
		return
	}
	updated, err := p.operations.Update(
		r.PathValue("space_id"), idempotencyKey, transaction.Hash, transaction.Status,
	)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, updated)
}

func (p *Platform) transaction(w http.ResponseWriter, r *http.Request) {
	item, err := p.networks.Get(r.PathValue("network_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	item = p.effectiveNetwork(item)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	result, err := p.chainTransaction(ctx, item, r.PathValue("tx_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	if _, err := p.operations.UpdateByTxHash(
		r.PathValue("space_id"), result.Hash, result.Status,
	); err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (p *Platform) tronResources(w http.ResponseWriter, r *http.Request) {
	item, _, address, ok := p.tronAccount(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	resources, err := p.tron.Resources(ctx, item.ID, address)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bandwidth":              map[string]string{"available": resources.Bandwidth.Available.String(), "total": resources.Bandwidth.Total.String()},
		"energy":                 map[string]string{"available": resources.Energy.Available.String(), "total": resources.Energy.Total.String()},
		"staked_bandwidth":       resources.StakedBandwidth.String(),
		"staked_energy":          resources.StakedEnergy.String(),
		"unstaking":              resources.Unstaking.String(),
		"withdrawable_now":       resources.WithdrawableNow.String(),
		"bandwidth_per_trx":      resources.BandwidthPerTRX.String(),
		"energy_per_trx":         resources.EnergyPerTRX.String(),
		"can_delegate_bandwidth": resources.CanDelegateBandwidth.String(),
		"can_delegate_energy":    resources.CanDelegateEnergy.String(),
		"unstake_slots":          resources.UnstakeSlots,
		"pending":                resources.Pending,
		"delegations":            resources.Delegations,
	})
}

type tronStakeRequest struct {
	Resource tronchain.Resource `json:"resource"`
	Amount   string             `json:"amount"`
}

func (p *Platform) tronStake(w http.ResponseWriter, r *http.Request) {
	p.tronStakeChange(w, r, false)
}

func (p *Platform) tronUnstake(w http.ResponseWriter, r *http.Request) {
	p.tronStakeChange(w, r, true)
}

func (p *Platform) tronStakeChange(w http.ResponseWriter, r *http.Request, unstake bool) {
	var request tronStakeRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	amount, err := decimal.NewFromString(request.Amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid amount")
		return
	}
	item, accountItem, address, ok := p.tronAccount(w, r)
	if !ok {
		return
	}
	operationKey, ok := p.beginChainOperation(w, r, item.ID, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	var txID string
	err = p.spaces.WithSigner(ctx, r.PathValue("space_id"), accountItem.ID, item.ID, account.FamilyTron,
		func(signer chain.Signer) error {
			var operationErr error
			if unstake {
				txID, operationErr = p.tron.Unstake(ctx, item.ID, address, request.Resource, amount, signer)
			} else {
				txID, operationErr = p.tron.Stake(ctx, item.ID, address, request.Resource, amount, signer)
			}
			return operationErr
		})
	if err != nil {
		if _, updateErr := p.operations.Update(r.PathValue("space_id"), operationKey, txID, "failed"); updateErr != nil {
			p.log.Error("persist failed Tron operation", "error", updateErr)
		}
		p.writePlatformError(w, err)
		return
	}
	if _, err := p.operations.Update(r.PathValue("space_id"), operationKey, txID, "pending"); err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"network_id": item.ID, "tx_id": txID, "status": "pending"})
}

type tronDelegationRequest struct {
	Resource tronchain.Resource `json:"resource"`
	Amount   string             `json:"amount"`
	To       string             `json:"to"`
	All      bool               `json:"all"`
}

func (p *Platform) tronDelegate(w http.ResponseWriter, r *http.Request) {
	p.tronDelegation(w, r, false)
}

func (p *Platform) tronReclaim(w http.ResponseWriter, r *http.Request) {
	p.tronDelegation(w, r, true)
}

func (p *Platform) tronDelegation(w http.ResponseWriter, r *http.Request, reclaim bool) {
	var request tronDelegationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	amount := decimal.Zero
	var err error
	if !request.All {
		amount, err = decimal.NewFromString(request.Amount)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid amount")
			return
		}
	}
	item, accountItem, address, ok := p.tronAccount(w, r)
	if !ok {
		return
	}
	operationKey, ok := p.beginChainOperation(w, r, item.ID, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	var txID string
	err = p.spaces.WithSigner(ctx, r.PathValue("space_id"), accountItem.ID, item.ID, account.FamilyTron,
		func(signer chain.Signer) error {
			var operationErr error
			if reclaim {
				txID, operationErr = p.tron.Reclaim(
					ctx, item.ID, address, request.To, request.Resource, amount, request.All, signer,
				)
			} else {
				txID, operationErr = p.tron.Delegate(
					ctx, item.ID, address, request.To, request.Resource, amount, signer,
				)
			}
			return operationErr
		})
	if err != nil {
		if _, updateErr := p.operations.Update(r.PathValue("space_id"), operationKey, txID, "failed"); updateErr != nil {
			p.log.Error("persist failed Tron operation", "error", updateErr)
		}
		p.writePlatformError(w, err)
		return
	}
	if _, err := p.operations.Update(r.PathValue("space_id"), operationKey, txID, "pending"); err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"network_id": item.ID, "tx_id": txID, "status": "pending"})
}

func (p *Platform) tronWithdraw(w http.ResponseWriter, r *http.Request) {
	p.tronWithdrawChange(w, r, false)
}

func (p *Platform) tronCancelUnstakes(w http.ResponseWriter, r *http.Request) {
	p.tronWithdrawChange(w, r, true)
}

func (p *Platform) tronWithdrawChange(w http.ResponseWriter, r *http.Request, cancelUnstakes bool) {
	item, accountItem, address, ok := p.tronAccount(w, r)
	if !ok {
		return
	}
	operationKey, ok := p.beginChainOperation(w, r, item.ID, struct {
		CancelUnstakes bool `json:"cancel_unstakes"`
	}{CancelUnstakes: cancelUnstakes})
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	var txID string
	err := p.spaces.WithSigner(ctx, r.PathValue("space_id"), accountItem.ID, item.ID, account.FamilyTron,
		func(signer chain.Signer) error {
			var operationErr error
			txID, operationErr = p.tron.Withdraw(ctx, item.ID, address, cancelUnstakes, signer)
			return operationErr
		})
	if err != nil {
		if _, updateErr := p.operations.Update(r.PathValue("space_id"), operationKey, txID, "failed"); updateErr != nil {
			p.log.Error("persist failed Tron operation", "error", updateErr)
		}
		p.writePlatformError(w, err)
		return
	}
	if _, err := p.operations.Update(r.PathValue("space_id"), operationKey, txID, "pending"); err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"network_id": item.ID, "tx_id": txID, "status": "pending"})
}

type tronDeployRequest struct {
	Name                       string `json:"name"`
	ABI                        string `json:"abi"`
	Bytecode                   string `json:"bytecode"`
	ConstructorParams          string `json:"constructor_params"`
	FeeLimit                   string `json:"fee_limit"`
	ConsumeUserResourcePercent int64  `json:"consume_user_resource_percent"`
	OriginEnergyLimit          int64  `json:"origin_energy_limit"`
}

func (p *Platform) tronDeployEstimate(w http.ResponseWriter, r *http.Request) {
	request, deployment, item, _, address, ok := p.decodeTronDeployment(w, r)
	_ = request
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), deployTimeout)
	defer cancel()
	cost, err := p.tron.EstimateDeploy(ctx, item.ID, address, deployment)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"energy": cost.Energy.String(), "bandwidth": cost.Bandwidth.String(),
		"fee": cost.Fee.String(), "min_fee_limit": cost.MinFeeLimit.String(),
		"shortfall": cost.Shortfall.String(),
	})
}

func (p *Platform) tronDeploy(w http.ResponseWriter, r *http.Request) {
	request, deployment, item, accountItem, address, ok := p.decodeTronDeployment(w, r)
	if !ok {
		return
	}
	operationKey, ok := p.beginChainOperation(w, r, item.ID, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), deployTimeout)
	defer cancel()
	var deployed tronchain.Deployed
	err := p.spaces.WithSigner(ctx, r.PathValue("space_id"), accountItem.ID, item.ID, account.FamilyTron,
		func(signer chain.Signer) error {
			var deployErr error
			deployed, deployErr = p.tron.Deploy(ctx, item.ID, address, deployment, signer)
			return deployErr
		})
	if err != nil {
		if _, updateErr := p.operations.Update(r.PathValue("space_id"), operationKey, deployed.TxID, "failed"); updateErr != nil {
			p.log.Error("persist failed Tron deploy", "error", updateErr)
		}
		p.writePlatformError(w, err)
		return
	}
	status := "pending"
	if deployed.Confirmed {
		status = "confirmed"
	}
	if deployed.Failure != "" {
		status = "failed"
	}
	if _, err := p.operations.Update(r.PathValue("space_id"), operationKey, deployed.TxID, status); err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"network_id": item.ID, "tx_id": deployed.TxID, "address": deployed.Address,
		"confirmed": deployed.Confirmed, "failure": deployed.Failure,
		"energy_used": deployed.EnergyUsed, "fee": deployed.Fee.String(),
	})
}

func (p *Platform) beginChainOperation(
	w http.ResponseWriter,
	r *http.Request,
	networkID string,
	request any,
) (string, bool) {
	requestHash, err := operation.RequestHash(struct {
		Path    string `json:"path"`
		Request any    `json:"request"`
	}{Path: r.URL.Path, Request: request})
	if err != nil {
		p.writePlatformError(w, err)
		return "", false
	}
	key := r.Header.Get("Idempotency-Key")
	existing, repeated, err := p.operations.Begin(
		r.PathValue("space_id"), key, requestHash, networkID,
	)
	if err != nil {
		p.writePlatformError(w, err)
		return "", false
	}
	if repeated {
		if rejectIncompleteReplay(w, existing) {
			return "", false
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"network_id": existing.NetworkID,
			"tx_id":      existing.TxHash,
			"status":     existing.Status,
		})
		return "", false
	}
	return key, true
}

func rejectIncompleteReplay(w http.ResponseWriter, existing operation.Operation) bool {
	if existing.TxHash != "" {
		return false
	}
	if existing.Status == "building" {
		writeError(w, http.StatusConflict, "operation is still in progress")
	} else {
		writeError(w, http.StatusConflict, "previous operation failed before broadcast; retry with a new idempotency key")
	}
	return true
}

func (p *Platform) decodeTronDeployment(
	w http.ResponseWriter, r *http.Request,
) (tronDeployRequest, tronchain.Deployment, network.Network, account.Account, string, bool) {
	var request tronDeployRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return request, tronchain.Deployment{}, network.Network{}, account.Account{}, "", false
	}
	feeLimit, err := decimal.NewFromString(request.FeeLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid fee_limit")
		return request, tronchain.Deployment{}, network.Network{}, account.Account{}, "", false
	}
	item, accountItem, address, ok := p.tronAccount(w, r)
	if !ok {
		return request, tronchain.Deployment{}, network.Network{}, account.Account{}, "", false
	}
	return request, tronchain.Deployment{
		Name: request.Name, ABI: request.ABI, Bytecode: request.Bytecode,
		ConstructorParams: request.ConstructorParams, FeeLimit: feeLimit,
		ConsumeUserResourcePercent: request.ConsumeUserResourcePercent,
		OriginEnergyLimit:          request.OriginEnergyLimit,
	}, item, accountItem, address, true
}

func (p *Platform) tronAccount(
	w http.ResponseWriter, r *http.Request,
) (network.Network, account.Account, string, bool) {
	item, err := p.networks.Get(r.PathValue("network_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return network.Network{}, account.Account{}, "", false
	}
	item = p.effectiveNetwork(item)
	if item.Family != network.FamilyTron || !item.Enabled {
		writeError(w, http.StatusBadRequest, "Tron operation requires an enabled Tron network")
		return network.Network{}, account.Account{}, "", false
	}
	accounts, err := p.spaces.Accounts(r.PathValue("space_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return network.Network{}, account.Account{}, "", false
	}
	for _, itemAccount := range accounts {
		if itemAccount.ID == r.PathValue("account_id") && itemAccount.BoundTo(item.ID) {
			address := itemAccount.Addresses[account.FamilyTron]
			if address != "" {
				return item, itemAccount, address, true
			}
		}
	}
	writeError(w, http.StatusNotFound, "account is not available for Tron")
	return network.Network{}, account.Account{}, "", false
}

func (p *Platform) networkContext(
	w http.ResponseWriter,
	r *http.Request,
) (network.Network, []chain.AccountAddress, []chain.Asset, bool) {
	item, err := p.networks.Get(r.PathValue("network_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return network.Network{}, nil, nil, false
	}
	item = p.effectiveNetwork(item)
	if !item.Enabled {
		writeError(w, http.StatusBadRequest, "network is disabled")
		return network.Network{}, nil, nil, false
	}
	accounts, err := p.spaces.Accounts(r.PathValue("space_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return network.Network{}, nil, nil, false
	}
	family := account.Family(item.Family)
	networkID := item.ID
	holders := make([]chain.AccountAddress, 0, len(accounts))
	filter := make(map[string]struct{})
	for _, id := range r.URL.Query()["account_id"] {
		filter[id] = struct{}{}
	}
	for _, accountItem := range accounts {
		if len(filter) > 0 {
			if _, selected := filter[accountItem.ID]; !selected {
				continue
			}
		}
		if !accountItem.BoundTo(networkID) {
			continue
		}
		address := accountItem.Addresses[family]
		if address != "" {
			holders = append(holders, chain.AccountAddress{AccountID: accountItem.ID, Address: address})
		}
	}
	return item, holders, p.assets.List(item), true
}

func (p *Platform) decodePlatformTransfer(
	w http.ResponseWriter,
	r *http.Request,
) (transferDTO, network.Network, chain.TransferRequest, bool) {
	var request transferDTO
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return transferDTO{}, network.Network{}, chain.TransferRequest{}, false
	}
	item, err := p.networks.Get(r.PathValue("network_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return transferDTO{}, network.Network{}, chain.TransferRequest{}, false
	}
	item = p.effectiveNetwork(item)
	if !item.Enabled {
		writeError(w, http.StatusBadRequest, "network is disabled")
		return transferDTO{}, network.Network{}, chain.TransferRequest{}, false
	}
	accounts, err := p.spaces.Accounts(r.PathValue("space_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return transferDTO{}, network.Network{}, chain.TransferRequest{}, false
	}
	var from string
	for _, candidate := range accounts {
		if candidate.ID == request.AccountID && candidate.BoundTo(item.ID) {
			from = candidate.Addresses[account.Family(item.Family)]
			break
		}
	}
	if from == "" {
		writeError(w, http.StatusNotFound, "account is not available for this network")
		return transferDTO{}, network.Network{}, chain.TransferRequest{}, false
	}
	asset, ok := findAsset(p.assets.List(item), request.AssetID)
	if !ok {
		writeError(w, http.StatusBadRequest, "asset does not belong to this network")
		return transferDTO{}, network.Network{}, chain.TransferRequest{}, false
	}
	return request, item, chain.TransferRequest{
		AccountID: request.AccountID, From: from, To: request.To, Asset: asset, Amount: request.Amount,
	}, true
}

func findAsset(assets []chain.Asset, id string) (chain.Asset, bool) {
	for _, asset := range assets {
		if asset.ID == id {
			return asset, true
		}
	}
	return chain.Asset{}, false
}

func (p *Platform) effectiveNetwork(item network.Network) network.Network {
	override, ok := p.settings.NetworkOverride(item.ID)
	if !ok {
		return item
	}
	if override.Enabled != nil {
		item.Enabled = *override.Enabled
	}
	if override.Explorer != nil {
		if override.Explorer.Address != "" {
			item.Explorer.Address = override.Explorer.Address
		}
		if override.Explorer.Tx != "" {
			item.Explorer.Tx = override.Explorer.Tx
		}
		if override.Explorer.Block != "" {
			item.Explorer.Block = override.Explorer.Block
		}
	}
	return item
}

func (p *Platform) enabledNetwork(id string) (network.Network, error) {
	if id == "" {
		return network.Network{}, errors.New("network_id is required")
	}
	item, err := p.networks.Get(id)
	if err != nil {
		return network.Network{}, err
	}
	item = p.effectiveNetwork(item)
	if !item.Enabled {
		return network.Network{}, errors.New("network is disabled")
	}
	return item, nil
}

func (p *Platform) chainBalanceStream(
	ctx context.Context, item network.Network, holders []chain.AccountAddress, assets []chain.Asset, refresh bool,
) <-chan chain.Balance {
	if item.Family == network.FamilyEVM {
		return p.evm.BalanceStream(ctx, item.ID, holders, assets)
	}
	return p.tron.BalanceStream(ctx, item.ID, holders, assets, refresh)
}

func (p *Platform) chainEstimate(ctx context.Context, item network.Network, request chain.TransferRequest) (chain.TransferEstimate, error) {
	if item.Family == network.FamilyEVM {
		return p.evm.EstimateTransfer(ctx, item.ID, request)
	}
	return p.tron.EstimateTransfer(ctx, item.ID, request)
}

func (p *Platform) chainSend(
	ctx context.Context, item network.Network, request chain.TransferRequest, signer chain.Signer,
) (chain.Transaction, error) {
	if item.Family == network.FamilyEVM {
		return p.evm.Send(ctx, item.ID, request, signer)
	}
	return p.tron.Send(ctx, item.ID, request, signer)
}

func (p *Platform) chainTransaction(ctx context.Context, item network.Network, txID string) (chain.Transaction, error) {
	if item.Family == network.FamilyEVM {
		return p.evm.Transaction(ctx, item.ID, txID)
	}
	return p.tron.Transaction(ctx, item.ID, txID)
}

func (p *Platform) writePlatformError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, space.ErrNotFound), errors.Is(err, space.ErrAccountNotFound),
		errors.Is(err, space.ErrNetworkBinding), errors.Is(err, network.ErrUnknownNetwork):
		status = http.StatusNotFound
	case errors.Is(err, space.ErrLocked):
		status = http.StatusLocked
	case errors.Is(err, space.ErrDuplicateKey), errors.Is(err, space.ErrFirstSpaceExists),
		errors.Is(err, operation.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, config.ErrRevisionConflict):
		status = http.StatusPreconditionFailed
	case errors.Is(err, config.ErrInvalidSettings):
		status = http.StatusBadRequest
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	default:
		if !errors.Is(err, account.ErrInvalidMnemonic) &&
			!errors.Is(err, account.ErrInvalidPrivateKey) &&
			!errors.Is(err, chain.ErrInvalidRequest) &&
			!errors.Is(err, space.ErrWeakPassword) &&
			!strings.Contains(err.Error(), "required") &&
			!strings.Contains(err.Error(), "invalid") {
			status = http.StatusBadGateway
			p.log.Warn("platform request failed", "error", err)
		}
	}
	writeError(w, status, err.Error())
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid JSON body")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON body")
	}
	return nil
}

func decodeOptionalJSON(r *http.Request, target any) error {
	if r.ContentLength == 0 {
		return nil
	}
	return decodeJSON(r, target)
}

func secretHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

func revisionHeader(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("If-Match"))
	return strings.Trim(value, `"`)
}
