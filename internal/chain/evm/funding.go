package evm

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/chain"
)

// EstimateFunding prices the actual call even when a token owner has no gas.
// The balance override is used for simulation only; token storage, sender,
// recipient and amount remain real. A node without this feature fails closed.
func (a *Adapter) EstimateFunding(ctx context.Context, networkID string, req chain.TransferRequest) (chain.TransferEstimate, error) {
	item, client, err := a.client(ctx, networkID)
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	call, _, err := a.transferCall(item, req)
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	estimate, err := a.EstimateTransfer(ctx, networkID, req)
	if err != nil && req.Asset.Kind == "erc20" && req.Action == "" {
		gas, simErr := estimateWithBalance(ctx, client, call)
		if simErr != nil {
			return chain.TransferEstimate{}, fmt.Errorf("cannot simulate token transfer without gas: %w", simErr)
		}
		fees, feeErr := suggestFees(ctx, client)
		if feeErr != nil {
			return chain.TransferEstimate{}, feeErr
		}
		estimate = chain.TransferEstimate{NetworkID: networkID, Amount: req.Amount, GasLimit: gas, FeeModel: fees.model, MaxFeePerGas: fees.feeCap.String(), MaxPriorityFeePerGas: fees.tipCap.String(),
			Fee: decimal.NewFromBigInt(new(big.Int).Mul(new(big.Int).SetUint64(gas), fees.maxFee()), -int32(item.Native.Decimals)).String()}
	} else if err != nil {
		return chain.TransferEstimate{}, err
	}
	extra, err := rollupFee(ctx, client, item.ID, item.ChainID, call, estimate)
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	fee, _ := decimal.NewFromString(estimate.Fee)
	fee = fee.Add(decimal.NewFromBigInt(extra, -int32(item.Native.Decimals)))
	// Current-price estimate is displayed separately from the approved cap:
	// EIP-1559 usually refunds part of that cap, which otherwise hides dust.
	expectedGasPrice, _ := new(big.Int).SetString(estimate.MaxFeePerGas, 10)
	if estimate.FeeModel == "eip1559" {
		tip, _ := new(big.Int).SetString(estimate.MaxPriorityFeePerGas, 10)
		expectedGasPrice.Sub(expectedGasPrice, tip).Quo(expectedGasPrice, big.NewInt(2)).Add(expectedGasPrice, tip)
	}
	expectedFee := new(big.Int).Mul(expectedGasPrice, new(big.Int).SetUint64(estimate.GasLimit))
	expectedFee.Add(expectedFee, extra)
	estimate.EstimatedFee = decimal.NewFromBigInt(expectedFee, -int32(item.Native.Decimals)).String()
	estimate.Fee = fee.String()
	estimate.AdditionalFee = decimal.NewFromBigInt(extra, -int32(item.Native.Decimals)).String()
	balance, err := client.PendingBalanceAt(ctx, call.From)
	if err != nil {
		return chain.TransferEstimate{}, fmt.Errorf("read spendable native balance: %w", err)
	}
	estimate.NativeBalance = decimal.NewFromBigInt(balance, -int32(item.Native.Decimals)).String()
	available, _ := decimal.NewFromString(estimate.NativeBalance)
	required := fee
	if req.Asset.Kind == "native" {
		value, _ := decimal.NewFromString(req.Amount)
		required = required.Add(value)
	}
	shortfall := required.Sub(available)
	if shortfall.IsNegative() {
		shortfall = decimal.Zero
	}
	estimate.FeeShortfall = shortfall.String()
	return estimate, nil
}

func estimateWithBalance(ctx context.Context, client *ethclient.Client, call ethereum.CallMsg) (uint64, error) {
	args := map[string]any{"from": call.From, "to": call.To, "data": hexutil.Bytes(call.Data), "value": (*hexutil.Big)(call.Value)}
	overrides := map[common.Address]any{call.From: map[string]string{"balance": "0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}}
	var gas hexutil.Uint64
	err := client.Client().CallContext(ctx, &gas, "eth_estimateGas", args, "latest", overrides)
	return uint64(gas), err
}

// OP Stack fees also include L1 data and operator charges. Never budget a
// top-up using L2 execution gas alone. Other adapters include data gas in their
// estimate (Arbitrum) or have no separate rollup fee.
func rollupFee(ctx context.Context, client *ethclient.Client, networkID, chainID string, call ethereum.CallMsg, estimate chain.TransferEstimate) (*big.Int, error) {
	if networkID != "base-mainnet" && networkID != "optimism-mainnet" && networkID != "optimism-sepolia" {
		return new(big.Int), nil
	}
	id, _ := new(big.Int).SetString(chainID, 10)
	cap, _ := new(big.Int).SetString(estimate.MaxFeePerGas, 10)
	tip, _ := new(big.Int).SetString(estimate.MaxPriorityFeePerGas, 10)
	// A maximal nonce bounds the RLP size before signing. The oracle accounts
	// for the signature overhead itself.
	tx := types.NewTx(&types.DynamicFeeTx{ChainID: id, Nonce: ^uint64(0), GasTipCap: tip, GasFeeCap: cap, Gas: estimate.GasLimit, To: call.To, Value: call.Value, Data: call.Data})
	wire, err := tx.MarshalBinary()
	if err != nil {
		return nil, err
	}
	oracle := common.HexToAddress("0x420000000000000000000000000000000000000F")
	total := new(big.Int)
	for _, method := range []struct {
		name  string
		value uint64
	}{{"getL1FeeUpperBound(uint256)", uint64(len(wire))}, {"getOperatorFee(uint256)", estimate.GasLimit}} {
		data := append(crypto.Keccak256([]byte(method.name))[:4], common.LeftPadBytes(new(big.Int).SetUint64(method.value).Bytes(), 32)...)
		output, err := client.CallContract(ctx, ethereum.CallMsg{To: &oracle, Data: data}, nil)
		if err != nil || len(output) != 32 {
			return nil, fmt.Errorf("%w: cannot estimate OP Stack %s", chain.ErrNotSupported, method.name)
		}
		total.Add(total, new(big.Int).SetBytes(output))
	}
	return total, nil
}

// NativeDelivery verifies the provider's delivery against the destination
// chain itself, including the recipient and amount, before unblocking a send.
func (a *Adapter) NativeDelivery(ctx context.Context, networkID, hash, recipient string, minimum *big.Int) (bool, error) {
	status, err := a.Transaction(ctx, networkID, hash)
	if err != nil {
		return false, err
	}
	if status.Status != "confirmed" {
		return false, nil
	}
	item, client, err := a.client(ctx, networkID)
	if err != nil {
		return false, err
	}
	tx, pending, err := client.TransactionByHash(ctx, common.HexToHash(hash))
	if err != nil {
		return false, err
	}
	id, _ := new(big.Int).SetString(item.ChainID, 10)
	if pending || tx.To() == nil || *tx.To() != common.HexToAddress(recipient) || minimum == nil || tx.Value().Cmp(minimum) < 0 || tx.ChainId().Cmp(id) != 0 || tx.Hash() != common.HexToHash(hash) {
		return false, fmt.Errorf("%w: provider delivery does not match this native top-up", chain.ErrInvalidRequest)
	}
	return true, nil
}
