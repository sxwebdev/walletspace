package evmabi_test

import (
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/evmabi"
)

func TestMustParse(t *testing.T) {
	t.Parallel()
	parsed := evmabi.MustParse("owner", `[{"type":"function","name":"approve","inputs":[{"name":"spender","type":"address"},{"name":"amount","type":"uint256"}]}]`)
	if got := parsed.Methods["approve"].Sig; got != "approve(address,uint256)" {
		t.Fatalf("approve signature = %q", got)
	}
}

func TestMustParsePanicsNamingTheOwner(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		message, ok := r.(string)
		if !ok || !strings.HasPrefix(message, "owner: bad ABI: ") {
			t.Fatalf("recovered %v, want a panic naming the owner", r)
		}
	}()
	evmabi.MustParse("owner", `[{"type":"function"`)
}
