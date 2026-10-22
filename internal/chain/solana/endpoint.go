package solana

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	"github.com/sxwebdev/walletspace/internal/network"
)

// genesisHashes names each Solana cluster by the hash of its genesis block,
// keyed by the network's ChainID. The genesis hash is the one identity a node
// cannot share with another cluster: devnet and mainnet answer every other
// question in the same shape.
var genesisHashes = map[string]string{
	"mainnet": "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
	"devnet":  "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG",
}

const (
	// maxHeadAge is how old the newest confirmed block may be before a node
	// is refused. A blockhash stays usable for 150 blocks, about 60–90 s, so
	// a node whose head is two minutes old can only hand out blockhashes that
	// have already expired: every transaction built from it would be dropped.
	// It is also well past the ~50 s (128 slots) a node may lag and still call
	// itself healthy, so a node getHealth accepts is not refused for ordinary
	// lag, and past normal block-time drift of a few seconds.
	maxHeadAge = 2 * time.Minute
	// maxHeadAhead tolerates a local clock that runs behind the cluster, the
	// same allowance the Tron probe makes.
	maxHeadAhead = 2 * time.Minute

	// blockNotAvailable is the node's answer for a block it has not stored.
	blockNotAvailable = -32004
	// blockTimeRetries and slotStepBack bound the walk back from a head
	// block a lagging backend does not have yet: at most 8 slots, about 3 s.
	blockTimeRetries = 2
	slotStepBack     = 4
	// slotDuration is the cluster's target slot time.
	slotDuration = 400 * time.Millisecond
)

// Family is the network family this adapter serves.
const Family = network.FamilySolana

// expectedGenesis returns the genesis hash a network's endpoints must report.
func expectedGenesis(item network.Network) (string, error) {
	if item.Family != Family {
		return "", errors.New("network is not Solana")
	}
	want, ok := genesisHashes[item.ChainID]
	if !ok {
		return "", fmt.Errorf("unknown Solana cluster %q", item.ChainID)
	}
	return want, nil
}

// VerifyEndpoint checks that an endpoint serves the network's cluster: its
// genesis hash must be the one the network's ChainID names. It costs one
// request, cheap enough to run when a user saves an endpoint.
func VerifyEndpoint(
	ctx context.Context, item network.Network, endpoint string, headers http.Header, httpClient *http.Client,
) error {
	want, err := expectedGenesis(item)
	if err != nil {
		return err
	}
	client, err := newRPC(endpoint, headers, httpClient)
	if err != nil {
		return err
	}
	return verifyGenesis(ctx, client, want)
}

func verifyGenesis(ctx context.Context, client *rpc.Client, want string) error {
	got, err := client.GetGenesisHash(ctx)
	if err = readable(err); err != nil {
		return fmt.Errorf("read Solana genesis hash: %w", err)
	}
	if got.String() != want {
		return fmt.Errorf("Solana genesis hash is %s, expected %s", got, want)
	}
	return nil
}

// ProbeEndpoint is the Node Doctor's check: the cluster identity, the node's
// own health verdict, and the age of its newest confirmed block.
//
// getHealth alone trusts the node to know it is behind; the block time also
// catches a node that is not, but whose cluster has stopped producing blocks —
// where no transaction can land either.
func ProbeEndpoint(
	ctx context.Context, item network.Network, endpoint string, headers http.Header, httpClient *http.Client,
) error {
	want, err := expectedGenesis(item)
	if err != nil {
		return err
	}
	client, err := newRPC(endpoint, headers, httpClient)
	if err != nil {
		return err
	}
	if err := verifyGenesis(ctx, client, want); err != nil {
		return err
	}
	health, err := client.GetHealth(ctx)
	if err = readable(err); err != nil {
		return fmt.Errorf("Solana node is unhealthy: %w", err)
	}
	if health != rpc.HealthOk {
		return fmt.Errorf("Solana node reports health %q", health)
	}
	slot, err := client.GetSlot(ctx, rpc.CommitmentConfirmed)
	if err = readable(err); err != nil {
		return fmt.Errorf("read Solana confirmed slot: %w", err)
	}
	if slot == 0 {
		return errors.New("Solana confirmed slot is zero")
	}
	blockTime, stepped, err := headBlockTime(ctx, client, slot)
	if err != nil {
		return fmt.Errorf("read Solana block time: %w", err)
	}
	if blockTime == nil || *blockTime <= 0 {
		return errors.New("Solana head block has no time")
	}
	// A block read a few slots back is that much older than the head; the
	// age is the head's, so the step-back is not held against the node.
	age := time.Since(blockTime.Time()) - time.Duration(stepped)*slotDuration
	if age < -maxHeadAhead || age > maxHeadAge {
		return fmt.Errorf("Solana head block is stale: age %s", age.Round(time.Second))
	}
	return nil
}

// headBlockTime reads the time of the confirmed head block. Behind a load
// balancer the slot and the time can come from two backends, and the one
// asked second may not have stored that block yet (-32004). Only then is a
// slightly earlier block asked instead, at most blockTimeRetries times; any
// other answer is final. It returns how many slots it stepped back.
func headBlockTime(ctx context.Context, client *rpc.Client, slot uint64) (*solana.UnixTimeSeconds, uint64, error) {
	var stepped uint64
	for attempt := 0; ; attempt++ {
		blockTime, err := client.GetBlockTime(ctx, slot-stepped)
		err = readable(err)
		if err == nil {
			return blockTime, stepped, nil
		}
		rpcErr, ok := errors.AsType[*jsonrpc.RPCError](err)
		if !ok || rpcErr.Code != blockNotAvailable || attempt == blockTimeRetries || slot <= stepped+slotStepBack {
			return nil, 0, err
		}
		stepped += slotStepBack
	}
}

// VerifyEndpoint is the package-level check over the provider's guarded client.
func (a *Adapter) VerifyEndpoint(
	ctx context.Context, item network.Network, endpoint string, headers http.Header,
) error {
	return VerifyEndpoint(ctx, item, endpoint, headers, a.endpoints.HTTPClient(item))
}

// ProbeEndpoint is the package-level probe over the provider's guarded client.
func (a *Adapter) ProbeEndpoint(
	ctx context.Context, item network.Network, endpoint string, headers http.Header,
) error {
	return ProbeEndpoint(ctx, item, endpoint, headers, a.endpoints.HTTPClient(item))
}
