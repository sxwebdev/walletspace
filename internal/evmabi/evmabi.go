// Package evmabi holds what the packages that encode contract calls by hand
// share about ABI definitions.
package evmabi

import (
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

// MustParse parses an ABI written into a package as a literal. Such a literal
// is fixed at build time, so a typo in it is a bug of the package that owns it:
// the panic names that package and fires while it initializes, which fails
// every test that imports it.
func MustParse(owner, definition string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(definition))
	if err != nil {
		panic(owner + ": bad ABI: " + err.Error())
	}
	return parsed
}
