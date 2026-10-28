package main

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/chain/chaintest"
	"github.com/sxwebdev/walletspace/internal/network"
)

func TestParseCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		want     command
		wantArgs []string
	}{
		{name: "no arguments serves", args: nil, want: commandServe},
		{
			name: "migrate keeps its own flags", args: []string{"migrate", "--from", "./data"},
			want: commandMigrate, wantArgs: []string{"--from", "./data"},
		},
		{
			name: "export keeps its own flags", args: []string{"export", "--out", "x.wsbackup"},
			want: commandExport, wantArgs: []string{"--out", "x.wsbackup"},
		},
		{
			name: "import keeps its file", args: []string{"import", "x.wsbackup"},
			want: commandImport, wantArgs: []string{"x.wsbackup"},
		},
		{name: "version word", args: []string{"version"}, want: commandVersion},
		{name: "version flag", args: []string{"--version"}, want: commandVersion},
		{name: "single dash version flag", args: []string{"-version"}, want: commandVersion},
		{name: "help word", args: []string{"help"}, want: commandHelp},
		{name: "help flag", args: []string{"--help"}, want: commandHelp},
		{name: "short help flag", args: []string{"-h"}, want: commandHelp},
		// A typo used to start the server, so it has to be reported instead. The
		// argument is handed back for the error message.
		{
			name: "typo is not the server", args: []string{"--versionn"},
			want: commandUnknown, wantArgs: []string{"--versionn"},
		},
		{
			name: "stray subcommand is not the server", args: []string{"serve"},
			want: commandUnknown, wantArgs: []string{"serve"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, gotArgs := parseCommand(tt.args)
			if got != tt.want {
				t.Errorf("parseCommand(%q) command = %v, want %v", tt.args, got, tt.want)
			}
			// slices.Equal treats nil and empty as equal, which is what the
			// wantArgs-less cases rely on.
			if !slices.Equal(gotArgs, tt.wantArgs) {
				t.Errorf("parseCommand(%q) args = %q, want %q", tt.args, gotArgs, tt.wantArgs)
			}
		})
	}
}

func TestUsageReportsVersion(t *testing.T) {
	t.Parallel()

	// The exact prefix, not a substring: `strings.Contains(out, version)` also
	// passes when the version placeholder is dropped from the format string and
	// "dev" happens to appear anywhere else in the text.
	var out strings.Builder
	usage(&out)
	want := "Walletspace " + version + " —"
	if !strings.HasPrefix(out.String(), want) {
		t.Errorf("usage() = %q..., want it to start with %q", firstLine(out.String()), want)
	}
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(value, "\n")
	return line
}

// A FlagSet in ContinueOnError mode reports -h as flag.ErrHelp, which used to
// travel up to main and turn a help request into "fatal" plus exit status 1.
func TestMigrateHelpIsNotAnError(t *testing.T) {
	// Not parallel: the flag package writes the usage to os.Stderr, and this
	// swaps it out so `go test` output stays readable.
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	original := os.Stderr
	os.Stderr = write
	t.Cleanup(func() {
		os.Stderr = original
		write.Close()
		read.Close()
	})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, arg := range []string{"-h", "-help", "--help"} {
		if err := runMigration(log, []string{arg}); err != nil {
			t.Errorf("runMigration(%q) error = %v, want nil", arg, err)
		}
	}
}

func TestUIURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		addr string
		want string
	}{
		{name: "loopback", addr: "127.0.0.1:8080", want: "http://127.0.0.1:8080/#token=t0ken"},
		{name: "hostname", addr: "localhost:9000", want: "http://localhost:9000/#token=t0ken"},
		// Wildcards are not routable, so the browser gets the loopback instead.
		{name: "ipv4 wildcard", addr: "0.0.0.0:8080", want: "http://127.0.0.1:8080/#token=t0ken"},
		{name: "empty host", addr: ":8080", want: "http://127.0.0.1:8080/#token=t0ken"},
		// Splitting on the first colon mangled this into "http://[:]:]:8080",
		// and the wildcard substitution never ran.
		{name: "ipv6 wildcard", addr: "[::]:8080", want: "http://127.0.0.1:8080/#token=t0ken"},
		{name: "ipv6 loopback", addr: "[::1]:8080", want: "http://[::1]:8080/#token=t0ken"},
		{name: "unparsable falls back verbatim", addr: "not-an-address", want: "http://not-an-address"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := uiURL(tt.addr, "t0ken"); got != tt.want {
				t.Errorf("uiURL(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

// The capability token belongs in the fragment and nowhere else: a query
// parameter would be sent to the server and could reach an access log, and the
// path would do the same.
func TestUIURLKeepsTheTokenInTheFragment(t *testing.T) {
	t.Parallel()

	got := uiURL("127.0.0.1:54321", "abc-123_XYZ")
	base, fragment, found := strings.Cut(got, "#")
	if !found {
		t.Fatalf("uiURL() = %q, want a fragment", got)
	}
	if strings.Contains(base, "abc-123_XYZ") {
		t.Errorf("uiURL() = %q leaks the token outside the fragment", got)
	}
	if fragment != "token=abc-123_XYZ" {
		t.Errorf("fragment = %q, want token=abc-123_XYZ", fragment)
	}
}

// `open <url>` puts the capability token in the helper process's argv, and on
// Linux /proc/<pid>/cmdline is world-readable — publishing the one secret that
// separates this UI from any other local program to exactly the adversary it
// exists to exclude. The URL goes into a file only this user can read instead.
func TestBrowserEntryPointKeepsTheTokenOutOfArgv(t *testing.T) {
	t.Parallel()

	const entry = "http://127.0.0.1:53124/#token=super-secret-token"
	target, cleanup, err := browserEntryPoint(entry)
	if err != nil {
		t.Fatalf("browserEntryPoint() error = %v", err)
	}
	t.Cleanup(cleanup)

	if strings.Contains(target, "super-secret-token") {
		t.Fatalf("browser target = %q, still carries the token", target)
	}
	path, ok := strings.CutPrefix(target, "file://")
	if !ok {
		t.Fatalf("browser target = %q, want a file URL", target)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", path, err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("entry point mode = %04o, want 0600", mode)
	}
	page, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if !strings.Contains(string(page), entry) {
		t.Errorf("entry point does not redirect to the UI:\n%s", page)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup left the entry point behind: %v", err)
	}
}

type headersStub struct {
	asked int
	err   error
}

// Headers answers with a credential that names the endpoint, so a test can
// tell the probe got this endpoint's credential and not some other.
func (h *headersStub) Headers(_ network.Network, endpoint string) (http.Header, error) {
	h.asked++
	return endpointCredential(endpoint), h.err
}

func endpointCredential(endpoint string) http.Header {
	return http.Header{"Authorization": {"Bearer key-for " + endpoint}}
}

// The Node Doctor probes each endpoint with its own family's adapter. A family
// without one fails the probe — it is not probed as Tron, which used to be the
// fallback for anything that was not EVM — and its credentials are not even
// resolved.
func TestDoctorVerifierProbesWithTheOwnFamily(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		family    network.Family
		wantErr   error
		wantTron  int
		wantEVM   int
		wantAsked int
	}{
		{name: "tron", family: network.FamilyTron, wantTron: 1, wantAsked: 1},
		{name: "evm", family: network.FamilyEVM, wantEVM: 1, wantAsked: 1},
		{name: "unknown family", family: "bogus", wantErr: chain.ErrUnsupportedFamily},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tron, evm := &chaintest.Adapter{}, &chaintest.Adapter{}
			chains, err := chain.NewAdapters(map[network.Family]chain.Adapter{
				network.FamilyTron: tron, network.FamilyEVM: evm,
			})
			if err != nil {
				t.Fatalf("chain.NewAdapters() error = %v", err)
			}
			credentials := &headersStub{}
			verify := doctorVerifier(chains, credentials)
			item := network.Network{ID: "net", Family: tt.family}
			if err := verify(t.Context(), item, "https://rpc.example"); !errors.Is(err, tt.wantErr) {
				t.Fatalf("verify() error = %v, want %v", err, tt.wantErr)
			}
			if got := len(tron.Calls()); got != tt.wantTron {
				t.Errorf("tron adapter calls = %v, want %d probe(s)", tron.Calls(), tt.wantTron)
			}
			if got := len(evm.Calls()); got != tt.wantEVM {
				t.Errorf("evm adapter calls = %v, want %d probe(s)", evm.Calls(), tt.wantEVM)
			}
			for _, call := range append(tron.Calls(), evm.Calls()...) {
				if call.Method != "ProbeEndpoint" {
					t.Errorf("adapter call %v, want ProbeEndpoint", call)
				}
			}
			if credentials.asked != tt.wantAsked {
				t.Errorf("credentials resolved %d times, want %d", credentials.asked, tt.wantAsked)
			}
			// The credential resolved for the endpoint reaches the probe
			// unchanged; without it every keyed node would fail as unauthorised.
			for _, probed := range append(tron.Headers(), evm.Headers()...) {
				if want := endpointCredential("https://rpc.example"); !reflect.DeepEqual(probed, want) {
					t.Errorf("probe headers = %v, want %v", probed, want)
				}
			}
			if got := len(tron.Headers()) + len(evm.Headers()); got != tt.wantTron+tt.wantEVM {
				t.Errorf("probes that received headers = %d, want %d", got, tt.wantTron+tt.wantEVM)
			}
		})
	}
}

// A credential that cannot be resolved fails the probe before the node is
// contacted without it.
func TestDoctorVerifierStopsOnACredentialError(t *testing.T) {
	t.Parallel()

	tron := &chaintest.Adapter{}
	chains, err := chain.NewAdapters(map[network.Family]chain.Adapter{network.FamilyTron: tron})
	if err != nil {
		t.Fatalf("chain.NewAdapters() error = %v", err)
	}
	unresolved := errors.New("WALLETSPACE_KEY is not set")
	verify := doctorVerifier(chains, &headersStub{err: unresolved})
	err = verify(t.Context(), network.Network{ID: "tron-nile", Family: network.FamilyTron}, "https://rpc.example")
	if !errors.Is(err, unresolved) {
		t.Fatalf("verify() error = %v, want %v", err, unresolved)
	}
	if calls := tron.Calls(); len(calls) != 0 {
		t.Errorf("the node was probed without its credentials: %v", calls)
	}
}
