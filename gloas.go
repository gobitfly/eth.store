package ethstore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/attestantio/go-eth2-client/api"
	ethHttp "github.com/attestantio/go-eth2-client/http"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethRPC "github.com/ethereum/go-ethereum/rpc"
	"golang.org/x/sync/errgroup"
)

// ErrPayloadsNotFinalized means the day's gloas payloads are not all decided by
// the finalized chain yet. It is expected right after a day's end is finalized,
// since the payload at endSlot is only decided by a later block: retry later.
var ErrPayloadsNotFinalized = errors.New("gloas payloads of the day are not decided by the finalized chain yet")

// payloadBlock is the part of a block needed to resolve Gloas payloads, which
// can only be done once the neighbouring blocks are known.
type payloadBlock struct {
	Slot            uint64
	ProposerIndex   phase0.ValidatorIndex
	Gloas           bool
	ParentRoot      phase0.Root
	BlockHash       phase0.Hash32
	ParentBlockHash phase0.Hash32
}

func newPayloadBlock(slot uint64, d *BlockData) *payloadBlock {
	return &payloadBlock{
		Slot:            slot,
		ProposerIndex:   d.ProposerIndex,
		Gloas:           d.Version >= spec.DataVersionGloas,
		ParentRoot:      d.ParentRoot,
		BlockHash:       d.ExecutionBlockHash,
		ParentBlockHash: d.ParentExecutionBlockHash,
	}
}

// resolveGloasPayloads works out, for blocks sorted by slot, which Gloas
// payloads became canonical and which payload paid the withdrawals that each
// Gloas block took off validator balances.
//
// From Gloas on, a block debits the withdrawals from the balances itself, but
// only when its parent's payload was FULL. The execution layer pays them in the
// next canonical FULL payload, which is the block's own payload or, after EMPTY
// slots, a later one. A payload is FULL when the next block's bid builds on it.
//
// parentBlockHash is the execution block hash committed by the parent of the
// first block. full holds the slots whose FULL/EMPTY status is decided, payers
// maps each debiting slot to the slot whose payload paid it, and pending holds
// the debiting slots whose payload is not decided yet.
func resolveGloasPayloads(parentBlockHash phase0.Hash32, blocks []*payloadBlock) (full map[uint64]bool, payers map[uint64]uint64, pending []uint64) {
	full = map[uint64]bool{}
	payers = map[uint64]uint64{}
	prevHash := parentBlockHash
	var prev *payloadBlock
	for _, b := range blocks {
		if b.Gloas {
			parentFull := b.ParentBlockHash == prevHash
			if prev != nil && prev.Gloas {
				full[prev.Slot] = parentFull
				if parentFull {
					for _, s := range pending {
						payers[s] = prev.Slot
					}
					pending = nil
				}
			}
			if parentFull {
				pending = append(pending, b.Slot)
			}
		}
		prevHash = b.BlockHash
		prev = b
	}
	return full, payers, pending
}

// gloasResolved reports whether every Gloas payload in [firstSlot,endSlot) is
// decided and every withdrawal debited in (firstSlot,endSlot] has a payer.
func gloasResolved(blocks []*payloadBlock, full map[uint64]bool, pending []uint64, endSlot uint64) bool {
	for _, s := range pending {
		if s <= endSlot {
			return false
		}
	}
	for _, b := range blocks {
		if b.Gloas && b.Slot < endSlot {
			if _, ok := full[b.Slot]; !ok {
				return false
			}
		}
	}
	return true
}

type elBlock struct {
	Hash          common.Hash     `json:"hash"`
	Number        hexutil.Uint64  `json:"number"`
	BaseFeePerGas *hexutil.Big    `json:"baseFeePerGas"`
	Transactions  []common.Hash   `json:"transactions"`
	Withdrawals   []*elWithdrawal `json:"withdrawals"`
}

type elWithdrawal struct {
	Index          hexutil.Uint64 `json:"index"`
	ValidatorIndex hexutil.Uint64 `json:"validatorIndex"`
	Amount         hexutil.Uint64 `json:"amount"`
}

