package wallet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sxwebdev/gotron/pkg/address"
)

// MnemonicFileName is the file the generated mnemonic is stored in.
const MnemonicFileName = "mnemonic.txt"

// ResolveMnemonic returns the mnemonic to derive every wallet from.
//
// Precedence: the configured value, then {dataDir}/mnemonic.txt, then a freshly
// generated 24-word mnemonic which is written to that file with 0600
// permissions. The second return value reports whether a new mnemonic was
// generated, so the caller can warn the user to back it up.
func ResolveMnemonic(configured, dataDir string) (string, bool, error) {
	if m := strings.TrimSpace(configured); m != "" {
		return m, false, nil
	}

	path := filepath.Join(dataDir, MnemonicFileName)

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		m := strings.TrimSpace(string(data))
		if m == "" {
			return "", false, fmt.Errorf("%s is empty: delete it to generate a new mnemonic", path)
		}

		return m, false, nil

	case !errors.Is(err, os.ErrNotExist):
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}

	m, err := address.GenerateMnemonic(256)
	if err != nil {
		return "", false, fmt.Errorf("generate mnemonic: %w", err)
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", false, fmt.Errorf("create %s: %w", dataDir, err)
	}

	if err := os.WriteFile(path, []byte(m+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("write %s: %w", path, err)
	}

	return m, true, nil
}
