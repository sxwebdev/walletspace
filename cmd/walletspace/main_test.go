package main

import (
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
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