func requestElBlock(elClient *gethRPC.Client, slot uint64, hash phase0.Hash32) (*elBlock, error) {
	var blk *elBlock
	var err error
	for j := 0; j < 10; j++ {
		ctx, cancel := context.WithTimeout(context.Background(), GetExecTimeout())
		blk = nil
		err = elClient.CallContext(ctx, &blk, "eth_getBlockByHash", common.Hash(hash), false)
		cancel()
		if err == nil {
			break
		}
		log.Printf("error doing eth_getBlockByHash for slot %v: %v", slot, err)
		time.Sleep(time.Duration(j) * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("error doing eth_getBlockByHash for slot %v: %w", slot, err)
	}
	if blk == nil {
		return nil, fmt.Errorf("execution block %#x of the canonical payload at slot %v not found", hash, slot)
	}
	if blk.Hash != common.Hash(hash) {
		return nil, fmt.Errorf("execution block for slot %v has hash %v, requested %#x", slot, blk.Hash, hash)
	}
	if blk.BaseFeePerGas == nil || blk.Withdrawals == nil {
		return nil, fmt.Errorf("execution block %#x for slot %v lacks baseFeePerGas or withdrawals", hash, slot)
	}
	return blk, nil
}

func getPayloadBlock(ctx context.Context, client *ethHttp.Service, blockID string) (*payloadBlock, error) {
	var res *api.Response[*spec.VersionedSignedBeaconBlock]
	var err error
	for j := 0; j < 10; j++ {
		res, err = client.SignedBeaconBlock(ctx, &api.SignedBeaconBlockOpts{Block: blockID})
		if err == nil {
			break
		}
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			break
		}
		log.Printf("error retrieving beacon block %v: %v", blockID, err)
		time.Sleep(time.Duration(j) * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("error getting block %v: %w", blockID, err)
	}
	slot, err := res.Data.Slot()
	if err != nil {
		return nil, fmt.Errorf("error getting slot of block %v: %w", blockID, err)
	}
	d, err := GetBlockData(res.Data)
	if err != nil {
		return nil, fmt.Errorf("error getting blockData for block %v: %w", blockID, err)
	}
	return newPayloadBlock(uint64(slot), d), nil
}

// getSlotPayloadBlock returns nil when the slot has no block.
func getSlotPayloadBlock(ctx context.Context, client *ethHttp.Service, slot uint64) (*payloadBlock, error) {
	b, err := getPayloadBlock(ctx, client, fmt.Sprintf("%d", slot))
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}

// accountGloasPayloads resolves the Gloas payloads of the day and adds their tx
// fees and withdrawals to the validators. blocks holds every block in
// [firstSlot,endSlot]; blocks after endSlot are fetched as far as needed.
func accountGloasPayloads(ctx context.Context, client *ethHttp.Service, elClient *gethRPC.Client, receiptsMode, concurrency int, blocks []*payloadBlock, firstSlot, endSlot, maxSlot uint64, validatorsByIndex map[phase0.ValidatorIndex]*Validator) error {
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Slot < blocks[j].Slot })
	if len(blocks) == 0 {
		return nil
	}

	var parentBlockHash phase0.Hash32
	if blocks[0].Gloas {
		parent, err := getPayloadBlock(ctx, client, fmt.Sprintf("%#x", blocks[0].ParentRoot))
		if err != nil {
			return fmt.Errorf("error getting parent of block at slot %v: %w", blocks[0].Slot, err)
		}
		parentBlockHash = parent.BlockHash
	}

	full, payers, pending := resolveGloasPayloads(parentBlockHash, blocks)
	for slot := endSlot + 1; !gloasResolved(blocks, full, pending, endSlot); slot++ {
		if slot > maxSlot {
			return fmt.Errorf("%w: slots up to %v (finalized slot: %v)", ErrPayloadsNotFinalized, endSlot, maxSlot)
		}
		b, err := getSlotPayloadBlock(ctx, client, slot)
		if err != nil {
			return err
		}
		if b == nil {
			continue
		}
		blocks = append(blocks, b)
		full, payers, pending = resolveGloasPayloads(parentBlockHash, blocks)
	}

	type payloadTask struct {
		block       *payloadBlock
		fees        bool
		withdrawals bool
	}
	tasks := map[uint64]*payloadTask{}
	bySlot := make(map[uint64]*payloadBlock, len(blocks))
	for _, b := range blocks {
		bySlot[b.Slot] = b
	}
	for debitSlot, payerSlot := range payers {
		if debitSlot <= firstSlot || debitSlot > endSlot {
			// withdrawals debited at firstSlot are already part of the start balance
			continue
		}
		tasks[payerSlot] = &payloadTask{block: bySlot[payerSlot], withdrawals: true}
	}
	for _, b := range blocks {
		if !b.Gloas || b.Slot < firstSlot || b.Slot >= endSlot || !full[b.Slot] {
			continue
		}
		if _, exists := validatorsByIndex[b.ProposerIndex]; !exists {
			continue
		}
		if t, exists := tasks[b.Slot]; exists {
			t.fees = true
		} else {
			tasks[b.Slot] = &payloadTask{block: b, fees: true}
		}
	}

	validatorsMu := sync.Mutex{}
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for _, t := range tasks {
		t := t
		g.Go(func() error {
			if gCtx.Err() != nil {
				return gCtx.Err()
			}
			blk, err := requestElBlock(elClient, t.block.Slot, t.block.BlockHash)
			if err != nil {
				return err
			}
			var txFees *big.Int
			if t.fees && len(blk.Transactions) > 0 {
				txFees, err = priorityFeesWei(elClient, receiptsMode, t.block.Slot, blk.Transactions, uint64(blk.Number), blk.BaseFeePerGas.ToInt())
				if err != nil {
					return err
				}
			}
			validatorsMu.Lock()
			defer validatorsMu.Unlock()
			if txFees != nil {
				validatorsByIndex[t.block.ProposerIndex].TxFeesSumWei.Add(validatorsByIndex[t.block.ProposerIndex].TxFeesSumWei, txFees)
			}
			if t.withdrawals {
				for _, w := range blk.Withdrawals {
					// builder payments carry BUILDER_INDEX_FLAG and match no validator
					v, exists := validatorsByIndex[phase0.ValidatorIndex(w.ValidatorIndex)]
					if !exists {
						continue
					}
					v.WithdrawalsSumGwei += phase0.Gwei(w.Amount)
				}
			}
			if GetDebugLevel() > 1 {
				log.Printf("DEBUG eth.store: gloas payload at slot %v: block: %v, txFees: %v, withdrawals: %v\n", t.block.Slot, uint64(blk.Number), txFees, t.withdrawals)
			}
			return nil
		})
	}
	return g.Wait()
}
