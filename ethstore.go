package ethstore

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/attestantio/go-eth2-client/api/v1"
	"github.com/attestantio/go-eth2-client/http"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethTypes "github.com/ethereum/go-ethereum/core/types"
	gethRPC "github.com/ethereum/go-ethereum/rpc"
	lru "github.com/hashicorp/golang-lru"
	"github.com/prysmaticlabs/prysm/v3/beacon-chain/core/signing"
	"github.com/prysmaticlabs/prysm/v3/contracts/deposit"
	ethpb "github.com/prysmaticlabs/prysm/v3/proto/prysm/v1alpha1"
	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"
)

var debugLevel = uint64(0)
var execTimeout = time.Second * 120
var execTimeoutMu = sync.Mutex{}
var consTimeout = time.Second * 120
var consTimeoutMu = sync.Mutex{}
var validatorsCache *lru.Cache
var validatorsCacheMu = sync.Mutex{}

type Day struct {
	Day                  decimal.Decimal `json:"day"`
	DayTime              time.Time       `json:"dayTime"`
	Apr                  decimal.Decimal `json:"apr"`
	Validators           decimal.Decimal `json:"validators"`
	StartEpoch           decimal.Decimal `json:"startEpoch"`
	EffectiveBalanceGwei decimal.Decimal `json:"effectiveBalanceGwei"`
	StartBalanceGwei     decimal.Decimal `json:"startBalanceGwei"`
	EndBalanceGwei       decimal.Decimal `json:"endBalanceGwei"`
	DepositsSumGwei      decimal.Decimal `json:"depositsSumGwei"`
	WithdrawalsSumGwei   decimal.Decimal `json:"withdrawalsSumGwei"`
	ConsensusRewardsGwei decimal.Decimal `json:"consensusRewardsGwei"`
	TxFeesSumWei         decimal.Decimal `json:"txFeesSumWei"`
	TotalRewardsWei      decimal.Decimal `json:"totalRewardsWei"`
}

type Validator struct {
	Index                phase0.ValidatorIndex
	Pubkey               phase0.BLSPubKey
	EffectiveBalanceGwei phase0.Gwei
	StartBalanceGwei     phase0.Gwei
	EndBalanceGwei       phase0.Gwei
	DepositsSumGwei      phase0.Gwei
	WithdrawalsSumGwei   phase0.Gwei
	TxFeesSumWei         *big.Int
}

func SetDebugLevel(lvl uint64) {
	atomic.StoreUint64(&debugLevel, lvl)
}

func GetDebugLevel() uint64 {
	return atomic.LoadUint64(&debugLevel)
}

func SetConsTimeout(dur time.Duration) {
	consTimeoutMu.Lock()
	defer consTimeoutMu.Unlock()
	consTimeout = dur
}

func SetExecTimeout(dur time.Duration) {
	execTimeoutMu.Lock()
	defer execTimeoutMu.Unlock()
	execTimeout = dur
}

func GetConsTimeout() time.Duration {
	consTimeoutMu.Lock()
	defer consTimeoutMu.Unlock()
	return consTimeout
}

func GetExecTimeout() time.Duration {
	execTimeoutMu.Lock()
	defer execTimeoutMu.Unlock()
	return execTimeout
}

func GetFinalizedDay(ctx context.Context, address string) (uint64, error) {
	service, err := http.New(ctx, http.WithAddress(address), http.WithTimeout(GetConsTimeout()), http.WithLogLevel(zerolog.WarnLevel))
	if err != nil {
		return 0, err
	}
	client := service.(*http.Service)
	apiSpec, err := client.Spec(ctx)
	if err != nil {
		return 0, err
	}
	secondsPerSlotIf, exists := apiSpec["SECONDS_PER_SLOT"]
	if !exists {
		return 0, fmt.Errorf("undefined SECONDS_PER_SLOT in spec")
	}
	secondsPerSlotDur, ok := secondsPerSlotIf.(time.Duration)
	if !ok {
		return 0, fmt.Errorf("invalid format of SECONDS_PER_SLOT in spec")
	}
	secondsPerSlot := uint64(secondsPerSlotDur.Seconds())
	slotsPerDay := 3600 * 24 / secondsPerSlot

	h, err := client.BeaconBlockHeader(ctx, "finalized")
	if err != nil {
		return 0, err
	}

	day := uint64(h.Header.Message.Slot)/slotsPerDay - 1
	return day, nil
}

