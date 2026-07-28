package tron

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/gotron/pkg/address"
	"github.com/sxwebdev/gotron/pkg/client"
	"github.com/sxwebdev/gotron/pkg/client/abi"
	"github.com/sxwebdev/gotron/schema/pb/api"
	"github.com/sxwebdev/gotron/schema/pb/core"
)

const (
	// receiptPoll is how often the receipt is asked for. A block is three
	// seconds, so asking faster only spends the node's rate limit.
	receiptPoll = 3 * time.Second
	// receiptWait bounds the wait for it. Two blocks are usually enough; this
	// leaves room for a node that is a few behind, and giving up costs nothing
	// but the confirmation — the deployment is on its way either way.
	receiptWait = 24 * time.Second
)

// Deployment is a contract to put on chain.
//
// Amounts stay decimal and the two limits stay whole numbers: only FeeLimit is
// money, the other two are chain settings the contract is stored with.
type Deployment struct {
	// Name is kept with the contract on chain. It is metadata, not an address.
	Name string
	// ABI is the compiler's JSON — solc's array or Tron's {"entrys":[…]}
	// envelope. Optional: a contract deploys without one, but then nothing can
	// decode its calls afterwards.
	ABI string
	// Bytecode is the compiled contract as hex, 0x prefix optional. Constructor
	// arguments do not belong here — see ConstructorParams.
	Bytecode string
	// ConstructorParams are the constructor's arguments in gotron's JSON form,
	// e.g. `[{"uint256":"1000"},{"address":"T…"}]`. Empty when it takes none.
	ConstructorParams string
	// FeeLimit caps what the deployment may burn, in TRX.
	FeeLimit decimal.Decimal
	// ConsumeUserResourcePercent is the share (0..100) of each future call paid
	// by the caller rather than out of this contract's own resources.
	ConsumeUserResourcePercent int64
	// OriginEnergyLimit caps the energy the contract's owner pays per call.
	OriginEnergyLimit int64
}

// Deployed is what came of a deployment.
//
// The address is known before the transaction is broadcast — Tron derives it
// from the transaction itself — so it is reported even when the receipt never
// arrives. It only holds a contract if Failure is empty.
type Deployed struct {
	TxID    string
	Address string
	// Confirmed is set once the receipt has been read. False means the wait ran
	// out, not that anything went wrong.
	Confirmed bool
	// Failure names the VM's verdict when the deployment was mined and refused —
	// OUT_OF_ENERGY for a fee limit set too low, REVERT for a constructor that
	// gave up. Empty when the contract is there.
	Failure string
	// EnergyUsed and Fee are what it actually cost, once known.
	EnergyUsed int64
	Fee        decimal.Decimal
}

// DeployCost is what a deployment will cost, priced before anything is sent.
type DeployCost struct {
	// Energy and Bandwidth are resource units the deployment consumes, not TRX.
	Energy    decimal.Decimal
	Bandwidth decimal.Decimal
	// Fee is the TRX that will actually leave the account: whatever the sender's
	// own staked resources do not cover. Zero for an account that has staked
	// enough energy, which is the point of staking.
	Fee decimal.Decimal
	// MinFeeLimit is the smallest fee limit that covers this much energy at the
	// current price. It is not the same as Fee and is usually much larger: the
	// limit caps the energy the transaction may consume at all, so it has to
	// cover energy that comes free out of the sender's own stake.
	//
	// Zero when the node did not report an energy price, which leaves the
	// figure unknown rather than unlimited.
	MinFeeLimit decimal.Decimal
	// Shortfall is the TRX the sender is missing for Fee, zero when covered.
	Shortfall decimal.Decimal
}

