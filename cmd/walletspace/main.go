// Command walletspace serves the local multichain Walletspace UI.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sxwebdev/walletspace/internal/asset"
	evmchain "github.com/sxwebdev/walletspace/internal/chain/evm"
	tronchain "github.com/sxwebdev/walletspace/internal/chain/tron"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/doctor"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"github.com/sxwebdev/walletspace/internal/rpcpool"
	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/storage"
	"github.com/sxwebdev/walletspace/internal/vault"
	"golang.org/x/term"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	var err error
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		err = runMigration(log, os.Args[2:])
	} else {
		err = run(log)
	}
	if err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	home, err := storage.ResolveHome("")
	if err != nil {
		return err
	}
	processLock, err := storage.AcquireLock(home)
	if err != nil {
		return err
	}
	defer processLock.Close()

	settings, err := config.NewHomeManager(home)
	if err != nil {
		return err
	}
	snapshot := settings.Snapshot()
	if value := strings.TrimSpace(os.Getenv("WALLETSPACE_ADDR")); value != "" {
		snapshot.Config.Server.Addr = value
	}
	if value := strings.TrimSpace(os.Getenv("WALLETSPACE_OPEN_BROWSER")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("parse WALLETSPACE_OPEN_BROWSER: %w", err)
		}
		snapshot.Config.Server.OpenBrowser = parsed
	}
	if err := config.ValidateHomeConfig(snapshot.Config); err != nil {
		return err
	}
	spaces, err := space.NewManager(home, snapshot.Config.Security.AutoLock, vault.DefaultParams)
	if err != nil {
		return err
	}
	defer spaces.Close()
	registry, err := network.Builtin()
	if err != nil {
		return err
	}
	resolver := rpcpool.New(settings)
	evm, err := evmchain.New(registry, resolver)
	if err != nil {
		return err
	}
	defer evm.Close()
	tron, err := tronchain.New(ctx, registry, settings, resolver, log)
	if err != nil {
		return err
	}
	defer tron.Close()
	assets, err := asset.New(home)
	if err != nil {
		return err
	}
	nodeDoctor, err := doctor.New(
		ctx, registry, resolver,
		func(checkCtx context.Context, item network.Network, endpoint string) error {
			headers, headerErr := resolver.Headers(item)
			if headerErr != nil {
				return headerErr
			}
			if item.Family == network.FamilyEVM {
				return evmchain.VerifyEndpoint(
					checkCtx, item, endpoint, headers, resolver.HTTPClient(item),
				)
			}
			return tronchain.ProbeEndpoint(
				checkCtx, item, endpoint, headers.Get("TRON-PRO-API-KEY"),
				resolver.HTTPClient(item),
			)
		},
		doctor.Options{Networks: func() []network.Network {
			items := registry.List()
			for i := range items {
				if override, ok := settings.NetworkOverride(items[i].ID); ok &&
					override.Enabled != nil {
					items[i].Enabled = *override.Enabled
				}
			}
			return items
		}},
	)
	if err != nil {
		return err
	}
	defer nodeDoctor.Close()
	handler, err := httpapi.NewPlatform(
		spaces, settings, registry, operation.New(home), assets, evm, tron, nodeDoctor, log,
	)
	if err != nil {
		return err
	}

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	addr := snapshot.Config.Server.Addr
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf(
				"%s is already in use — another walletspace is probably still running; "+
					"stop it (lsof -nP -iTCP:%s -sTCP:LISTEN) or change the address in settings",
				addr, portOf(addr),
			)
		}
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "home", home, "spaces", len(spaces.List()))
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	url := uiURL(addr)
	if snapshot.Config.Server.OpenBrowser {
		openBrowser(url, log)
	} else {
		log.Info("UI available", "url", url)
	}

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

type legacyFile struct {
	Wallets []struct {
		Index   uint32 `json:"index"`
		Address string `json:"address"`
		Label   string `json:"label"`
	} `json:"wallets"`
}

