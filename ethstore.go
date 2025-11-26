package ethstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/attestantio/go-eth2-client/api"
	v1 "github.com/attestantio/go-eth2-client/api/v1"
	ethHttp "github.com/attestantio/go-eth2-client/http"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethTypes "github.com/ethereum/go-ethereum/core/types"
	gethRPC "github.com/ethereum/go-ethereum/rpc"
	lru "github.com/hashicorp/golang-lru"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/signing"
	"github.com/prysmaticlabs/prysm/v5/contracts/deposit"
	ethpb "github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1"
	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"
)

const RECEIPTS_MODE_BATCH = 0
const RECEIPTS_MODE_SINGLE = 1

var debugLevel = uint64(0)
var beaconchainApiClient = NewBeaconchainApiClient()
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

func SetDomain(domain string) {
	beaconchainApiClient.SetDomain(domain)
}

func GetDomain() string {
	return beaconchainApiClient.GetDomain()
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

func SetBeaconchainApiKey(apiKey string) {
	beaconchainApiClient.SetApiKey(apiKey)
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
	service, err := ethHttp.New(ctx, ethHttp.WithAddress(address), ethHttp.WithTimeout(GetConsTimeout()), ethHttp.WithLogLevel(zerolog.WarnLevel))
	if err != nil {
		return 0, err
	}
	client := service.(*ethHttp.Service)
	apiSpec, err := client.Spec(ctx, &api.SpecOpts{})
	if err != nil {
		return 0, fmt.Errorf("error calling client.Spec: %w", err)
	}
	secondsPerSlotIf, exists := apiSpec.Data["SECONDS_PER_SLOT"]
	if !exists {
		return 0, fmt.Errorf("undefined SECONDS_PER_SLOT in spec")
	}
	secondsPerSlotDur, ok := secondsPerSlotIf.(time.Duration)
	if !ok {
		return 0, fmt.Errorf("invalid format of SECONDS_PER_SLOT in spec")
	}
	secondsPerSlot := uint64(secondsPerSlotDur.Seconds())
	slotsPerDay := 3600 * 24 / secondsPerSlot

	h, err := client.BeaconBlockHeader(ctx, &api.BeaconBlockHeaderOpts{Block: "finalized"})
	if err != nil {
		return 0, fmt.Errorf("error calling client.BeaconBlockHeader: %w", err)
	}

	day := uint64(h.Data.Header.Message.Slot)/slotsPerDay - 1
	return day, nil
}

func GetHeadDay(ctx context.Context, address string) (uint64, error) {
	service, err := ethHttp.New(ctx, ethHttp.WithAddress(address), ethHttp.WithTimeout(GetConsTimeout()), ethHttp.WithLogLevel(zerolog.WarnLevel))
	if err != nil {
		return 0, err
	}
	client := service.(*ethHttp.Service)
	apiSpec, err := client.Spec(ctx, &api.SpecOpts{})
	if err != nil {
		return 0, fmt.Errorf("error calling client.Spec: %w", err)
	}
	secondsPerSlotIf, exists := apiSpec.Data["SECONDS_PER_SLOT"]
	if !exists {
		return 0, fmt.Errorf("undefined SECONDS_PER_SLOT in spec")
	}
	secondsPerSlotDur, ok := secondsPerSlotIf.(time.Duration)
	if !ok {
		return 0, fmt.Errorf("invalid format of SECONDS_PER_SLOT in spec")
	}
	secondsPerSlot := uint64(secondsPerSlotDur.Seconds())
	slotsPerDay := 3600 * 24 / secondsPerSlot

	h, err := client.BeaconBlockHeader(ctx, &api.BeaconBlockHeaderOpts{Block: "head"})
	if err != nil {
		return 0, fmt.Errorf("error calling client.BeaconBlockHeader: %w", err)
	}

	day := uint64(h.Data.Header.Message.Slot) / slotsPerDay
	return day, nil
}

func GetValidators(ctx context.Context, client *ethHttp.Service, stateID string) (map[phase0.ValidatorIndex]*v1.Validator, error) {
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
	vals, err := client.Validators(ctx, &api.ValidatorsOpts{State: stateID})
	if err != nil {
		return nil, fmt.Errorf("error getting validators for slot %v: %w", stateID, err)
	}
	validatorsCache.Add(key, vals.Data)
	return vals.Data, nil
}

type BlockData struct {
	Version       spec.DataVersion
	ProposerIndex phase0.ValidatorIndex
	Transactions  []bellatrix.Transaction
	BaseFeePerGas *big.Int
	Deposits      []*phase0.Deposit
	GasUsed       uint64
	GasLimit      uint64
	Withdrawals   []*capella.Withdrawal
	BlockNumber   uint64
}

func GetBlockData(block *spec.VersionedSignedBeaconBlock) (*BlockData, error) {
	d := &BlockData{}
	d.Version = block.Version
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
		baseFeePerGasBEBytes := make([]byte, len(block.Bellatrix.Message.Body.ExecutionPayload.BaseFeePerGas))
		for i := 0; i < 32; i++ {
			baseFeePerGasBEBytes[i] = block.Bellatrix.Message.Body.ExecutionPayload.BaseFeePerGas[32-1-i]
		}
		d.BaseFeePerGas = new(big.Int).SetBytes(baseFeePerGasBEBytes)
		d.BlockNumber = block.Bellatrix.Message.Body.ExecutionPayload.BlockNumber
		d.Transactions = block.Bellatrix.Message.Body.ExecutionPayload.Transactions
	case spec.DataVersionCapella:
		d.Deposits = block.Capella.Message.Body.Deposits
		d.ProposerIndex = block.Capella.Message.ProposerIndex
		d.GasUsed = block.Capella.Message.Body.ExecutionPayload.GasUsed
		d.GasLimit = block.Capella.Message.Body.ExecutionPayload.GasLimit
		baseFeePerGasBEBytes := make([]byte, len(block.Capella.Message.Body.ExecutionPayload.BaseFeePerGas))
		for i := 0; i < 32; i++ {
			baseFeePerGasBEBytes[i] = block.Capella.Message.Body.ExecutionPayload.BaseFeePerGas[32-1-i]
		}
		d.BaseFeePerGas = new(big.Int).SetBytes(baseFeePerGasBEBytes)
		d.Withdrawals = block.Capella.Message.Body.ExecutionPayload.Withdrawals
		d.BlockNumber = block.Capella.Message.Body.ExecutionPayload.BlockNumber
		d.Transactions = block.Capella.Message.Body.ExecutionPayload.Transactions
	case spec.DataVersionDeneb:
		d.Deposits = block.Deneb.Message.Body.Deposits
		d.ProposerIndex = block.Deneb.Message.ProposerIndex
		d.GasUsed = block.Deneb.Message.Body.ExecutionPayload.GasUsed
		d.GasLimit = block.Deneb.Message.Body.ExecutionPayload.GasLimit
		d.BaseFeePerGas = block.Deneb.Message.Body.ExecutionPayload.BaseFeePerGas.ToBig()
		d.Withdrawals = block.Deneb.Message.Body.ExecutionPayload.Withdrawals
		d.BlockNumber = block.Deneb.Message.Body.ExecutionPayload.BlockNumber
		d.Transactions = block.Deneb.Message.Body.ExecutionPayload.Transactions
	case spec.DataVersionElectra:
		d.Deposits = block.Electra.Message.Body.Deposits
		d.ProposerIndex = block.Electra.Message.ProposerIndex
		d.GasUsed = block.Electra.Message.Body.ExecutionPayload.GasUsed
		d.GasLimit = block.Electra.Message.Body.ExecutionPayload.GasLimit
		d.BaseFeePerGas = block.Electra.Message.Body.ExecutionPayload.BaseFeePerGas.ToBig()
		d.Withdrawals = block.Electra.Message.Body.ExecutionPayload.Withdrawals
		d.BlockNumber = block.Electra.Message.Body.ExecutionPayload.BlockNumber
		d.Transactions = block.Electra.Message.Body.ExecutionPayload.Transactions
	case spec.DataVersionFulu:
		d.Deposits = block.Fulu.Message.Body.Deposits
		d.ProposerIndex = block.Fulu.Message.ProposerIndex
		d.GasUsed = block.Fulu.Message.Body.ExecutionPayload.GasUsed
		d.GasLimit = block.Fulu.Message.Body.ExecutionPayload.GasLimit
		d.BaseFeePerGas = block.Fulu.Message.Body.ExecutionPayload.BaseFeePerGas.ToBig()
		d.Withdrawals = block.Fulu.Message.Body.ExecutionPayload.Withdrawals
		d.BlockNumber = block.Fulu.Message.Body.ExecutionPayload.BlockNumber
		d.Transactions = block.Fulu.Message.Body.ExecutionPayload.Transactions
	default:
		return nil, fmt.Errorf("unknown block version: %v", block.Version)
	}
	return d, nil
}

