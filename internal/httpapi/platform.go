package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
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
	"github.com/sxwebdev/walletspace/internal/price"
	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/vault"
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
	prices     price.Provider
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
	prices price.Provider,
	access Access,
	log *slog.Logger,
) (http.Handler, error) {
	if spaces == nil || settings == nil || networks == nil || operations == nil ||
		assets == nil || evm == nil || tron == nil || nodeDoctor == nil || prices == nil {
		return nil, errors.New("all platform services are required")
	}
	// Without these the guard would fall open, so refuse to build a handler at
	// all rather than serve spendable keys behind a boundary that is not there.
	if access.Token == "" || len(access.Hosts) == 0 {
		return nil, errors.New("a capability token and at least one allowed host are required")
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Platform{
		spaces: spaces, settings: settings, networks: networks,
		operations: operations, assets: assets, evm: evm, tron: tron,
		doctor: nodeDoctor, prices: prices, log: log,
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
	mux.HandleFunc("POST /api/spaces/{space_id}/confirm-send", p.confirmSend)

	mux.HandleFunc("GET /api/spaces/{space_id}/accounts", p.listAccounts)
	mux.HandleFunc("POST /api/spaces/{space_id}/accounts/derive", p.deriveAccount)
	mux.HandleFunc("POST /api/spaces/{space_id}/accounts/import", p.importAccount)
	mux.HandleFunc("PATCH /api/spaces/{space_id}/accounts/{account_id}", p.renameAccount)
	mux.HandleFunc("POST /api/spaces/{space_id}/accounts/{account_id}/networks", p.bindAccountNetwork)
	mux.HandleFunc("POST /api/spaces/{space_id}/accounts/{account_id}/private-key", p.exportPrivateKey)

	mux.HandleFunc("GET /api/networks", p.listNetworks)
	mux.HandleFunc("GET /api/networks/{network_id}/health", p.networkHealth)
	mux.HandleFunc("GET /api/doctor", p.doctorHealth)
	mux.HandleFunc("GET /api/prices", p.assetPrices)

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

	return access.guard(mux), nil
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
	var request struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := p.spaces.Mnemonic(r.PathValue("space_id"), request.Password)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	secretHeaders(w)
	writeJSON(w, http.StatusOK, map[string]string{"mnemonic": value})
}

func (p *Platform) backupSpace(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := p.spaces.Backup(r.PathValue("space_id"), request.Password)
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

// confirmSend opens the window in which this space may spend.
//
// Unlocking proves who was at the keyboard when the space was opened. It says
// nothing about who is asking now — a script injected into the page and any
// local process holding the capability token both inherit an unlocked space.
// This is the one thing neither of them can produce.
func (p *Platform) confirmSend(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The request's own context, so a dialog the person dismisses while the
	// derivation is still running does not leave a window open behind a browser
	// that has already reported nothing was confirmed.
	expires, err := p.spaces.ConfirmSend(r.Context(), r.PathValue("space_id"), request.Password)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	secretHeaders(w)
	writeJSON(w, http.StatusOK, map[string]string{"expires_at": expires.Format(time.RFC3339)})
}

// requireSendConfirmation gates everything that moves funds.
//
// Every refusal goes through the shared mapper, including the one this gate
// exists to raise. Building that answer by hand here meant the status and the
// code lived at the call site: moving the check anywhere else — into a helper,
// down into an adapter — produced the mapper's default branch instead, which is
// 502 with a "platform request failed" line in the log and no code for the UI
// to act on.
func (p *Platform) requireSendConfirmation(w http.ResponseWriter, r *http.Request) bool {
	if err := p.spaces.RequireSendConfirmation(r.PathValue("space_id")); err != nil {
		p.writePlatformError(w, err)
		return false
	}
	return true
}

// codeSendConfirmationRequired is the contract between the gate above and the
// prompt in the UI. Matching on the message text would be a string comparison
// that breaks the moment the wording improves.
const codeSendConfirmationRequired = "send_confirmation_required"

// statusClientClosedRequest is 499, which the standard library has no name for
// because it is not in the RFC. It says what happened when a caller goes away
// mid-request, and the alternatives all say something false: 200 that the work
// was done, 504 that a node was slow, 502 that the wallet broke.
const statusClientClosedRequest = 499

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
		Family   account.Family `json:"family"`
		Password string         `json:"password"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := p.spaces.ExportPrivateKey(
		r.PathValue("space_id"), r.PathValue("account_id"), request.Family, request.Password,
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
		AutoLock     string `json:"auto_lock"`
		ConfirmSends bool   `json:"confirm_sends"`
		SendGrantTTL string `json:"send_grant_ttl"`
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
	response.Security.ConfirmSends = snapshot.Config.Security.ConfirmSends
	response.Security.SendGrantTTL = snapshot.Config.Security.SendGrantTTL.String()
	response.NodeDiscovery.Enabled = snapshot.Config.NodeDiscovery.Enabled
	response.NodeDiscovery.URL = snapshot.Config.NodeDiscovery.URL
	response.NodeDiscovery.RefreshInterval = snapshot.Config.NodeDiscovery.RefreshInterval.String()
	response.NodeDiscovery.RequestTimeout = snapshot.Config.NodeDiscovery.RequestTimeout.String()
	response.NodeDiscovery.AllowInsecureRPC = snapshot.Config.NodeDiscovery.AllowInsecureRPC
	response.UI = snapshot.Config.UI
	response.Revision = snapshot.Revision
	// The fields the API will not change while the process runs, named so the
	// form can present them as read-only instead of offering a control whose
	// save is always refused. They are here for different reasons: a listen
	// address cannot move under a running listener, while confirm_sends must not
	// be movable by the caller the spending step-up exists to stop.
	response.RestartRequired = []string{"server.addr", "security.confirm_sends"}
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
	snapshot := p.settings.Snapshot()
	// Defaulted from what is stored, so a client that sends only the field it
	// changed does not silently switch the spending step-up off.
	request := struct {
		AutoLock     string `json:"auto_lock"`
		ConfirmSends *bool  `json:"confirm_sends"`
		SendGrantTTL string `json:"send_grant_ttl"`
	}{AutoLock: snapshot.Config.Security.AutoLock.String()}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// confirm_sends is restart-only, and refused here in both directions.
	//
	// The step-up exists to stop anything that merely holds the capability
	// token — a script injected into the page, another local process — from
	// spending an unlocked space. A switch that same caller can flip with one
	// PATCH is not a control: refused transfer, PATCH, identical transfer
	// accepted, funds gone. Asking for a password would not close it, since the
	// setting is global and belongs to no space, and a caller with the token can
	// create a space whose password it chose. So config.yaml is the only way to
	// change it, on the same footing as an address that cannot move under a
	// running listener — and one field above, auto-lock cannot be switched off
	// at all.
	//
	// A PATCH that echoes the stored value back is accepted rather than refused:
	// the settings form posts the whole security block, so treating the field's
	// presence as the offence would break every save of the two beside it.
	if request.ConfirmSends != nil && *request.ConfirmSends != snapshot.Config.Security.ConfirmSends {
		writeError(w, http.StatusForbidden,
			"confirm_sends cannot be changed through the API: edit security.confirm_sends "+
				"in config.yaml and restart Walletspace")
		return
	}
	duration, err := time.ParseDuration(request.AutoLock)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid auto_lock duration")
		return
	}
	grantTTL := snapshot.Config.Security.SendGrantTTL
	if request.SendGrantTTL != "" {
		grantTTL, err = time.ParseDuration(request.SendGrantTTL)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid send_grant_ttl duration")
			return
		}
	}
	// ConfirmSends is not assigned from the request: it can only have arrived
	// equal to what is stored, and carrying the stored value forward is what
	// keeps the file the single authority on it.
	next := snapshot.Config
	next.Security.AutoLock = duration
	next.Security.SendGrantTTL = grantTTL
	saved, err := p.settings.SaveConfig(next, revisionHeader(r))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	if err := p.spaces.SetAutoLock(duration); err != nil {
		p.writePlatformError(w, err)
		return
	}
	p.spaces.SetSendConfirmation(saved.Config.Security.ConfirmSends, saved.Config.Security.SendGrantTTL)
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

// endpointDTO is one RPC endpoint as the UI sends it back.
//
// A nil Headers map — the field absent from the JSON — means "leave whatever is
// stored for this endpoint alone", which is what the UI sends for an endpoint
// whose secret it was never shown. An empty object means "delete it".
type endpointDTO struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

func (p *Platform) putNetworkSettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled          *bool             `json:"enabled"`
		Endpoints        []endpointDTO     `json:"endpoints"`
		Explorer         *network.Explorer `json:"explorer"`
		DiscoveryEnabled *bool             `json:"discovery_enabled"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	override := config.NetworkOverride{
		Enabled: request.Enabled, Explorer: request.Explorer,
		Discovery: request.DiscoveryEnabled, Endpoints: overrideEndpoints(request.Endpoints),
	}
	if len(override.Endpoints) > 0 {
		networkID := r.PathValue("network_id")
		// Verified with the credentials that will actually be stored, including
		// the ones being carried forward — otherwise an endpoint that only
		// answers when authenticated would be rejected on every save after the
		// first, when the browser no longer has the secret to send back.
		verificationOverride := override
		if previous, ok := p.settings.NetworkOverride(networkID); ok {
			verificationOverride = config.CarryHeadersForward(previous, override)
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
		Endpoints []endpointDTO `json:"endpoints"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	networkID := r.PathValue("network_id")
	override := config.NetworkOverride{Endpoints: overrideEndpoints(request.Endpoints)}
	if previous, ok := p.settings.NetworkOverride(networkID); ok {
		override = config.CarryHeadersForward(previous, override)
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

// overrideEndpoints converts the wire shape into stored endpoints, preserving
// the nil-versus-empty distinction that decides whether a stored credential
// survives the save.
func overrideEndpoints(endpoints []endpointDTO) []config.Endpoint {
	if len(endpoints) == 0 {
		return nil
	}
	out := make([]config.Endpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		out = append(out, config.Endpoint{
			URL: strings.TrimSpace(endpoint.URL), Headers: endpoint.Headers,
		})
	}
	return out
}

func (p *Platform) verifyRPCs(ctx context.Context, item network.Network, override config.NetworkOverride) error {
	if len(override.Endpoints) == 0 {
		return errors.New("at least one RPC URL is required")
	}
	timeout := max(p.settings.Snapshot().Config.NodeDiscovery.RequestTimeout, 5*time.Second)
	for _, configured := range override.Endpoints {
		// Only this endpoint's own credentials are sent to it. Probing every
		// endpoint with a merged header set would be the same leak the storage
		// layout was changed to prevent, one request earlier.
		headers := make(http.Header, len(configured.Headers))
		for name, value := range configured.Headers {
			resolved, err := config.ExpandValue(value)
			if err != nil {
				return err
			}
			headers.Set(name, resolved)
		}
		endpoint, expandErr := config.ExpandValue(configured.URL)
		if expandErr != nil {
			return expandErr
		}
		checkCtx, cancel := context.WithTimeout(ctx, timeout)
		var err error
		if item.Family == network.FamilyEVM {
			err = p.evm.VerifyEndpoint(checkCtx, item, endpoint, headers)
		} else {
			err = p.tron.VerifyEndpoint(checkCtx, item, endpoint, headers)
		}
		cancel()
		if err != nil {
			// Both the endpoint and whatever the node said about it are
			// redacted: the URL was expanded from an ${ENV} reference a moment
			// ago and may carry the provider key in its path or query, and this
			// message goes to the browser and into the log.
			return fmt.Errorf(
				"verify RPC %s: %w", config.RedactURL(endpoint), config.RedactError(err),
			)
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

type assetPriceDTO struct {
	AssetID        string          `json:"asset_id"`
	CurrentUSD     decimal.Decimal `json:"current_usd"`
	Previous24hUSD decimal.Decimal `json:"previous_24h_usd"`
	HasPrevious    bool            `json:"has_previous_24h"`
	Timestamp      time.Time       `json:"timestamp"`
}

func (p *Platform) assetPrices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	requested := make(map[string]struct{})
	for _, assetID := range r.URL.Query()["asset_id"] {
		requested[assetID] = struct{}{}
	}
	targets := make(map[string][]string)
	for _, item := range p.networks.List() {
		item = p.effectiveNetwork(item)
		if !item.Enabled || item.Testnet {
			continue
		}
		for _, itemAsset := range p.assets.List(item) {
			if _, ok := requested[itemAsset.ID]; !ok {
				continue
			}
			identifier := priceIdentifier(item, itemAsset)
			if identifier != "" {
				targets[identifier] = append(targets[identifier], itemAsset.ID)
			}
		}
	}
	identifiers := make([]string, 0, len(targets))
	for identifier := range targets {
		identifiers = append(identifiers, identifier)
	}
	sort.Strings(identifiers)
	if len(identifiers) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"quotes": []assetPriceDTO{}, "stale": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	snapshot, err := p.prices.Quotes(ctx, identifiers)
	if err != nil {
		p.log.Warn("price feed unavailable", "error", err)
		writeError(w, http.StatusBadGateway, "price feed is unavailable")
		return
	}
	quotes := make([]assetPriceDTO, 0, len(snapshot.Quotes))
	for identifier, quote := range snapshot.Quotes {
		for _, assetID := range targets[identifier] {
			quotes = append(quotes, assetPriceDTO{
				AssetID: assetID, CurrentUSD: quote.Current,
				Previous24hUSD: quote.Previous, HasPrevious: quote.HasPrevious,
				Timestamp: quote.Timestamp,
			})
		}
	}
	sort.Slice(quotes, func(i, j int) bool { return quotes[i].AssetID < quotes[j].AssetID })
	writeJSON(w, http.StatusOK, map[string]any{"quotes": quotes, "stale": snapshot.Stale})
}

func priceIdentifier(item network.Network, itemAsset chain.Asset) string {
	if itemAsset.Kind == "native" {
		return item.NativePrice
	}
	// Price Robinhood's canonical WETH through ETH even when contract pricing
	// is unavailable for this network. Match the contract, never the symbol.
	// https://docs.robinhood.com/chain/contracts/
	if item.ID == "robinhood-mainnet" && itemAsset.Kind == "erc20" &&
		strings.EqualFold(itemAsset.Contract, "0x0Bd7D308f8E1639FAb988df18A8011f41EAcAD73") {
		return "coingecko:ethereum"
	}
	if item.PriceChain == "" || itemAsset.Contract == "" {
		return ""
	}
	contract := itemAsset.Contract
	if item.Family == network.FamilyEVM {
		contract = strings.ToLower(contract)
	}
	return item.PriceChain + ":" + contract
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
	// The fee the user was shown and confirmed, echoed back from the estimate.
	// Send signs these rather than whatever the node answers at signing time,
	// so the transaction that is committed to is the one that was on screen.
	// Absent on /estimate, required on /transfers for an EVM network.
	FeeModel             string `json:"fee_model,omitempty"`
	GasLimit             uint64 `json:"gas_limit,omitempty"`
	MaxFeePerGas         string `json:"max_fee_per_gas,omitempty"`
	MaxPriorityFeePerGas string `json:"max_priority_fee_per_gas,omitempty"`
}

func (d transferDTO) approval() *chain.FeeApproval {
	if d.FeeModel == "" && d.GasLimit == 0 && d.MaxFeePerGas == "" {
		return nil
	}

	return &chain.FeeApproval{
		FeeModel: d.FeeModel, GasLimit: d.GasLimit,
		MaxFeePerGas: d.MaxFeePerGas, MaxPriorityFeePerGas: d.MaxPriorityFeePerGas,
	}
}

func (p *Platform) estimateTransfer(w http.ResponseWriter, r *http.Request) {
	request, networkItem, transfer, ok := p.decodePlatformTransfer(w, r)
	_ = request
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	var estimate chain.TransferEstimate
	var err error
	if transfer.Amount == "max" {
		estimate, err = p.chainEstimateMax(ctx, networkItem, transfer)
	} else {
		estimate, err = p.chainEstimate(ctx, networkItem, transfer)
	}
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, estimate)
}

func (p *Platform) sendTransfer(w http.ResponseWriter, r *http.Request) {
	if !p.requireSendConfirmation(w, r) {
		return
	}
	request, networkItem, transfer, ok := p.decodePlatformTransfer(w, r)
	if !ok {
		return
	}
	// The hash covers the whole DTO, so the approved fee is part of what the
	// idempotency key is bound to: a replay that keeps the key but raises the
	// fee is a different request and is refused as a conflict.
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
			if networkItem.Family == network.FamilyTron {
				signer = p.recordingTronSigner(r.PathValue("space_id"), idempotencyKey, signer)
			}
			var sendErr error
			transaction, sendErr = p.chainSend(ctx, networkItem, transfer, signer)
			return sendErr
		})
	if err != nil {
		// Classified on the error, not on whether a hash came back. An adapter
		// that loses the hash on the way out must not turn a lost answer into
		// `failed` — that is the one status which tells the caller it is safe to
		// build and sign a replacement.
		status := operation.StatusFailed
		if errors.Is(err, chain.ErrBroadcastUnknown) {
			status = operation.StatusBroadcastUnknown
		}
		updated, updateErr := p.operations.Update(
			r.PathValue("space_id"), idempotencyKey, transaction.Hash, status,
		)
		if updateErr != nil {
			p.writePlatformError(w, updateErr)
			return
		}
		if status == operation.StatusBroadcastUnknown {
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
	if !p.requireSendConfirmation(w, r) {
		return
	}
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
			signer = p.recordingTronSigner(r.PathValue("space_id"), operationKey, signer)
			var operationErr error
			if unstake {
				txID, operationErr = p.tron.Unstake(ctx, item.ID, address, request.Resource, amount, signer)
			} else {
				txID, operationErr = p.tron.Stake(ctx, item.ID, address, request.Resource, amount, signer)
			}
			return operationErr
		})
	p.finishTronOperation(w, r, item.ID, operationKey, txID, err)
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
	if !p.requireSendConfirmation(w, r) {
		return
	}
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
			signer = p.recordingTronSigner(r.PathValue("space_id"), operationKey, signer)
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
	p.finishTronOperation(w, r, item.ID, operationKey, txID, err)
}

func (p *Platform) tronWithdraw(w http.ResponseWriter, r *http.Request) {
	p.tronWithdrawChange(w, r, false)
}

func (p *Platform) tronCancelUnstakes(w http.ResponseWriter, r *http.Request) {
	p.tronWithdrawChange(w, r, true)
}

func (p *Platform) tronWithdrawChange(w http.ResponseWriter, r *http.Request, cancelUnstakes bool) {
	if !p.requireSendConfirmation(w, r) {
		return
	}
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
			signer = p.recordingTronSigner(r.PathValue("space_id"), operationKey, signer)
			var operationErr error
			txID, operationErr = p.tron.Withdraw(ctx, item.ID, address, cancelUnstakes, signer)
			return operationErr
		})
	p.finishTronOperation(w, r, item.ID, operationKey, txID, err)
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
	if !p.requireSendConfirmation(w, r) {
		return
	}
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
			signer = p.recordingTronSigner(r.PathValue("space_id"), operationKey, signer)
			var deployErr error
			deployed, deployErr = p.tron.Deploy(ctx, item.ID, address, deployment, signer)
			return deployErr
		})
	if err != nil {
		p.finishTronOperation(w, r, item.ID, operationKey, deployed.TxID, err)
		return
	}
	// A deployment waits for its receipt, so unlike the other operations it can
	// already know the outcome. Confirmed and failed here are both about what
	// the chain did with a transaction it definitely received.
	status := operation.StatusPending
	if deployed.Confirmed {
		status = operation.StatusConfirmed
	}
	if deployed.Failure != "" {
		status = operation.StatusFailed
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

// recordingTronSigner writes down which transaction is about to be sent, before
// it is signed.
//
// A Tron transaction id is sha256 of the raw data, and that is exactly the
// digest handed to the signer — so at this point the identity of what is about
// to leave the process is already known, and can be made durable first. If the
// wallet dies between here and the node's answer, the record still names the
// transaction: the retry can be told to go and look for it instead of being
// invited to build a second one.
//
// This only works because the Tron digest is the transaction id. It is not true
// of EVM, where the signing hash is not the transaction hash, so the equivalent
// there is the hash computed just before the send.
func (p *Platform) recordingTronSigner(spaceID, key string, signer chain.Signer) chain.Signer {
	return recordingSigner{Signer: signer, record: func(digest []byte) error {
		if _, err := p.operations.Update(
			spaceID, key, hex.EncodeToString(digest), operation.StatusBroadcasting,
		); err != nil {
			return fmt.Errorf("record the transaction before signing it: %w", err)
		}
		return nil
	}}
}

type recordingSigner struct {
	chain.Signer
	record func(digest []byte) error
}

func (s recordingSigner) SignDigest(ctx context.Context, digest []byte) ([]byte, error) {
	if err := s.record(digest); err != nil {
		return nil, err
	}
	return s.Signer.SignDigest(ctx, digest)
}

// finishTronOperation records the outcome of a signed Tron operation and writes
// the response.
//
// The status is the whole point. Only a provable non-broadcast is `failed`,
// because that is the one status that tells the caller it is safe to build and
// sign again. A lost answer keeps the transaction id and reports
// `broadcast_unknown` with 202: the transaction may be on chain, and the caller
// has to check it rather than replace it.
func (p *Platform) finishTronOperation(
	w http.ResponseWriter, r *http.Request, networkID, key, txID string, err error,
) {
	spaceID := r.PathValue("space_id")
	if err != nil {
		// Classified on the error alone. The transaction id comes back from the
		// stored record, which the recording signer wrote before the signature —
		// so an operation whose id never made it back through the return value
		// is still reported with the id it was given.
		status := operation.StatusFailed
		if errors.Is(err, chain.ErrBroadcastUnknown) {
			status = operation.StatusBroadcastUnknown
		}
		updated, updateErr := p.operations.Update(spaceID, key, txID, status)
		if updateErr != nil {
			p.log.Error("persist Tron operation outcome", "error", updateErr)
		}
		if status == operation.StatusBroadcastUnknown {
			writeJSON(w, http.StatusAccepted, map[string]string{
				"network_id": networkID, "tx_id": updated.TxHash, "status": status,
				"warning": config.RedactError(err).Error(),
			})
			return
		}
		p.writePlatformError(w, err)
		return
	}
	if _, err := p.operations.Update(spaceID, key, txID, operation.StatusPending); err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"network_id": networkID, "tx_id": txID, "status": operation.StatusPending,
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

// rejectIncompleteReplay decides what a repeat of an operation is allowed to
// do, and the answer turns on whether a transaction may exist on chain.
//
// Advising a new idempotency key means "build and sign a second transaction",
// which is only safe when the first provably never reached a node. Once a
// transaction has been signed and sent, that advice is how one transfer becomes
// two — so an in-flight operation is answered with what is known about it
// instead, and the caller is told to look for that transaction rather than
// replace it.
func rejectIncompleteReplay(w http.ResponseWriter, existing operation.Operation) bool {
	switch {
	case operation.InFlight(existing.Status):
		// A transaction may exist. Answer with what is known about it rather
		// than inviting a second one.
		return false
	case existing.Status == operation.StatusBuilding:
		writeError(w, http.StatusConflict, "operation is still in progress")
	case existing.Status == operation.StatusFailed:
		writeError(w, http.StatusConflict,
			"previous operation failed before broadcast; retry with a new idempotency key")
	default:
		// An unrecognised status is treated as in-flight. Being wrong in this
		// direction costs a confusing answer; being wrong in the other costs a
		// second transfer.
		return false
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
		AccountID: request.AccountID, From: from, To: request.To, Asset: asset,
		Amount: request.Amount, Approved: request.approval(),
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

func (p *Platform) chainEstimateMax(ctx context.Context, item network.Network, request chain.TransferRequest) (chain.TransferEstimate, error) {
	if item.Family == network.FamilyEVM {
		return p.evm.EstimateMaxTransfer(ctx, item.ID, request)
	}
	return p.tron.EstimateMaxTransfer(ctx, item.ID, request)
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
	case errors.Is(err, space.ErrPasswordRequired), errors.Is(err, vault.ErrInvalidPassword):
		// A step-up that was not attempted, or attempted with the wrong
		// password. Both answer the same way so neither confirms the other.
		status = http.StatusUnauthorized
	case errors.Is(err, space.ErrSendConfirmationRequired):
		// Forbidden rather than unauthorized: nothing is wrong with what the
		// caller sent or with the token it holds, and telling a tab it has lost
		// its token would send the user to reopen a wallet that is working. The
		// space is being asked for its password before it will spend.
		status = http.StatusForbidden
	case errors.Is(err, space.ErrDuplicateKey), errors.Is(err, space.ErrFirstSpaceExists),
		errors.Is(err, operation.ErrConflict), errors.Is(err, chain.ErrFeeChanged):
		// A stale fee approval is a conflict, not a bad request: nothing the
		// caller sent was wrong, the network simply moved and the user has to
		// see the new numbers before anything is signed.
		status = http.StatusConflict
	case errors.Is(err, space.ErrTooManyAttempts):
		status = http.StatusTooManyRequests
	case errors.Is(err, space.ErrQuotaExceeded), errors.Is(err, asset.ErrQuotaExceeded):
		status = http.StatusInsufficientStorage
	case errors.Is(err, config.ErrRevisionConflict):
		status = http.StatusPreconditionFailed
	case errors.Is(err, config.ErrInvalidSettings), errors.Is(err, asset.ErrInvalidMetadata):
		status = http.StatusBadRequest
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		// The caller hung up — a dismissed dialog, a closed tab, a navigation
		// away from a page mid-stream. Nothing failed and there is most likely
		// nobody left to read the answer, so it must not go through the default
		// branch, which would call every abandoned request a bad gateway and
		// write a warning about it in the log.
		status = statusClientClosedRequest
	default:
		if !errors.Is(err, account.ErrInvalidMnemonic) &&
			!errors.Is(err, account.ErrInvalidPrivateKey) &&
			!errors.Is(err, chain.ErrInvalidRequest) &&
			!errors.Is(err, space.ErrWeakPassword) &&
			!strings.Contains(err.Error(), "required") &&
			!strings.Contains(err.Error(), "invalid") {
			status = http.StatusBadGateway
			p.log.Warn("platform request failed", "error", config.RedactError(err))
		}
	}
	// Classification runs on the original error so the sentinels above still
	// match; only the text that leaves the process is redacted. An error raised
	// against an RPC endpoint quotes it, and by that point the ${ENV} reference
	// in the configuration has been expanded into the real credential.
	message := config.RedactError(err).Error()
	if code := platformErrorCode(err); code != "" {
		writeJSON(w, status, map[string]string{"error": message, "code": code})
		return
	}
	writeError(w, status, message)
}

// platformErrorCode labels the refusals the browser has to act on rather than
// merely display.
//
// Exactly one error has a code, and that is the point: a code is a promise that
// the UI has a remedy for this particular refusal — here, ask the person at the
// keyboard for the space password and repeat the identical request, idempotency
// key and approved fee included. Handing one to every error would turn a
// contract into decoration, and leaving it at the call site is what let the
// contract depend on where the check happened to be raised from.
func platformErrorCode(err error) string {
	if errors.Is(err, space.ErrSendConfirmationRequired) {
		return codeSendConfirmationRequired
	}
	return ""
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
