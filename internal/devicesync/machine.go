package devicesync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// errNoMachineID reports a machine whose id could not be read. The device id
// is then kept as it is: binding it to anything else would either change it
// on every start or bind it to nothing.
var errNoMachineID = errors.New("this machine's id could not be read")

// machineIDTimeout bounds asking the system for its id. ioreg answers in tens
// of milliseconds; one that does not answer must not hold up the start.
const machineIDTimeout = 5 * time.Second

// machineID is this machine's id, read once per process: it does not change
// while the wallet runs, and ioreg is a process to start. A failure is kept
// too; the next start asks again.
var machineID = sync.OnceValues(func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), machineIDTimeout)
	defer cancel()
	return lookupMachineID(ctx)
})

var (
	ioPlatformUUID = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})"`)
	systemdID      = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// parseIOPlatformUUID finds the hardware UUID in what
// `ioreg -rd1 -c IOPlatformExpertDevice` printed.
func parseIOPlatformUUID(out []byte) (string, error) {
	match := ioPlatformUUID.FindSubmatch(out)
	if match == nil {
		return "", fmt.Errorf("%w: ioreg printed no IOPlatformUUID", errNoMachineID)
	}
	return strings.ToUpper(string(match[1])), nil
}

// machineIDFromFiles reads the first of paths that holds a machine id in
// systemd's form. An empty file or "uninitialized" — a system that has not
// finished its first boot — is no id at all.
func machineIDFromFiles(paths ...string) (string, error) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(data)); systemdID.MatchString(id) {
			return id, nil
		}
	}
	return "", fmt.Errorf("%w: none of %s holds one", errNoMachineID, strings.Join(paths, ", "))
}
