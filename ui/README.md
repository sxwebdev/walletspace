# Walletspace UI

The wallet's interface: React, Vite, Tailwind and shadcn/ui (Base UI
primitives). It is built into `../internal/httpapi/ui/dist` and embedded into
the Go binary with `go:embed`; without a build the binary serves a placeholder
page that says so.

## Develop

Run the wallet on a fixed port, against a throwaway home if you do not want to
touch your real vaults:

```bash
WALLETSPACE_HOME=/tmp/ws-dev WALLETSPACE_ADDR=127.0.0.1:8080 WALLETSPACE_OPEN_BROWSER=false make start
```

Then start Vite, which proxies `/api` to it (`WALLETSPACE_DEV_API` changes the
target):

```bash
pnpm install
pnpm dev
```

Open `http://localhost:5173/#token=<token>` with the token the wallet printed.

## Build and check

```bash
make ui        # from the repository root: install and build into the embed dir
make ui-lint   # type-check and lint
```

## Constraints

The server sends a strict Content-Security-Policy (`internal/httpapi/server.go`):

- no inline scripts or `<style>` elements — `main.tsx` wraps the app in Base
  UI's `CSPProvider disableStyleElements`, and `index.css` carries the rules it
  would have injected;
- fonts and images come from the binary only — Vite inlines nothing
  (`assetsInlineLimit: 0`), and token icons are drawn, not fetched;
- on-chain text (token names, symbols) is attacker-controlled: never use
  `dangerouslySetInnerHTML`.

Add components with `pnpm dlx shadcn@latest add <name>`.
