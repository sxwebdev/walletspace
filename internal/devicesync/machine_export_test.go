package devicesync

// DeviceIDOn is DeviceID on a machine whose id machine returns.
func DeviceIDOn(home string, machine func() (string, error)) (string, error) {
	return deviceID(home, machine)
}

var (
	ParseIOPlatformUUID = parseIOPlatformUUID
	MachineIDFromFiles  = machineIDFromFiles
	LookupMachineID     = lookupMachineID
	ErrNoMachineID      = errNoMachineID
)
