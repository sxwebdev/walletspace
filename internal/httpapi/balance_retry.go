package httpapi

import (
	"context"
	"time"

	"github.com/sxwebdev/walletspace/internal/chain"
)

// balanceRetryDelays are the pauses before each repeated read of the balances
// that failed. Public nodes answer a burst of balance calls with rate-limit
// errors now and then; a moment later the same call succeeds. Without a retry
// such a balance reached the UI as an error, and the wallet's total counted
// it as nothing.
var balanceRetryDelays = []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond}

// balancePass is one adapter read of every holder against every asset.
type balancePass func(
	ctx context.Context, holders []chain.AccountAddress, assets []chain.Asset, refresh bool,
) <-chan chain.Balance

// retryBalances forwards a balance pass as it arrives, except for the
// balances that failed: those are read again after each delay, alone and past
// any cache, and only a balance that failed every time is sent with its error.
// A balance the pass never answered is not invented; it stays missing, as it
// would have without a retry.
func retryBalances(
	ctx context.Context, pass balancePass,
	holders []chain.AccountAddress, assets []chain.Asset, refresh bool,
	delays []time.Duration,
) <-chan chain.Balance {
	out := make(chan chain.Balance)
	go func() {
		defer close(out)
		send := func(balance chain.Balance) bool {
			select {
			case out <- balance:
				return true
			case <-ctx.Done():
				return false
			}
		}
		holderByID := make(map[string]chain.AccountAddress, len(holders))
		for _, holder := range holders {
			holderByID[holder.AccountID] = holder
		}
		assetByID := make(map[string]chain.Asset, len(assets))
		for _, asset := range assets {
			assetByID[asset.ID] = asset
		}

		failed := make(map[[2]string]chain.Balance)
		collect := func(results <-chan chain.Balance) bool {
			for result := range results {
				key := [2]string{result.AccountID, result.AssetID}
				if result.Error != "" {
					failed[key] = result
					continue
				}
				delete(failed, key)
				if !send(result) {
					return false
				}
			}
			return true
		}
		if !collect(pass(ctx, holders, assets, refresh)) {
			return
		}
		for _, delay := range delays {
			if len(failed) == 0 {
				break
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
			// One pass per asset over exactly the holders that failed on it,
			// so nothing that already succeeded is read again.
			byAsset := make(map[string][]chain.AccountAddress)
			for key := range failed {
				holder, okHolder := holderByID[key[0]]
				_, okAsset := assetByID[key[1]]
				if okHolder && okAsset {
					byAsset[key[1]] = append(byAsset[key[1]], holder)
				}
			}
			for assetID, retryHolders := range byAsset {
				if !collect(pass(ctx, retryHolders, []chain.Asset{assetByID[assetID]}, true)) {
					return
				}
			}
		}
		for _, balance := range failed {
			if !send(balance) {
				return
			}
		}
	}()
	return out
}
