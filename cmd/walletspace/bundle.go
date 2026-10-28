package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/storage"
	"github.com/sxwebdev/walletspace/internal/vault"
)

// spaceIDs collects a repeated --space flag.
type spaceIDs []string

func (s *spaceIDs) String() string { return strings.Join(*s, ",") }

func (s *spaceIDs) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("empty space id")
	}
	*s = append(*s, value)
	return nil
}

// parseFlags is flag.Parse with -h treated as a request that was answered
// rather than as a failure.
func parseFlags(flags *flag.FlagSet, args []string) (helped bool, err error) {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// runExport writes spaces to a backup file without asking for passwords.
//
// Whoever can run this can already read ~/.walletspace, so a password here
// would protect nothing; the browser export asks for one because a capability
// token is not the owner of the machine. Reading needs no process lock: the
// wallet only ever replaces space files whole.
func runExport(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("walletspace export", flag.ContinueOnError)
	outFlag := flags.String("out", "", "backup file to create")
	homeFlag := flags.String("home", "", "Walletspace home")
	var ids spaceIDs
	flags.Var(&ids, "space", "space id to export; repeat for several")
	if helped, err := parseFlags(flags, args); helped || err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	path := strings.TrimSpace(*outFlag)
	if path == "" {
		return errors.New("export requires --out")
	}
	// A backup holds every vault; replacing another file by mistake, perhaps
	// an older backup that is the only copy of something, is not worth saving
	// a keystroke.
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	home, err := storage.ResolveHome(*homeFlag)
	if err != nil {
		return err
	}
	data, err := space.ExportHome(home, ids)
	if err != nil {
		return err
	}
	files, err := space.ParseBundle(data)
	if err != nil {
		return err
	}
	if err := storage.AtomicWrite(path, data); err != nil {
		return err
	}
	fmt.Fprintf(out, "Exported %d space(s) to %s:\n", len(files), path)
	for _, file := range files {
		fmt.Fprintf(out, "  %s  %s\n", file.ID, file.Name)
	}
	fmt.Fprint(out, "\nEach vault in the file is still under its own password, but the file is as\n"+
		"sensitive as ~/.walletspace. Move it directly to the other machine and delete\n"+
		"it on both sides after importing.\n")
	return nil
}

// runImport adds the spaces of a backup file to a home. The wallet must not be
// running: it holds the home's spaces in memory and would not see them.
func runImport(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("walletspace import", flag.ContinueOnError)
	homeFlag := flags.String("home", "", "Walletspace home")
	if helped, err := parseFlags(flags, args); helped || err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("import takes exactly one backup file")
	}
	data, err := readBackup(flags.Arg(0))
	if err != nil {
		return err
	}
	home, err := storage.ResolveHome(*homeFlag)
	if err != nil {
		return err
	}
	processLock, err := storage.AcquireLock(home)
	if errors.Is(err, storage.ErrAlreadyLocked) {
		return fmt.Errorf("%w: quit Walletspace before importing, or import in the browser", err)
	}
	if err != nil {
		return err
	}
	defer processLock.Close()

	manager, err := space.NewManager(home, 15*time.Minute, vault.DefaultParams)
	if err != nil {
		return err
	}
	defer manager.Close()
	result, err := manager.ImportBundle(data)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Imported %d space(s):\n", len(result.Imported))
	for _, summary := range result.Imported {
		fmt.Fprintf(out, "  %s  %s\n", summary.ID, summary.Name)
	}
	if len(result.Skipped) > 0 {
		fmt.Fprintf(out, "Skipped %d space(s):\n", len(result.Skipped))
		for _, skipped := range result.Skipped {
			reason := "already on this machine"
			if skipped.Reason == space.SkipQuota {
				reason = "the space limit is reached"
			}
			fmt.Fprintf(out, "  %s  %s — %s\n", skipped.ID, skipped.Name, reason)
		}
	}
	fmt.Fprint(out, "\nThe spaces are locked; each opens with the password it had. Delete the\n"+
		"backup file now that it is imported.\n")
	return nil
}

// readBackup reads at most one byte past the bundle ceiling, so a wrong path
// pointing at something huge is refused without being loaded.
func readBackup(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, space.MaxBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}
