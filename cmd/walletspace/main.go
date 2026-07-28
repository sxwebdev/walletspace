// Command walletspace serves a local UI for wallets derived from a single
// BIP39 mnemonic, showing their TRX and USDT balances.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/tron"
	"github.com/sxwebdev/walletspace/internal/wallet"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	mnemonic, generated, err := wallet.ResolveMnemonic(cfg.Mnemonic, cfg.DataDir)
	if err != nil {
		return err
	}

	if generated {
		log.Warn("a new mnemonic was generated — back it up now, it is the only way to recover funds",
			"file", filepath.Join(cfg.DataDir, wallet.MnemonicFileName))
	}

	store, err := wallet.New(filepath.Join(cfg.DataDir, "wallets.json"), mnemonic, cfg.Passphrase)
	if err != nil {
		return err
	}

	chain, err := tron.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer chain.Close()

	handler, err := httpapi.New(store, chain, cfg.Network, cfg.ExplorerURL(), log)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Bind before anything else so a busy port fails here, synchronously,
	// instead of racing a browser window that would open on a dead URL.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf(
				"%s is already in use — another walletspace is probably still running; "+
					"stop it (lsof -nP -iTCP:%s -sTCP:LISTEN) or set ADDR to a free port",
				cfg.Addr, portOf(cfg.Addr),
			)
		}

		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "network", cfg.Network, "wallets", len(store.List()))
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	url := uiURL(cfg.Addr)
	if cfg.OpenBrowser {
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

	return srv.Shutdown(shutdownCtx)
}

// portOf extracts the port from a listen address, for use in hints.
func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}

	return port
}

// uiURL turns a listen address into a URL a browser can open.
func uiURL(addr string) string {
	// SplitHostPort, not Cut: an IPv6 listen address like "[::]:8080" contains
	// colons of its own, and splitting on the first one mangles it.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}

	// Wildcards are not routable; point the browser at the loopback instead.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	return "http://" + net.JoinHostPort(host, port)
}

func openBrowser(url string, log *slog.Logger) {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}

	if err := cmd.Start(); err != nil {
		log.Warn("could not open a browser, open the UI manually", "url", url, "error", err)
		return
	}

	log.Info("UI opened", "url", url)
}