// EstimateDeploy prices a deployment without sending it.
//
// This is worth more here than for a transfer. A deployment whose fee limit is
// too low is still accepted, mined and charged for, and only the receipt says
// OUT_OF_ENERGY — the fee is gone and no contract exists, so there is no second
// chance to find out what it costs. A constructor that reverts also fails here,
// before it costs anything.
func (s *Service) EstimateDeploy(ctx context.Context, from string, d Deployment) (DeployCost, error) {
	req, err := deployRequest(from, d)
	if err != nil {
		return DeployCost{}, err
	}

	// Nothing here is broadcast — the node builds a transaction and simulates
	// it — so every call is safe to route past an unhealthy endpoint.
	est, err := retry(ctx, s.nodes, func() (*client.EstimateTransferResult, error) {
		return s.client.EstimateDeployContract(ctx, req)
	})
	if err != nil {
		return DeployCost{}, s.chainError("estimate deployment", err)
	}

	cost := DeployCost{
		Energy:    est.Usage.Energy,
		Bandwidth: est.Usage.Bandwidth,
		Fee:       est.Fee.TRX(),
	}

	params, err := retry(ctx, s.nodes, func() (*client.ChainParams, error) {
		return s.client.ChainParams(ctx)
	})
	if err != nil {
		return DeployCost{}, s.chainError("read chain parameters", err)
	}

	if params.EnergyFee > 0 {
		cost.MinFeeLimit = cost.Energy.Mul(decimal.NewFromInt(params.EnergyFee)).Shift(-6)
	}

	// A balance the service could not read is not worth failing a priced
	// estimate over: the shortfall only adds a warning to it.
	if missing, err := s.missingTRX(ctx, from, cost.Fee); err != nil {
		s.log.Warn("deployment shortfall check failed", "from", from, "error", err)
	} else {
		cost.Shortfall = missing
	}

	return cost, nil
}

// Deploy puts a contract on chain from the given wallet and returns where it
// landed.
//
// Nothing is priced here — call EstimateDeploy for that. This is the point of
// no return: everything the chain would refuse outright is caught before the
// key is touched, and after that the transaction is the caller's to pay for.
func (s *Service) Deploy(ctx context.Context, from string, d Deployment, key *ecdsa.PrivateKey) (Deployed, error) {
	req, err := deployRequest(from, d)
	if err != nil {
		return Deployed{}, err
	}

	// Building is a read-only call on the node, so it is safe to route past an
	// unhealthy endpoint; only the broadcast inside submit must not be retried.
	tx, err := retry(ctx, s.nodes, func() (*api.TransactionExtention, error) {
		return s.client.DeployContract(ctx, req)
	})
	if err != nil {
		return Deployed{}, s.chainError("build deployment", err)
	}

	// Read before broadcasting and after gotron has applied the fee limit: the
	// address is derived from the transaction, and the fee limit is part of it.
	contract, err := client.DeployedContractAddress(tx.GetTransaction())
	if err != nil {
		return Deployed{}, fmt.Errorf("derive contract address: %w", err)
	}

	txid, err := s.submit(ctx, tx, key)
	if err != nil {
		return Deployed{}, err
	}

	// A deployment spends energy, or TRX when there is no energy staked.
	s.invalidate(from)

	out := Deployed{TxID: txid, Address: contract}
	s.awaitDeployment(ctx, &out)

	return out, nil
}

// Validate reports whether the chain would take this deployment, without
// spending a round-trip — or a key derivation — to find out.
//
// It exists so the HTTP layer can refuse a malformed deployment before it
// reaches the mnemonic. Deploy and EstimateDeploy validate again through the
// same code, so a caller that skips this loses nothing but the ordering.
func (d Deployment) Validate(from string) error {
	_, err := deployRequest(from, d)

	return err
}