func GetHeadDay(ctx context.Context, address string) (uint64, error) {
	service, err := http.New(ctx, http.WithAddress(address), http.WithTimeout(GetConsTimeout()), http.WithLogLevel(zerolog.WarnLevel))
	if err != nil {
		return 0, err
	}
	client := service.(*http.Service)
	apiSpec, err := client.Spec(ctx)
	if err != nil {
		return 0, err
	}
	secondsPerSlotIf, exists := apiSpec["SECONDS_PER_SLOT"]
	if !exists {
		return 0, fmt.Errorf("undefined SECONDS_PER_SLOT in spec")
	}
	secondsPerSlotDur, ok := secondsPerSlotIf.(time.Duration)
	if !ok {
		return 0, fmt.Errorf("invalid format of SECONDS_PER_SLOT in spec")
	}
	secondsPerSlot := uint64(secondsPerSlotDur.Seconds())
	slotsPerDay := 3600 * 24 / secondsPerSlot

	h, err := client.BeaconBlockHeader(ctx, "finalized")
	if err != nil {
		return 0, err
	}

	day := uint64(h.Header.Message.Slot) / slotsPerDay
	return day, nil
}

func GetValidators(ctx context.Context, client *http.Service, stateID string) (map[phase0.ValidatorIndex]*v1.Validator, error) {
	validatorsCacheMu.Lock()
	defer validatorsCacheMu.Unlock()

	if validatorsCache == nil {
		c, err := lru.New(2)
		if err != nil {
			return nil, err
		}
		validatorsCache = c
	}

	key := fmt.Sprintf("%s:%s", client.Address(), stateID)
	val, found := validatorsCache.Get(key)
	if found {
		return val.(map[phase0.ValidatorIndex]*v1.Validator), nil
	}
	vals, err := client.Validators(ctx, stateID, nil)
	if err != nil {
		return nil, fmt.Errorf("error getting validators for slot %v: %w", stateID, err)
	}
	validatorsCache.Add(key, vals)
	return vals, nil
}

type BlockData struct {
	ProposerIndex phase0.ValidatorIndex
	Transactions  []bellatrix.Transaction
	BaseFeePerGas [32]byte
	Deposits      []*phase0.Deposit
	GasUsed       uint64
	GasLimit      uint64
	Withdrawals   []*capella.Withdrawal
	BlockNumber   uint64
}

