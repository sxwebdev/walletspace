package devicesync_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"testing"

	"github.com/sxwebdev/walletspace/internal/devicesync"
)

const (
	machineA = "4C4C4544-0000-1111-2222-333344445555"
	machineB = "4C4C4544-0000-1111-2222-666677778888"
)

func on(id string) func() (string, error) {
	return func() (string, error) { return id, nil }
}

var unreadable = func() (string, error) { return "", devicesync.ErrNoMachineID }

func deviceIDOn(t *testing.T, home string, machine func() (string, error)) string {
	t.Helper()
	id, err := devicesync.DeviceIDOn(home, machine)
	if err != nil {
		t.Fatalf("DeviceID() error = %v", err)
	}
	return id
}

type storedDevice struct {
	DeviceID string `json:"device_id"`
	Binding  string `json:"binding"`
}

func readDevice(t *testing.T, home string) storedDevice {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored storedDevice
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func writeDevice(t *testing.T, home string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "device.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func bindingOf(machine, home string) string {
	sum := sha256.Sum256([]byte(machine + home))
	return hex.EncodeToString(sum[:])
}

func TestADeviceIDBelongsToItsMachineAndFolder(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	original := deviceIDOn(t, home, on(machineA))
	if got := readDevice(t, home); got.DeviceID != original || got.Binding != bindingOf(machineA, home) {
		t.Fatalf("device.json = %+v, want %s bound to machine A and %s", got, original, home)
	}
	if again := deviceIDOn(t, home, on(machineA)); again != original {
		t.Fatalf("same machine and folder: id %s became %s", original, again)
	}

	// The data folder copied to another path on the same machine.
	copied := t.TempDir()
	data, err := os.ReadFile(filepath.Join(home, "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeDevice(t, copied, data)
	moved := deviceIDOn(t, copied, on(machineA))
	if moved == original {
		t.Fatal("a copy at another path kept the original's device id")
	}
	if again := deviceIDOn(t, copied, on(machineA)); again != moved {
		t.Fatalf("the copy's new id %s did not stay: %s", moved, again)
	}

	// The same folder, at the same path, on another machine.
	cloned := deviceIDOn(t, home, on(machineB))
	if cloned == original || cloned == moved {
		t.Fatalf("a copy on another machine got id %s (original %s, moved %s)", cloned, original, moved)
	}
	if got := readDevice(t, home); got.DeviceID != cloned || got.Binding != bindingOf(machineB, home) {
		t.Fatalf("device.json after the move = %+v, want %s bound to machine B", got, cloned)
	}
}

func TestADeviceIDMadeBeforeBindingIsAdopted(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	writeDevice(t, home, []byte(`{"device_id":"dev_madebefore"}`+"\n"))
	if got := deviceIDOn(t, home, on(machineA)); got != "dev_madebefore" {
		t.Fatalf("DeviceID() = %s, want the id already there", got)
	}
	if got := readDevice(t, home); got.Binding != bindingOf(machineA, home) {
		t.Fatalf("binding = %q, want machine A's", got.Binding)
	}
	// Adopted for real: from now on another machine is a move.
	if got := deviceIDOn(t, home, on(machineB)); got == "dev_madebefore" {
		t.Fatal("an adopted id followed the folder to another machine")
	}
}

func TestADeviceIDStaysWhenTheMachineIDCannotBeRead(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	bound := deviceIDOn(t, home, on(machineA))
	before, err := os.ReadFile(filepath.Join(home, "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, machine := range []func() (string, error){unreadable, on("")} {
		if got := deviceIDOn(t, home, machine); got != bound {
			t.Fatalf("DeviceID() without a machine id = %s, want %s", got, bound)
		}
	}
	after, err := os.ReadFile(filepath.Join(home, "device.json"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("device.json changed without a machine id:\n%s\nwant\n%s", after, before)
	}

	// Made where the id cannot be read: no binding that would differ at the
	// next start, and the first start that can read it adopts the id.
	fresh := t.TempDir()
	made := deviceIDOn(t, fresh, unreadable)
	if got := readDevice(t, fresh); got.Binding != "" {
		t.Fatalf("binding made without a machine id = %q", got.Binding)
	}
	if again := deviceIDOn(t, fresh, unreadable); again != made {
		t.Fatalf("id without a machine id changed: %s, then %s", made, again)
	}
	if adopted := deviceIDOn(t, fresh, on(machineA)); adopted != made {
		t.Fatalf("id %s was not adopted once the machine id was readable: %s", made, adopted)
	}
}

func TestDeviceIDCallsAtOnceAgreeOnOneID(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	const callers = 16
	ids := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() { ids[i], errs[i] = devicesync.DeviceIDOn(home, on(machineA)) })
	}
	wg.Wait()
	stored := readDevice(t, home).DeviceID
	for i := range callers {
		if errs[i] != nil || ids[i] != stored {
			t.Fatalf("caller %d got %q, %v; device.json holds %q", i, ids[i], errs[i], stored)
		}
	}
}

func TestIOPlatformUUIDIsReadFromIoreg(t *testing.T) {
	t.Parallel()

	out := []byte(`+-o J316sAP  <class IOPlatformExpertDevice, id 0x100000245, registered, matched, active, busy 0 (13 ms), retain 47>
    {
      "IOPlatformSerialNumber" = "C02XXXXXXXXX"
      "IOPlatformUUID" = "4c4c4544-0000-1111-2222-333344445555"
      "model" = <"MacBookPro18,2">
    }
`)
	got, err := devicesync.ParseIOPlatformUUID(out)
	if err != nil || got != machineA {
		t.Fatalf("ParseIOPlatformUUID() = %q, %v; want %q", got, err, machineA)
	}
	for _, bad := range []string{"", `"IOPlatformUUID" = ""`, `"IOPlatformUUID" = "not-a-uuid"`, `"IOPlatformSerialNumber" = "4C4C4544-0000-1111-2222-333344445555"`} {
		if got, err := devicesync.ParseIOPlatformUUID([]byte(bad)); !errors.Is(err, devicesync.ErrNoMachineID) {
			t.Errorf("ParseIOPlatformUUID(%q) = %q, %v; want ErrNoMachineID", bad, got, err)
		}
	}
}

func TestMachineIDIsReadFromTheFirstFileThatHoldsOne(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const id = "0123456789abcdef0123456789abcdef"
	missing := filepath.Join(dir, "missing")
	uninitialized := write("uninitialized", "uninitialized\n")
	empty := write("empty", "")
	good := write("good", id+"\n")
	other := write("other", "fedcba9876543210fedcba9876543210\n")

	if got, err := devicesync.MachineIDFromFiles(missing, uninitialized, empty, good, other); err != nil || got != id {
		t.Fatalf("MachineIDFromFiles() = %q, %v; want %q", got, err, id)
	}
	if got, err := devicesync.MachineIDFromFiles(missing, uninitialized, empty); !errors.Is(err, devicesync.ErrNoMachineID) {
		t.Fatalf("MachineIDFromFiles(no id) = %q, %v; want ErrNoMachineID", got, err)
	}
}

func TestThisMachineHasAnID(t *testing.T) {
	t.Parallel()

	got, err := devicesync.LookupMachineID(t.Context())
	switch runtime.GOOS {
	case "darwin":
		if err != nil || !regexp.MustCompile(`^[0-9A-F]{8}(-[0-9A-F]{4}){3}-[0-9A-F]{12}$`).MatchString(got) {
			t.Fatalf("LookupMachineID() = %q, %v; want the IOPlatformUUID", got, err)
		}
	default:
		if err != nil {
			t.Skipf("no machine id here: %v", err)
		}
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(got) {
			t.Fatalf("LookupMachineID() = %q", got)
		}
	}
}
