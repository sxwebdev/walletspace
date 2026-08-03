// Command walletspace serves the local multichain Walletspace UI.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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
	"github.com/sxwebdev/walletspace/internal/price"
	"github.com/sxwebdev/walletspace/internal/rpcpool"
	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/storage"
	"github.com/sxwebdev/walletspace/internal/vault"
	"golang.org/x/term"
)

// version is stamped by the release build; see .goreleaser.yml and the Makefile.
var version = "dev"

type command int

const (
	commandServe command = iota
	commandMigrate
	commandVersion
	commandHelp
	commandUnknown
)

// parseCommand splits the process arguments into a command and the arguments that
// belong to it. The server itself takes no flags — its settings come from
// ~/.walletspace and from the environment — so only subcommands appear here.
func parseCommand(args []string) (command, []string) {
	if len(args) == 0 {
		return commandServe, nil
	}
	switch args[0] {
	case "migrate":
		return commandMigrate, args[1:]
	case "version", "--version", "-version":
		return commandVersion, args[1:]
	case "help", "--help", "-help", "-h":
		return commandHelp, args[1:]
	}
	return commandUnknown, args
}

func usage(out io.Writer) {
	fmt.Fprintf(out, `Walletspace %s — local multichain wallet manager.

Usage:
  walletspace                     serve the UI on the configured loopback address
  walletspace migrate --from DIR  import a legacy data directory into a new space
  walletspace version             print the version
  walletspace help                print this help

Migrate flags:
  --from DIR   legacy data directory (required)
  --home DIR   Walletspace home to create the new space in
  --name NAME  name for the new space
  --dry-run    verify every legacy address without writing anything

Environment:
  WALLETSPACE_HOME          data directory, default ~/.walletspace
  WALLETSPACE_ADDR          loopback listen address
  WALLETSPACE_OPEN_BROWSER  open a browser on start, true or false

Everything persistent is edited on the /settings page or in
~/.walletspace/config.yaml.
`, version)
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cmd, args := parseCommand(os.Args[1:])
	var err error
	switch cmd {
	case commandServe:
		err = run(log)
	case commandMigrate:
		err = runMigration(log, args)
	case commandVersion:
		fmt.Fprintln(os.Stdout, version)
	case commandHelp:
		usage(os.Stdout)
	case commandUnknown:
		// A mistyped argument used to fall through to the server, which then
		// started as if nothing had been asked for.
		fmt.Fprintf(os.Stderr, "unknown argument %q\n\n", args[0])
		usage(os.Stderr)
		os.Exit(2)
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
			// Per endpoint, not per network: the Doctor probes the official
			// fallbacks and whatever discovery suggested alongside the user's own
			// nodes, and a provider credential belongs to exactly one of them.
			headers, headerErr := resolver.Headers(item, endpoint)
			if headerErr != nil {
				return headerErr
			}
			if item.Family == network.FamilyEVM {
				return evmchain.VerifyEndpoint(
					checkCtx, item, endpoint, headers, resolver.HTTPClient(item),
				)
			}
			return tronchain.ProbeEndpoint(
				checkCtx, item, endpoint, headers, resolver.HTTPClient(item),
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

	// The listener comes first: the guard checks the Host header against the
	// address actually opened, and with the default port of 0 that address is
	// only known once the kernel has chosen one.
	configured := snapshot.Config.Server.Addr
	listener, err := net.Listen("tcp", configured)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf(
				"%s is already in use — another walletspace is probably still running; "+
					"stop it (lsof -nP -iTCP:%s -sTCP:LISTEN) or change the address in settings",
				configured, portOf(configured),
			)
		}
		return fmt.Errorf("listen on %s: %w", configured, err)
	}
	defer listener.Close()
	addr := listener.Addr().String()

	token, err := httpapi.NewToken()
	if err != nil {
		return err
	}
	access, err := httpapi.LoopbackAccess(token, listener.Addr())
	if err != nil {
		return err
	}

	handler, err := httpapi.NewPlatform(
		spaces, settings, registry, operation.New(home), assets, evm, tron, nodeDoctor,
		price.New(price.Options{}), access, log,
	)
	if err != nil {
		return err
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Comfortably above the longest legitimate response: the balance stream
		// runs for up to five minutes on a large portfolio behind rate-limited
		// public nodes, and a deployment waits ninety seconds for its receipt.
		WriteTimeout:   6 * time.Minute,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "home", home, "spaces", len(spaces.List()))
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// The token rides in the fragment, which browsers keep to themselves: it is
	// never sent to the server, never lands in a Referer header and never shows
	// up in a proxy or access log the way a query parameter would.
	entryURL := uiURL(addr, token)
	if snapshot.Config.Server.OpenBrowser {
		openBrowser(entryURL, log)
	}
	// Printed either way. With a random port and a per-run token this line is
	// the only way back into the UI if the browser did not open or was closed.
	fmt.Fprintf(os.Stdout, "Walletspace is ready:\n  %s\n", entryURL)

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
		// -h asked for the usage and got it; the FlagSet has already printed it.
		// Without this the one subcommand that takes flags is the one whose help
		// exits non-zero with "fatal error=flag: help requested".
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
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
		fmt.Fprintf(
			os.Stdout,
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
	log.Info(
		"legacy data migrated",
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

func uiURL(addr, token string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/#token=" + url.QueryEscape(token)
}

// openBrowser hands the entry URL to the desktop browser without putting the
// capability token where another process can read it.
//
// The obvious `open <url>` puts the token in the helper's argv, and on Linux
// /proc/<pid>/cmdline is world-readable — so the one secret separating this UI
// from any other local program would be published to exactly the adversary it
// exists to exclude. Instead the URL goes into a 0600 file that only this user
// can open, and the browser is pointed at the file. That keeps the token inside
// the wallet's own files, which the threat model already assumes the adversary
// cannot reach.
//
// The token never reaches the log either, for the same reason a fragment is
// used in the first place: a log outlives the run.
func openBrowser(entryURL string, log *slog.Logger) {
	target, cleanup, err := browserEntryPoint(entryURL)
	if err != nil {
		log.Warn("could not prepare the browser entry point", "error", err)
		return
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	if err := command.Start(); err != nil {
		cleanup()
		log.Warn("could not open a browser, use the printed URL", "error", err)
		return
	}
	// Long enough for a cold browser start, short enough that the file is not
	// left lying around. The printed URL is the fallback either way.
	time.AfterFunc(2*time.Minute, cleanup)
	log.Info("UI opened")
}

// browserEntryPoint writes a redirect page readable only by this user and
// returns a file:// URL for it.
func browserEntryPoint(entryURL string) (string, func(), error) {
	file, err := os.CreateTemp("", "walletspace-*.html")
	if err != nil {
		return "", func() {}, fmt.Errorf("create browser entry point: %w", err)
	}
	name := file.Name()
	cleanup := func() { _ = os.Remove(name) }
	// CreateTemp already makes the file 0600; this is belt and braces for a
	// umask or filesystem that says otherwise.
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("restrict browser entry point: %w", err)
	}
	escaped := html.EscapeString(entryURL)
	page := "<!doctype html><meta charset=\"utf-8\">" +
		"<meta http-equiv=\"refresh\" content=\"0; url=" + escaped + "\">" +
		"<title>Walletspace</title>" +
		"<p>Opening Walletspace… <a href=\"" + escaped + "\">continue</a></p>"
	if _, err := file.WriteString(page); err != nil {
		file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("write browser entry point: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close browser entry point: %w", err)
	}

	return "file://" + name, cleanup, nil
}