func GetBlockData(block *spec.VersionedSignedBeaconBlock) (*BlockData, error) {
	d := &BlockData{}
	switch block.Version {
	case spec.DataVersionPhase0:
		d.Deposits = block.Phase0.Message.Body.Deposits
		d.ProposerIndex = block.Phase0.Message.ProposerIndex
	case spec.DataVersionAltair:
		d.Deposits = block.Altair.Message.Body.Deposits
		d.ProposerIndex = block.Altair.Message.ProposerIndex
	case spec.DataVersionBellatrix:
		d.Deposits = block.Bellatrix.Message.Body.Deposits
		d.ProposerIndex = block.Bellatrix.Message.ProposerIndex
		d.GasUsed = block.Bellatrix.Message.Body.ExecutionPayload.GasUsed
		d.GasLimit = block.Bellatrix.Message.Body.ExecutionPayload.GasLimit
		d.BaseFeePerGas = block.Bellatrix.Message.Body.ExecutionPayload.BaseFeePerGas
		d.BlockNumber = block.Bellatrix.Message.Body.ExecutionPayload.BlockNumber
		d.Transactions = block.Bellatrix.Message.Body.ExecutionPayload.Transactions
	case spec.DataVersionCapella:
		d.Deposits = block.Capella.Message.Body.Deposits
		d.ProposerIndex = block.Capella.Message.ProposerIndex
		d.GasUsed = block.Capella.Message.Body.ExecutionPayload.GasUsed
		d.GasLimit = block.Capella.Message.Body.ExecutionPayload.GasLimit
		d.BaseFeePerGas = block.Capella.Message.Body.ExecutionPayload.BaseFeePerGas
		d.Withdrawals = block.Capella.Message.Body.ExecutionPayload.Withdrawals
		d.BlockNumber = block.Capella.Message.Body.ExecutionPayload.BlockNumber
		d.Transactions = block.Capella.Message.Body.ExecutionPayload.Transactions
	default:
		return nil, fmt.Errorf("unknown block version: %v", block.Version)
	}
	return d, nil
}

