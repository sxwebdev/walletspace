package refuel_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/refuel"
)

func TestTopUpPlanSurvivesRestartAndPreventsSecondPayment(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store := refuel.NewStore(home)
	const space = "spc_0123456789abcdef0123456789abcdef"
	plan := refuel.Plan{NetworkID: "kaia-mainnet", Target: refuel.Target{AccountID: "wallet", To: recipient, Amount: "3"}, Quote: refuel.Quote{ExpiresAt: time.Now().Add(time.Minute)}}
	first, err := store.Put(space, plan)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(space, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Claim(space, first.ID); err != nil {
		t.Fatal(err)
	}
	reopened := refuel.NewStore(home)
	got, err := reopened.Get(space, first.ID)
	if err != nil || !got.Started || got.Target.To != recipient {
		t.Fatalf("reloaded = %+v, %v", got, err)
	}
	if err := reopened.Claim(space, second.ID); err == nil {
		t.Fatal("second plan claimed while payment unresolved")
	}
	active, found, err := reopened.Active(space, "kaia-mainnet", "wallet")
	if err != nil || !found || active.ID != first.ID {
		t.Fatalf("active = %+v, %t, %v", active, found, err)
	}
	if err := reopened.Mark(space, first.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Claim(space, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Get("spc_abcdef0123456789abcdef0123456789", first.ID); !errors.Is(err, refuel.ErrPlanNotFound) {
		t.Fatalf("cross-space lookup = %v", err)
	}
}