func runMigration(log *slog.Logger, args []string) error {
	flags := flag.NewFlagSet("walletspace migrate", flag.ContinueOnError)
	from := flags.String("from", "", "legacy data directory")
	homeFlag := flags.String("home", "", "Walletspace home")
	nameFlag := flags.String("name", "", "new space name")
	dryRun := flags.Bool("dry-run", false, "verify every legacy address without writing")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*from) == "" {
		return errors.New("migrate requires --from")
	}
	mnemonicBytes, err := os.ReadFile(filepath.Join(*from, "mnemonic.txt"))
	if err != nil {
		return fmt.Errorf("read legacy mnemonic: %w", err)
	}
	mnemonic := strings.TrimSpace(string(mnemonicBytes))
	clear(mnemonicBytes)
	walletBytes, err := os.ReadFile(filepath.Join(*from, "wallets.json"))
	if err != nil {
		return fmt.Errorf("read legacy wallets: %w", err)
	}
	var legacy legacyFile
	if err := json.Unmarshal(walletBytes, &legacy); err != nil {
		return fmt.Errorf("decode legacy wallets: %w", err)
	}
	clear(walletBytes)

	accounts := make([]space.LegacyAccount, len(legacy.Wallets))
	for i, item := range legacy.Wallets {
		accounts[i] = space.LegacyAccount{
			Index: item.Index, Label: item.Label, TronAddress: item.Address,
		}
	}
	fmt.Fprint(os.Stdout, "Legacy BIP39 passphrase (empty if none): ")
	passphraseBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stdout)
	if err != nil {
		return fmt.Errorf("read BIP39 passphrase: %w", err)
	}
	defer clear(passphraseBytes)
	if err := space.ValidateLegacy(mnemonic, string(passphraseBytes), accounts); err != nil {
		return fmt.Errorf("legacy verification failed: %w", err)
	}
	if *dryRun {
		fmt.Fprintf(os.Stdout,
			"Dry run complete: %d legacy addresses match the mnemonic. No files were written.\n",
			len(accounts),
		)
		return nil
	}

	home, err := storage.ResolveHome(*homeFlag)
	if err != nil {
		return err
	}
	processLock, err := storage.AcquireLock(home)
	if err != nil {
		return err
	}
	defer processLock.Close()

	name := strings.TrimSpace(*nameFlag)
	reader := bufio.NewReader(os.Stdin)
	if name == "" {
		fmt.Fprint(os.Stdout, "New space name [default]: ")
		value, readErr := reader.ReadString('\n')
		if readErr != nil {
			return fmt.Errorf("read space name: %w", readErr)
		}
		name = strings.TrimSpace(value)
	}
	fmt.Fprint(os.Stdout, "New vault password: ")
	passwordBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stdout)
	if err != nil {
		return fmt.Errorf("read vault password: %w", err)
	}
	defer clear(passwordBytes)

	manager, err := space.NewManager(home, 15*time.Minute, vault.DefaultParams)
	if err != nil {
		return err
	}
	defer manager.Close()
	result, err := manager.ImportLegacy(space.CreateRequest{
		Name: name, Mnemonic: mnemonic, BIP39Passphrase: string(passphraseBytes),
		Password: string(passwordBytes),
	}, accounts)
	if err != nil {
		return err
	}
	log.Info("legacy data migrated",
		"space_id", result.Space.ID,
		"space_file", filepath.Join(home, "spaces", result.Space.ID, "space.json"),
		"legacy_unchanged", *from,
	)
	fmt.Fprintf(os.Stdout,
		"Migration complete. Open Walletspace and verify every address.\n"+
			"Assign the original Tron network to the migrated wallets in the UI before using them.\n"+
			"Legacy files were not changed: %s\n"+
			"Archive or delete them manually only after verification.\n", *from)
	return nil
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return port
}

func uiURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func openBrowser(url string, log *slog.Logger) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	if err := command.Start(); err != nil {
		log.Warn("could not open a browser, open the UI manually", "url", url, "error", err)
		return
	}
	log.Info("UI opened", "url", url)
}