// deployRequest validates the deployment and turns it into the request gotron
// takes.
//
// Everything here is refused without a node: an address that is not one, an
// unparseable ABI, bytecode that is empty or truncated, a percentage out of
// range, constructor arguments that do not encode. Each would otherwise cost a
// failed build against every node and come back as an upstream failure rather
// than as the bad request it is.
func deployRequest(from string, d Deployment) (client.DeployContractRequest, error) {
	if err := address.Validate(from); err != nil {
		return client.DeployContractRequest{}, fmt.Errorf("%w: invalid sender address: %s", ErrInvalidRequest, err)
	}

	// A deployment burns energy in proportion to the code its constructor runs,
	// far past what a transfer costs. gotron leaves the field unset for zero and
	// the node then applies its own default, which is well below that — so an
	// unset limit is a deployment that dies half-built with the fee spent. It is
	// refused here instead, where it can still be fixed.
	if d.FeeLimit.LessThanOrEqual(decimal.Zero) {
		return client.DeployContractRequest{}, fmt.Errorf(
			"%w: fee limit must be greater than zero — a deployment costs far more energy than a transfer", ErrInvalidRequest)
	}

	feeLimit, err := trxAmount(d.FeeLimit)
	if err != nil {
		return client.DeployContractRequest{}, err
	}

	var parsed *core.SmartContract_ABI
	if strings.TrimSpace(d.ABI) != "" {
		parsed, err = abi.LoadContractABI(d.ABI)
		if err != nil {
			return client.DeployContractRequest{}, fmt.Errorf("%w: contract ABI: %s", ErrInvalidRequest, err)
		}
	}

	req := client.DeployContractRequest{
		From:                       from,
		Name:                       d.Name,
		ABI:                        parsed,
		Bytecode:                   d.Bytecode,
		ConstructorParams:          d.ConstructorParams,
		FeeLimit:                   feeLimit,
		ConsumeUserResourcePercent: d.ConsumeUserResourcePercent,
		OriginEnergyLimit:          d.OriginEnergyLimit,
	}

	// gotron validates and builds in one pass, so this cannot drift from what is
	// actually sent — and it spends no round-trip to find out.
	if err := req.Validate(); err != nil {
		return client.DeployContractRequest{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}

	return req, nil
}

// awaitDeployment fills in what the receipt says, leaving the deployment
// unconfirmed if it does not arrive in time.
//
// A deployment is accepted long before it runs: the broadcast succeeds, the fee
// is charged, and only in a block does the VM find out whether the constructor
// fit inside the fee limit. Reporting the txid as a success would hand back an
// address that holds nothing, which is exactly what a fee limit set too low
// produces — the one failure worth naming.
func (s *Service) awaitDeployment(ctx context.Context, out *Deployed) {
	ctx, cancel := context.WithTimeout(ctx, receiptWait)
	defer cancel()

	ticker := time.NewTicker(receiptPoll)
	defer ticker.Stop()

	for {
		info, err := s.client.GetTransactionInfoByHash(ctx, out.TxID)
		if err == nil {
			out.Confirmed = true
			out.EnergyUsed = info.GetReceipt().GetEnergyUsageTotal()
			out.Fee = client.SUN(info.GetFee()).TRX()
			out.Failure = deployFailure(info)

			return
		}

		// Not in a block yet is the ordinary answer for the first few seconds,
		// and a node that is briefly unreachable is worth another try rather
		// than an unconfirmed result.
		if !errors.Is(err, client.ErrTransactionInfoNotFound) {
			s.log.Debug("read deployment receipt", "txid", out.TxID, "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// deployFailure reads the VM's verdict out of a receipt, returning an empty
// string when the contract is there.
//
// DEFAULT counts as success: it is the zero value, which a receipt carrying no
// contract result at all also has, and calling that a failed deployment would
// be worse than saying nothing.
func deployFailure(info *core.TransactionInfo) string {
	result := info.GetReceipt().GetResult()
	if result == core.Transaction_Result_SUCCESS || result == core.Transaction_Result_DEFAULT {
		return ""
	}

	// The message carries the revert reason when there is one; the enum alone
	// says OUT_OF_ENERGY, which is the common case and needs no elaboration.
	if reason := strings.TrimSpace(string(info.GetResMessage())); reason != "" {
		return fmt.Sprintf("%s: %s", result, reason)
	}

	return result.String()
}