func Calculate(ctx context.Context, bnAddress, elAddress, dayStr string, concurrency int, receiptsMode int) (*Day, map[uint64]*Day, error) {
	gethRpcClient, err := gethRPC.Dial(elAddress)
	if err != nil {
		return nil, nil, err
	}

	service, err := ethHttp.New(ctx, ethHttp.WithAddress(bnAddress), ethHttp.WithTimeout(GetConsTimeout()), ethHttp.WithLogLevel(zerolog.WarnLevel))
	if err != nil {
		return nil, nil, err
	}
	client := service.(*ethHttp.Service)

	apiSpec, err := client.Spec(ctx, &api.SpecOpts{})
	if err != nil {
		return nil, nil, fmt.Errorf("error getting spec: %w", err)
	}

	genesisForkVersionIf, exists := apiSpec.Data["GENESIS_FORK_VERSION"]
	if !exists {
		return nil, nil, fmt.Errorf("undefined GENESIS_FORK_VERSION in spec")
	}
	genesisForkVersion, ok := genesisForkVersionIf.(phase0.Version)
	if !ok {
		return nil, nil, fmt.Errorf("invalid format of GENESIS_FORK_VERSION in spec")
	}

	beaconchainApiNetworkName := "mainnet"
	switch fmt.Sprintf("%#x", genesisForkVersion) {
	case "0x00000000":
		beaconchainApiNetworkName = "mainnet"
	case "0x00000064":
		beaconchainApiNetworkName = "gnosis"
	case "0x10000910":
		beaconchainApiNetworkName = "hoodi"
	default:
		return nil, nil, fmt.Errorf("unsupported GENESIS_FORK_VERSION: %s (only mainnet, gnosis, hoodi)", fmt.Sprintf("%#x", genesisForkVersion))
	}

	electraForkEpoch := uint64(math.MaxUint64)
	electraForkEpochStr, exists := apiSpec.Data["ELECTRA_FORK_EPOCH"]
	if exists {
		var ok bool
		electraForkEpoch, ok = electraForkEpochStr.(uint64)
		if !ok {
			return nil, nil, fmt.Errorf("invalid format of ELECTRA_FORK_EPOCH in spec")
		}
	}

	domainDepositIf, exists := apiSpec.Data["DOMAIN_DEPOSIT"]
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

	slotsPerEpochIf, exists := apiSpec.Data["SLOTS_PER_EPOCH"]
	if !exists {
		return nil, nil, fmt.Errorf("undefined SLOTS_PER_EPOCH in spec")
	}
	slotsPerEpoch, ok := slotsPerEpochIf.(uint64)
	if !ok {
		return nil, nil, fmt.Errorf("invalid format of SLOTS_PER_EPOCH in spec")
	}

	secondsPerSlotIf, exists := apiSpec.Data["SECONDS_PER_SLOT"]
	if !exists {
		return nil, nil, fmt.Errorf("undefined SECONDS_PER_SLOT in spec")
	}
	secondsPerSlotDur, ok := secondsPerSlotIf.(time.Duration)
	if !ok {
		return nil, nil, fmt.Errorf("invalid format of SECONDS_PER_SLOT in spec")
	}
	secondsPerSlot := uint64(secondsPerSlotDur.Seconds())

	slotsPerDay := 3600 * 24 / secondsPerSlot

	//finalizedHeader, err := client.BeaconBlockHeader(ctx, "finalized")
	finalizedHeader, err := client.BeaconBlockHeader(ctx, &api.BeaconBlockHeaderOpts{Block: "finalized"})
	if err != nil {
		return nil, nil, err
	}
	finalizedSlot := uint64(finalizedHeader.Data.Header.Message.Slot)
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
		log.Printf("DEBUG eth.store: calculating day %v (%v - %v, epochs: %v-%v, slots: %v-%v, genesis: %v, finalizedSlot: %v, beaconchainApiNetworkName: %s, genesisForkVersion: %#x)\n", day, startTime, endTime, firstEpoch, lastEpoch, firstSlot, lastSlot, genesis, finalizedSlot, beaconchainApiNetworkName, genesisForkVersion)
	}

	validatorsByIndex := map[phase0.ValidatorIndex]*Validator{}
	validatorsByPubkey := map[string]*Validator{}
	validatorsMu := sync.Mutex{}

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
		validatorsByPubkey[val.Validator.PublicKey.String()] = vv
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
			delete(validatorsByPubkey, val.Validator.PublicKey.String())
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

	if uint64(electraForkEpoch) <= lastEpoch {
		if GetDebugLevel() > 0 {
			log.Printf("DEBUG eth.store: fetching deposits and consolidation requests (electraForkEpoch: %v)\n", electraForkEpoch)
		}
		g.Go(func() error {
			beaconchainApiGroup := new(errgroup.Group)
			beaconchainApiGroup.SetLimit(10)

			for i := firstEpoch; i <= lastEpoch; i++ {
				i := i
				if i == 0 {
					continue
				}
				beaconchainApiGroup.Go(func() error {
					var err error
					depositRequests, err := beaconchainApiClient.DepositRequests(ctx, beaconchainApiNetworkName, i*slotsPerEpoch-1)
					if err != nil {
						return fmt.Errorf("error getting depositRequests for epoch %v (slot %v): %w", i, i*slotsPerEpoch-1, err)
					}
					if GetDebugLevel() > 1 {
						for _, d := range depositRequests.Data {
							log.Printf("DEBUG eth.store: depositRequest for epoch %v (slot %v): %v\n", i, i*slotsPerEpoch-1, d)
						}
					}
					validatorsMu.Lock()
					defer validatorsMu.Unlock()
					for _, d := range depositRequests.Data {
						v, exists := validatorsByPubkey[d.Pubkey]
						if exists {
							// only calculate for validators that have been active the whole day
							v.DepositsSumGwei += phase0.Gwei(d.Amount)
						}
					}
					return nil
				})
				beaconchainApiGroup.Go(func() error {
					var err error
					consolidationRequests, err := beaconchainApiClient.ConsolidationRequests(ctx, beaconchainApiNetworkName, i*slotsPerEpoch-1)
					if err != nil {
						return fmt.Errorf("error getting consolidationRequests for epoch %v (slot %v): %w", i, i*slotsPerEpoch-1, err)
					}
					if GetDebugLevel() > 1 {
						for _, d := range consolidationRequests.Data {
							log.Printf("DEBUG eth.store: consolidationRequest for epoch %v (slot %v): %v\n", i, i*slotsPerEpoch-1, d)
						}
					}
					validatorsMu.Lock()
					defer validatorsMu.Unlock()
					for _, d := range consolidationRequests.Data {
						vSource, exists := validatorsByIndex[phase0.ValidatorIndex(d.SourceIndex)]
						if exists {
							// only calculate for validators that have been active the whole day
							vSource.WithdrawalsSumGwei += phase0.Gwei(d.AmountConsolidated)
						}
						vTarget, exists := validatorsByIndex[phase0.ValidatorIndex(d.TargetIndex)]
						if exists {
							// only calculate for validators that have been active the whole day
							vTarget.DepositsSumGwei += phase0.Gwei(d.AmountConsolidated)
						}
					}
					return nil
				})
			}

			if err := beaconchainApiGroup.Wait(); err != nil {
				return err
			}
			return nil
		})
	}

	// get all deposits and txs of all active validators in the slot interval [startSlot,endSlot)
	for i := firstSlot; i < endSlot; i++ {
		i := i
		if GetDebugLevel() > 0 && (endSlot-i)%1000 == 0 {
			log.Printf("DEBUG eth.store: checking blocks for deposits and txs: %.0f%% (%v of %v-%v)\n", 100*float64(i-firstSlot)/float64(endSlot-firstSlot), i, firstSlot, endSlot)
		}
		g.Go(func() error {
			var blockData *BlockData

			var blockRes *api.Response[*spec.VersionedSignedBeaconBlock]
			var block *spec.VersionedSignedBeaconBlock
			var err error
			for j := 0; j < 10; j++ { // retry up to 10 times on failure
				blockRes, err = client.SignedBeaconBlock(ctx, &api.SignedBeaconBlockOpts{Block: fmt.Sprintf("%d", i)})
				if err == nil {
					break
				} else {
					var apiErr *api.Error
					if errors.As(err, &apiErr) {
						switch apiErr.StatusCode {
						case 404:
							// block not found
							return nil
						default:
							log.Printf("error retrieving beacon block at slot %v: %v", i, err)
							time.Sleep(time.Duration(j) * time.Second)
						}
					}
				}
			}
			if err != nil {
				return fmt.Errorf("error getting block %v: %w", i, err)
			}
			block = blockRes.Data
			if block == nil {
				return nil
			}
			blockData, err = GetBlockData(block)
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

					if receiptsMode == RECEIPTS_MODE_BATCH {
						txReceipts, err = batchRequestReceipts(ctx, gethRpcClient, txHashes)
						if err == nil {
							cancel()
							break
						} else {
							log.Printf("error doing batchRequestReceipts for slot %v: %v", i, err)
							time.Sleep(time.Duration(j) * time.Second)
						}
					} else if receiptsMode == RECEIPTS_MODE_SINGLE {
						txReceipts, err = requestReceipts(ctx, gethRpcClient, blockData.BlockNumber)
						if err == nil {
							cancel()
							break
						} else {
							log.Printf("error doing requestReceipts for slot %v: %v", i, err)
							time.Sleep(time.Duration(j) * time.Second)
						}
					}
					cancel()
				}
				if err != nil {
					return fmt.Errorf("error doing batchRequestReceipts for slot %v: %w", i, err)
				}

				totalTxFee := big.NewInt(0)
				for _, r := range txReceipts {
					if r.EffectiveGasPrice == nil {
						return fmt.Errorf("no EffectiveGasPrice for slot %v: %+v", i, *r)
					}
					txFee := new(big.Int).Mul(r.EffectiveGasPrice.ToInt(), new(big.Int).SetUint64(uint64(r.GasUsed)))
					totalTxFee.Add(totalTxFee, txFee)
				}

				baseFeePerGas := blockData.BaseFeePerGas
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
				v, exists := validatorsByPubkey[d.Data.PublicKey.String()]
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

		if validatorRewardsWei.LessThan(decimal.Zero) {
			if GetDebugLevel() > 1 {
				log.Printf("DEBUG eth.store: negative rewards for validator %v: %v -- %+v\n", v.Index, validatorRewardsWei, *v)
			}
		}

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

func requestReceipts(ctx context.Context, elClient *gethRPC.Client, blockNumber uint64) ([]*TxReceipt, error) {
	txReceipts := make([]*TxReceipt, 0)
	blockNumberHex := fmt.Sprintf("0x%x", blockNumber)
	ioErr := elClient.CallContext(ctx, &txReceipts, "eth_getBlockReceipts", blockNumberHex)
	if ioErr != nil {
		return nil, fmt.Errorf("io-error when fetching tx-receipts: %w", ioErr)
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

type BeaconchainApiClient struct {
	apikey      string
	apikeyMu    sync.Mutex
	domain      string
	domainMu    sync.Mutex
	ratelimiter *Ratelimiter
}

func NewBeaconchainApiClient() *BeaconchainApiClient {
	c := &BeaconchainApiClient{
		apikey:      "",
		domain:      "beaconcha.in",
		ratelimiter: NewRatelimiter(1),
	}
	return c
}

func (c *BeaconchainApiClient) SetDomain(domain string) {
	c.domainMu.Lock()
	defer c.domainMu.Unlock()
	c.domain = domain
}

func (c *BeaconchainApiClient) GetDomain() string {
	c.domainMu.Lock()
	defer c.domainMu.Unlock()
	return c.domain
}

func (c *BeaconchainApiClient) SetApiKey(apiKey string) {
	c.apikeyMu.Lock()
	defer c.apikeyMu.Unlock()
	c.apikey = apiKey
}

func (c *BeaconchainApiClient) GetApiKey() string {
	c.apikeyMu.Lock()
	defer c.apikeyMu.Unlock()
	return c.apikey
}

func (c *BeaconchainApiClient) HttpReq(ctx context.Context, method, url string, headers map[string]string, params, result interface{}) error {
	c.ratelimiter.Wait()
	if headers == nil {
		headers = make(map[string]string)
	}
	headers["apikey"] = c.GetApiKey()

	var err error
	var req *http.Request
	if params != nil {
		paramsJSON, err := json.Marshal(params)
		if err != nil {
			return HttpReqError{Url: url, HTTPError: fmt.Errorf("error marhsaling params for request: %w", err)}
		}
		req, err = http.NewRequestWithContext(ctx, method, url, bytes.NewBuffer(paramsJSON))
		if err != nil {
			return HttpReqError{Url: url, HTTPError: fmt.Errorf("error creating request with params: %w", err)}
		}
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return HttpReqError{Url: url, HTTPError: fmt.Errorf("error creating request: %w", err)}
		}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	httpClient := &http.Client{Timeout: time.Minute}
	res, err := httpClient.Do(req)
	if err != nil {
		return HttpReqError{StatusCode: res.StatusCode, Url: url, HTTPError: err}
	}

	rlStr := res.Header.Get("ratelimit-limit")
	if rlStr != "" {
		rl, err := strconv.ParseInt(rlStr, 10, 64)
		if err == nil && rl > 2 {
			rlFloat := float64(rl) * .9 // avoid hitting the ratelimit
			if rlFloat != c.ratelimiter.GetRate() {
				c.ratelimiter.SetRate(rlFloat)
				if GetDebugLevel() > 0 {
					log.Printf("DEBUG eth.store: setting beaconchain-ratelimit: %v\n", rlFloat)
				}
			}
		}
	}

	if res.StatusCode == 429 {
		log.Printf("DEBUG eth.store: status: 429, limit: %v, remaining: %v, reset: %v, window: %v, remaining-day: %v, remaining-hour: %v, remaining-minute: %v, remaining-month: %v, remaining-second: %v\n", res.Header.Get("ratelimit-limit"), res.Header.Get("ratelimit-remaining"), res.Header.Get("ratelimit-reset"), res.Header.Get("ratelimit-window"), res.Header.Get("x-ratelimit-remaining-day"), res.Header.Get("x-ratelimit-remaining-hour"), res.Header.Get("x-ratelimit-remaining-minute"), res.Header.Get("x-ratelimit-remaining-month"), res.Header.Get("x-ratelimit-remaining-second"))
	}

	defer res.Body.Close()
	if res.StatusCode > 299 {
		body, _ := io.ReadAll(res.Body)
		return HttpReqError{StatusCode: res.StatusCode, Url: url, Body: body}
	}
	if result != nil {
		err = json.NewDecoder(res.Body).Decode(result)
		if err != nil {
			return HttpReqError{StatusCode: res.StatusCode, Url: url, JSONError: err}
		}
	}
	return nil
}

func (c *BeaconchainApiClient) ConsolidationRequests(ctx context.Context, network string, slot uint64) (*BeaconchainConsolidationRequestsResponse, error) {
	res := &BeaconchainConsolidationRequestsResponse{}
	err := c.HttpReq(ctx, http.MethodGet, fmt.Sprintf("https://%s.%s/api/v1/slot/%d/consolidation_requests", network, c.GetDomain(), slot), nil, nil, res)
	return res, err
}

func (c *BeaconchainApiClient) DepositRequests(ctx context.Context, network string, slot uint64) (*BeaconchainDepositRequestsResponse, error) {
	res := &BeaconchainDepositRequestsResponse{}
	err := c.HttpReq(ctx, http.MethodGet, fmt.Sprintf("https://%s.%s/api/v1/slot/%d/deposit_requests", network, c.GetDomain(), slot), nil, nil, res)
	return res, err
}

type BeaconchainDepositRequestsResponse struct {
	Status string                      `json:"status"`
	Data   []BeaconchainDepositRequest `json:"data"`
}

type BeaconchainDepositRequest struct {
	Amount                int64  `json:"amount"`
	BlockRoot             string `json:"block_root"`
	BlockSlot             uint64 `json:"block_slot"`
	Pubkey                string `json:"pubkey"`
	RequestIndex          uint64 `json:"request_index"`
	Signature             string `json:"signature"`
	WithdrawalCredentials string `json:"withdrawal_credentials"`
}

type BeaconchainConsolidationRequestsResponse struct {
	Status string                            `json:"status"`
	Data   []BeaconchainConsolidationRequest `json:"data"`
}

type BeaconchainConsolidationRequest struct {
	AmountConsolidated int64  `json:"amount_consolidated"`
	BlockRoot          string `json:"block_root"`
	BlockSlot          uint64 `json:"block_slot"`
	RequestIndex       uint64 `json:"request_index"`
	SourceIndex        uint64 `json:"source_index"`
	TargetIndex        uint64 `json:"target_index"`
}

type HttpReqError struct {
	StatusCode int
	Url        string
	Body       []byte
	JSONError  error
	HTTPError  error
}

func (e HttpReqError) Error() string {
	var bs string
	if len(e.Body) > 1024 {
		bs = string(e.Body[:1024]) + "…"
	} else {
		bs = string(e.Body)
	}
	return fmt.Sprintf("error: statusCode: %v, url: %v, jsonError: %v, httpErro: %v, body: %v", e.StatusCode, e.Url, e.JSONError, e.HTTPError, bs)
}

type Ratelimiter struct {
	reqChan     chan int
	ticker      *time.Ticker
	reqPerSec   float64
	reqPerSecMu sync.RWMutex
}

func NewRatelimiter(reqPerSec float64) *Ratelimiter {
	rl := &Ratelimiter{}
	rl.reqPerSecMu = sync.RWMutex{}
	rl.reqChan = make(chan int, 1)
	rl.ticker = time.NewTicker(1e9 * time.Second / time.Duration(1e9*reqPerSec))
	rl.SetRate(reqPerSec)
	go func() {
		for range rl.ticker.C {
			select {
			case <-rl.reqChan:
			default:
			}
		}
	}()
	return rl
}

func (rl *Ratelimiter) Wait() {
	rl.reqChan <- 1
}

func (rl *Ratelimiter) GetRate() float64 {
	rl.reqPerSecMu.RLock()
	defer rl.reqPerSecMu.RUnlock()
	return rl.reqPerSec
}

func (rl *Ratelimiter) SetRate(reqPerSec float64) {
	rl.reqPerSecMu.Lock()
	defer rl.reqPerSecMu.Unlock()
	if reqPerSec < 0 {
		return
	}
	if rl.reqPerSec == reqPerSec {
		return
	}
	rl.reqPerSec = reqPerSec
	rl.ticker.Reset(1e9 * time.Second / time.Duration(1e9*rl.reqPerSec))
}