func Calculate(ctx context.Context, bnAddress, elAddress, dayStr string, concurrency int) (*Day, map[uint64]*Day, error) {
	gethRpcClient, err := gethRPC.Dial(elAddress)
	if err != nil {
		return nil, nil, err
	}

	service, err := http.New(ctx, http.WithAddress(bnAddress), http.WithTimeout(GetConsTimeout()), http.WithLogLevel(zerolog.WarnLevel))
	if err != nil {
		return nil, nil, err
	}
	client := service.(*http.Service)

	apiSpec, err := client.Spec(ctx)
	if err != nil {
		return nil, nil, err
	}

	genesisForkVersionIf, exists := apiSpec["GENESIS_FORK_VERSION"]
	if !exists {
		return nil, nil, fmt.Errorf("undefined GENESIS_FORK_VERSION in spec")
	}
	genesisForkVersion, ok := genesisForkVersionIf.(phase0.Version)
	if !ok {
		return nil, nil, fmt.Errorf("invalid format of GENESIS_FORK_VERSION in spec")
	}

	domainDepositIf, exists := apiSpec["DOMAIN_DEPOSIT"]
	if !exists {
		return nil, nil, fmt.Errorf("undefined DOMAIN_DEPOSIT in spec")
	}
	domainDeposit, ok := domainDepositIf.(phase0.DomainType)
	if !ok {
		return nil, nil, fmt.Errorf("invalid format of DOMAIN_DEPOSIT in spec")
	}

	genesisValidatorsRoot := [32]byte{}
	depositDomainComputed, err := signing.ComputeDomain(domainDeposit, genesisForkVersion[:], genesisValidatorsRoot[:])
	if err != nil {
		return nil, nil, err
	}

	slotsPerEpochIf, exists := apiSpec["SLOTS_PER_EPOCH"]
	if !exists {
		return nil, nil, fmt.Errorf("undefined SLOTS_PER_EPOCH in spec")
	}
	slotsPerEpoch, ok := slotsPerEpochIf.(uint64)
	if !ok {
		return nil, nil, fmt.Errorf("invalid format of SLOTS_PER_EPOCH in spec")
	}

	secondsPerSlotIf, exists := apiSpec["SECONDS_PER_SLOT"]
	if !exists {
		return nil, nil, fmt.Errorf("undefined SECONDS_PER_SLOT in spec")
	}
	secondsPerSlotDur, ok := secondsPerSlotIf.(time.Duration)
	if !ok {
		return nil, nil, fmt.Errorf("invalid format of SECONDS_PER_SLOT in spec")
	}
	secondsPerSlot := uint64(secondsPerSlotDur.Seconds())

	slotsPerDay := 3600 * 24 / secondsPerSlot

	finalizedHeader, err := client.BeaconBlockHeader(ctx, "finalized")
	if err != nil {
		return nil, nil, err
	}
	finalizedSlot := uint64(finalizedHeader.Header.Message.Slot)
	finalizedDay := finalizedSlot/slotsPerDay - 1

	var day uint64
	if dayStr == "finalized" {
		day = finalizedDay
	} else if dayStr == "head" {
		day = finalizedSlot / slotsPerDay
	} else {
		day, err = strconv.ParseUint(dayStr, 10, 64)
		if err != nil {
			return nil, nil, err
		}
	}

	if day > finalizedDay {
		return nil, nil, fmt.Errorf("requested to calculate eth.store for a future day (last finalized day: %v, requested day: %v)", finalizedDay, day)
	}

	firstSlot := day * slotsPerDay
	endSlot := (day + 1) * slotsPerDay // first slot not included in this eth.store-day

	if endSlot > finalizedSlot {
		endSlot = finalizedSlot
	}
	lastSlot := endSlot - 1

	firstEpoch := firstSlot / slotsPerEpoch
	lastEpoch := lastSlot / slotsPerEpoch
	endEpoch := lastEpoch + 1

	genesis, err := client.GenesisTime(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting genesisTime: %w", err)
	}
	startTime := time.Unix(genesis.Unix()+int64(firstSlot)*int64(secondsPerSlot), 0)
	endTime := time.Unix(genesis.Unix()+int64(lastSlot)*int64(secondsPerSlot), 0)

	if GetDebugLevel() > 0 {
		log.Printf("DEBUG eth.store: calculating day %v (%v - %v, epochs: %v-%v, slots: %v-%v, genesis: %v, finalizedSlot: %v)\n", day, startTime, endTime, firstEpoch, lastEpoch, firstSlot, lastSlot, genesis, finalizedSlot)
	}

	validatorsByIndex := map[phase0.ValidatorIndex]*Validator{}
	validatorsByPubkey := map[phase0.BLSPubKey]*Validator{}

	startValidators, err := GetValidators(ctx, client, fmt.Sprintf("%d", firstSlot))
	if err != nil {
		return nil, nil, fmt.Errorf("error getting startValidators for firstSlot %d: %w", firstSlot, err)
	}

	for _, val := range startValidators {
		if !val.Status.IsActive() {
			continue
		}
		vv := &Validator{
			Index:                val.Index,
			Pubkey:               val.Validator.PublicKey,
			EffectiveBalanceGwei: val.Validator.EffectiveBalance,
			StartBalanceGwei:     val.Balance,
			TxFeesSumWei:         new(big.Int),
		}
		validatorsByIndex[val.Index] = vv
		validatorsByPubkey[val.Validator.PublicKey] = vv
	}

	endValidators, err := GetValidators(ctx, client, fmt.Sprintf("%d", endSlot))
	if err != nil {
		return nil, nil, fmt.Errorf("error getting endValidators for endSlot %d: %w", endSlot, err)
	}

	for _, val := range endValidators {
		v, exists := validatorsByIndex[val.Index]
		if !exists {
			continue
		}
		if uint64(val.Validator.ExitEpoch) < endEpoch {
			// do not account validators that have not been active until the end of the day
			delete(validatorsByIndex, val.Index)
			delete(validatorsByPubkey, val.Validator.PublicKey)
			continue
		}
		// set endBalance of validator to the balance of the first epoch of the next day
		v.EndBalanceGwei = val.Balance
	}
	if GetDebugLevel() > 0 {
		log.Printf("DEBUG eth.store: startValidators: %v, endValidators: %v, ethstoreValidators: %v", len(startValidators), len(endValidators), len(validatorsByIndex))
	}

	g := new(errgroup.Group)
	g.SetLimit(concurrency)
	validatorsMu := sync.Mutex{}

	// get all deposits and txs of all active validators in the slot interval [startSlot,endSlot)
	for i := firstSlot; i < endSlot; i++ {
		i := i
		if GetDebugLevel() > 0 && (endSlot-i)%1000 == 0 {
			log.Printf("DEBUG eth.store: checking blocks for deposits and txs: %.0f%% (%v of %v-%v)\n", 100*float64(i-firstSlot)/float64(endSlot-firstSlot), i, firstSlot, endSlot)
		}
		g.Go(func() error {
			var block *spec.VersionedSignedBeaconBlock
			var err error
			for j := 0; j < 10; j++ { // retry up to 10 times on failure
				block, err = client.SignedBeaconBlock(ctx, fmt.Sprintf("%d", i))

				if err == nil {
					break
				} else {
					log.Printf("error retrieving beacon block at slot %v: %v", i, err)
					time.Sleep(time.Duration(j) * time.Second)
				}
			}
			if err != nil {
				return fmt.Errorf("error getting block %v: %w", i, err)
			}
			if block == nil {
				return nil
			}
			blockData, err := GetBlockData(block)
			if err != nil {
				return fmt.Errorf("error getting blockData for block at slot %v: %w", i, err)
			}

			v, exists := validatorsByIndex[blockData.ProposerIndex]
			// only calculate for validators that have been active the whole day
			if exists && len(blockData.Transactions) > 0 {
				txHashes := []common.Hash{}
				for _, tx := range blockData.Transactions {
					var decTx gethTypes.Transaction
					err := decTx.UnmarshalBinary([]byte(tx))
					if err != nil {
						return err
					}
					txHashes = append(txHashes, decTx.Hash())
				}

				var txReceipts []*TxReceipt
				for j := 0; j < 10; j++ { // retry up to 10 times
					ctx, cancel := context.WithTimeout(context.Background(), GetExecTimeout())
					txReceipts, err = batchRequestReceipts(ctx, gethRpcClient, txHashes)
					if err == nil {
						cancel()
						break
					} else {
						log.Printf("error doing batchRequestReceipts for slot %v: %v", i, err)
						time.Sleep(time.Duration(j) * time.Second)
					}
					cancel()
				}
				if err != nil {
					return fmt.Errorf("error doing batchRequestReceipts for slot %v: %w", i, err)
				}

				totalTxFee := big.NewInt(0)
				for _, r := range txReceipts {
					if r.EffectiveGasPrice == nil {
						return fmt.Errorf("no EffectiveGasPrice for slot %v: %v", i, txHashes)
					}
					txFee := new(big.Int).Mul(r.EffectiveGasPrice.ToInt(), new(big.Int).SetUint64(uint64(r.GasUsed)))
					totalTxFee.Add(totalTxFee, txFee)
				}

				// base fee per gas is stored little-endian but we need it
				// big-endian for big.Int.
				var baseFeePerGasBEBytes [32]byte
				for i := 0; i < 32; i++ {
					baseFeePerGasBEBytes[i] = blockData.BaseFeePerGas[32-1-i]
				}
				baseFeePerGas := new(big.Int).SetBytes(baseFeePerGasBEBytes[:])
				burntFee := new(big.Int).Mul(baseFeePerGas, new(big.Int).SetUint64(blockData.GasUsed))

				totalTxFee.Sub(totalTxFee, burntFee)

				validatorsMu.Lock()
				v.TxFeesSumWei.Add(v.TxFeesSumWei, totalTxFee)
				validatorsMu.Unlock()

				if GetDebugLevel() > 1 {
					log.Printf("DEBUG eth.store: slot: %v, block: %v, baseFee: %v, txFees: %v, burnt: %v\n", i, blockData.BlockNumber, baseFeePerGas, totalTxFee, burntFee)
				}
			}

			validatorsMu.Lock()
			defer validatorsMu.Unlock()
			for _, d := range blockData.Deposits {
				v, exists := validatorsByPubkey[d.Data.PublicKey]
				if !exists {
					// only calculate for validators that have been active the whole day
					continue
				}
				msg := &ethpb.Deposit_Data{
					PublicKey:             d.Data.PublicKey[:],
					WithdrawalCredentials: d.Data.WithdrawalCredentials,
					Amount:                uint64(d.Data.Amount),
					Signature:             d.Data.Signature[:],
				}
				err := deposit.VerifyDepositSignature(msg, depositDomainComputed)
				if err != nil {
					if GetDebugLevel() > 0 {
						log.Printf("DEBUG eth.store: invalid deposit signature in block %d: %v", i, err)
					}
					continue
				}
				if GetDebugLevel() > 0 {
					log.Printf("DEBUG eth.store: extra deposit at block %d from %v: %#x: %v\n", i, v.Index, d.Data.PublicKey, d.Data.Amount)
				}
				v.DepositsSumGwei += d.Data.Amount
			}
			for _, d := range blockData.Withdrawals {
				v, exists := validatorsByIndex[d.ValidatorIndex]
				if !exists {
					// only calculate for validators that have been active the whole day
					continue
				}
				v.WithdrawalsSumGwei += d.Amount
			}

			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}

	var totalEffectiveBalanceGwei phase0.Gwei
	var totalStartBalanceGwei phase0.Gwei
	var totalEndBalanceGwei phase0.Gwei
	var totalDepositsSumGwei phase0.Gwei
	var totalWithdrawalsSumGwei phase0.Gwei
	totalTxFeesSumWei := new(big.Int)

	ethstorePerValidator := make(map[uint64]*Day, len(validatorsByIndex))

	for index, v := range validatorsByIndex {
		totalEffectiveBalanceGwei += v.EffectiveBalanceGwei
		totalStartBalanceGwei += v.StartBalanceGwei
		totalEndBalanceGwei += v.EndBalanceGwei
		totalDepositsSumGwei += v.DepositsSumGwei
		totalWithdrawalsSumGwei += v.WithdrawalsSumGwei
		totalTxFeesSumWei.Add(totalTxFeesSumWei, v.TxFeesSumWei)

		validatorConsensusRewardsGwei := decimal.NewFromInt(int64(v.EndBalanceGwei) - int64(v.StartBalanceGwei) - int64(v.DepositsSumGwei) + int64(v.WithdrawalsSumGwei))
		validatorRewardsWei := decimal.NewFromBigInt(v.TxFeesSumWei, 0).Add(validatorConsensusRewardsGwei.Mul(decimal.NewFromInt(1e9)))

		ethstorePerValidator[uint64(index)] = &Day{
			Day:                  decimal.NewFromInt(int64(day)),
			DayTime:              startTime,
			StartEpoch:           decimal.NewFromInt(int64(firstEpoch)),
			Apr:                  decimal.NewFromInt(365).Mul(validatorRewardsWei).Div(decimal.NewFromInt(int64(v.EffectiveBalanceGwei)).Mul(decimal.NewFromInt(1e9))),
			Validators:           decimal.NewFromInt(int64(len(validatorsByIndex))),
			EffectiveBalanceGwei: decimal.NewFromInt(int64(v.EffectiveBalanceGwei)),
			StartBalanceGwei:     decimal.NewFromInt(int64(v.StartBalanceGwei)),
			EndBalanceGwei:       decimal.NewFromInt(int64(v.EndBalanceGwei)),
			DepositsSumGwei:      decimal.NewFromInt(int64(v.DepositsSumGwei)),
			TxFeesSumWei:         decimal.NewFromBigInt(v.TxFeesSumWei, 0),
			ConsensusRewardsGwei: validatorConsensusRewardsGwei,
			TotalRewardsWei:      validatorRewardsWei,
			WithdrawalsSumGwei:   decimal.NewFromInt(int64(v.WithdrawalsSumGwei)),
		}
	}

	totalConsensusRewardsGwei := decimal.NewFromInt(int64(totalEndBalanceGwei) - int64(totalStartBalanceGwei) - int64(totalDepositsSumGwei) + int64(totalWithdrawalsSumGwei))
	totalRewardsWei := decimal.NewFromBigInt(totalTxFeesSumWei, 0).Add(totalConsensusRewardsGwei.Mul(decimal.NewFromInt(1e9)))

	ethstoreDay := &Day{
		Day:                  decimal.NewFromInt(int64(day)),
		DayTime:              startTime,
		StartEpoch:           decimal.NewFromInt(int64(firstEpoch)),
		Apr:                  decimal.NewFromInt(365).Mul(totalRewardsWei).Div(decimal.NewFromInt(int64(totalEffectiveBalanceGwei)).Mul(decimal.NewFromInt(1e9))),
		Validators:           decimal.NewFromInt(int64(len(validatorsByIndex))),
		EffectiveBalanceGwei: decimal.NewFromInt(int64(totalEffectiveBalanceGwei)),
		StartBalanceGwei:     decimal.NewFromInt(int64(totalStartBalanceGwei)),
		EndBalanceGwei:       decimal.NewFromInt(int64(totalEndBalanceGwei)),
		DepositsSumGwei:      decimal.NewFromInt(int64(totalDepositsSumGwei)),
		TxFeesSumWei:         decimal.NewFromBigInt(totalTxFeesSumWei, 0),
		ConsensusRewardsGwei: totalConsensusRewardsGwei,
		WithdrawalsSumGwei:   decimal.NewFromInt(int64(totalWithdrawalsSumGwei)),
		TotalRewardsWei:      totalRewardsWei,
	}

	if GetDebugLevel() > 0 {
		log.Printf("DEBUG eth.store: %+v\n", ethstoreDay)
	}

	return ethstoreDay, ethstorePerValidator, nil
}

func batchRequestReceipts(ctx context.Context, elClient *gethRPC.Client, txHashes []common.Hash) ([]*TxReceipt, error) {
	elems := make([]gethRPC.BatchElem, 0, len(txHashes))
	errors := make([]error, 0, len(txHashes))
	txReceipts := make([]*TxReceipt, len(txHashes))
	for i, h := range txHashes {
		txReceipt := &TxReceipt{}
		err := error(nil)
		elems = append(elems, gethRPC.BatchElem{
			Method: "eth_getTransactionReceipt",
			Args:   []interface{}{h.Hex()},
			Result: txReceipt,
			Error:  err,
		})
		txReceipts[i] = txReceipt
		errors = append(errors, err)
	}
	ioErr := elClient.BatchCallContext(ctx, elems)
	if ioErr != nil {
		return nil, fmt.Errorf("io-error when fetching tx-receipts: %w", ioErr)
	}
	for _, e := range errors {
		if e != nil {
			return nil, fmt.Errorf("error when fetching tx-receipts: %w", e)
		}
	}
	return txReceipts, nil
}

type TxReceipt struct {
	BlockHash         *common.Hash    `json:"blockHash"`
	BlockNumber       *hexutil.Big    `json:"blockNumber"`
	ContractAddress   *common.Address `json:"contractAddress,omitempty"`
	CumulativeGasUsed hexutil.Uint64  `json:"cumulativeGasUsed"`
	EffectiveGasPrice *hexutil.Big    `json:"effectiveGasPrice"`
	From              *common.Address `json:"from,omitempty"`
	GasUsed           hexutil.Uint64  `json:"gasUsed"`
	LogsBloom         hexutil.Bytes   `json:"logsBloom"`
	Status            hexutil.Uint64  `json:"status"`
	To                *common.Address `json:"to,omitempty"`
	TransactionHash   *common.Hash    `json:"transactionHash"`
	TransactionIndex  hexutil.Uint64  `json:"transactionIndex"`
	Type              hexutil.Uint64  `json:"type"`
}
