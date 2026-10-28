//go:build !darwin

package devicesync

import "context"

// lookupMachineID reads systemd's machine id, or D-Bus's where an older
// system keeps it only there. A system with neither has no id to bind to.
func lookupMachineID(context.Context) (string, error) {
	return machineIDFromFiles("/etc/machine-id", "/var/lib/dbus/machine-id")
}
