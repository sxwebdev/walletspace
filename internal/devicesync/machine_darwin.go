package devicesync

import (
	"context"
	"fmt"
)

// ioreg is named by its full path so that nothing earlier in PATH answers in
// its place.
const ioreg = "/usr/sbin/ioreg"

// lookupMachineID reads the Mac's hardware UUID, the IOPlatformUUID a clone of
// the disk does not carry to another Mac.
func lookupMachineID(ctx context.Context) (string, error) {
	out, err := runCommand(ctx, ioreg, "-rd1", "-c", "IOPlatformExpertDevice")
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoMachineID, err)
	}
	return parseIOPlatformUUID(out)
}
