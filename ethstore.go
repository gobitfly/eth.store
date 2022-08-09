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

	"github.com/attestantio/go-eth2-client/http"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	gethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"
)

var debugLevel = uint64(0)
var timeout = time.Second * 120
var timeoutMu = sync.Mutex{}

type Day struct {
	Day                  uint64          `json:"day"`
	DayTime              time.Time       `json:"dayTime"`
	Apr                  decimal.Decimal `json:"apr"`
	Validators           uint64          `json:"validators"`
	StartEpoch           phase0.Epoch    `json:"startEpoch"`
	EffectiveBalanceGwei decimal.Decimal `json:"effectiveBalanceGwei"`
	StartBalanceGwei     decimal.Decimal `json:"startBalanceGwei"`
	EndBalanceGwei       decimal.Decimal `json:"endBalanceGwei"`
	DepositsSumGwei      decimal.Decimal `json:"depositsSumGwei"`
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
	TxFeesSumWei         *big.Int
}

func SetDebugLevel(lvl uint64) {
	atomic.StoreUint64(&debugLevel, lvl)
}

func GetDebugLevel() uint64 {
	return atomic.LoadUint64(&debugLevel)
}

func SetApiTimeout(dur time.Duration) {
	timeoutMu.Lock()
	defer timeoutMu.Unlock()
	timeout = dur
}

func GetApiTimeout() time.Duration {
	timeoutMu.Lock()
	defer timeoutMu.Unlock()
	return timeout
}

func GetLatestDay(ctx context.Context, address string) (uint64, error) {
	service, err := http.New(ctx, http.WithAddress(address), http.WithTimeout(GetApiTimeout()), http.WithLogLevel(zerolog.WarnLevel))
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

	bbh, err := client.BeaconBlockHeader(ctx, "finalized")
	if err != nil {
		return 0, err
	}

	day := uint64(bbh.Header.Message.Slot)/slotsPerDay - 1
	return day, nil
}

func Calculate(ctx context.Context, address string, dayStr string) (*Day, error) {
	service, err := http.New(ctx, http.WithAddress(address), http.WithTimeout(GetApiTimeout()), http.WithLogLevel(zerolog.WarnLevel))
	if err != nil {
		return nil, err
	}
	client := service.(*http.Service)

	apiSpec, err := client.Spec(ctx)
	if err != nil {
		return nil, err
	}

	slotsPerEpochIf, exists := apiSpec["SLOTS_PER_EPOCH"]
	if !exists {
		return nil, fmt.Errorf("undefined SLOTS_PER_EPOCH in spec")
	}
	slotsPerEpoch, ok := slotsPerEpochIf.(uint64)
	if !ok {
		return nil, fmt.Errorf("invalid format of SLOTS_PER_EPOCH in spec")
	}

	secondsPerSlotIf, exists := apiSpec["SECONDS_PER_SLOT"]
	if !exists {
		return nil, fmt.Errorf("undefined SECONDS_PER_SLOT in spec")
	}
	secondsPerSlotDur, ok := secondsPerSlotIf.(time.Duration)
	if !ok {
		return nil, fmt.Errorf("invalid format of SECONDS_PER_SLOT in spec")
	}
	secondsPerSlot := uint64(secondsPerSlotDur.Seconds())

	slotsPerDay := 3600 * 24 / secondsPerSlot
	var day uint64
	if dayStr == "finalized" || dayStr == "latest" {
		bbh, err := client.BeaconBlockHeader(ctx, "finalized")
		if err != nil {
			return nil, err
		}
		day = uint64(bbh.Header.Message.Slot)/slotsPerDay - 1
	} else {
		day, err = strconv.ParseUint(dayStr, 10, 64)
		if err != nil {
			return nil, err
		}
	}
	firstSlotOfCurrentDay := day * slotsPerDay
	firstSlotOfNextDay := (day + 1) * slotsPerDay
	firstEpochOfCurrentDay := firstSlotOfCurrentDay / slotsPerEpoch
	firstEpochOfNextDay := firstSlotOfNextDay / slotsPerEpoch

	genesis, err := client.GenesisTime(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting genesisTime: %w", err)
	}
	dayTime := time.Unix(genesis.Unix()+int64(firstSlotOfCurrentDay)*int64(secondsPerSlot), 0)

	if GetDebugLevel() > 0 {
		log.Printf("DEBUG eth.store: calculating day %v (%v, firstEpochOfCurrentDay: %v, firstSlotOfCurrentDay: %v, firstSlotOfNextDay: %v)\n", day, dayTime, firstEpochOfCurrentDay, firstSlotOfCurrentDay, firstSlotOfNextDay)
	}

	validatorsByIndex := map[phase0.ValidatorIndex]*Validator{}
	validatorsByPubkey := map[phase0.BLSPubKey]*Validator{}

	startValidators, err := client.Validators(ctx, fmt.Sprintf("%d", firstSlotOfCurrentDay), nil)
	if err != nil {
		return nil, fmt.Errorf("error getting startValidators for firstSlotOfCurrentDay %d: %w", firstSlotOfCurrentDay, err)
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

	endValidators, err := client.Validators(ctx, fmt.Sprintf("%d", firstSlotOfNextDay), nil)
	if err != nil {
		return nil, fmt.Errorf("error getting endValidators for firstSlotOfNextDay %d: %w", firstSlotOfCurrentDay, err)
	}
	for _, val := range endValidators {
		v, exists := validatorsByIndex[val.Index]
		if !exists {
			continue
		}
		if uint64(val.Validator.ExitEpoch) < firstEpochOfNextDay {
			// do not account validators that have not been active until the end of the day
			delete(validatorsByIndex, val.Index)
			delete(validatorsByPubkey, val.Validator.PublicKey)
			continue
		}
		// set endBalance of validator to the balance of the first epoch of the next day
		v.EndBalanceGwei = val.Balance
	}

	g := new(errgroup.Group)
	g.SetLimit(10)
	validatorsMu := sync.Mutex{}

	// get all deposits and txs of all active validators in the slot interval [firstSlotOfCurrentDay,firstSlotOfNextDay)
	for i := firstSlotOfCurrentDay; i < firstSlotOfNextDay; i++ {
		i := i
		if GetDebugLevel() > 0 && (firstSlotOfNextDay-i)%1000 == 0 {
			log.Printf("DEBUG eth.store: checking blocks for deposits and txs: %.0f%% (%v of %v-%v)\n", 100*float64(i-firstSlotOfCurrentDay)/float64(firstSlotOfNextDay-firstSlotOfCurrentDay), i, firstSlotOfCurrentDay, firstSlotOfNextDay)
		}
		g.Go(func() error {
			block, err := client.SignedBeaconBlock(ctx, fmt.Sprintf("%d", i))
			if err != nil {
				return fmt.Errorf("error getting block %v: %w", i, err)
			}
			if block == nil {
				return nil
			}
			var deposits []*phase0.Deposit
			var exec *bellatrix.ExecutionPayload
			var proposerIndex phase0.ValidatorIndex
			switch block.Version {
			case spec.DataVersionPhase0:
				deposits = block.Phase0.Message.Body.Deposits
				proposerIndex = block.Phase0.Message.ProposerIndex
			case spec.DataVersionAltair:
				deposits = block.Altair.Message.Body.Deposits
				proposerIndex = block.Altair.Message.ProposerIndex
			case spec.DataVersionBellatrix:
				deposits = block.Bellatrix.Message.Body.Deposits
				proposerIndex = block.Bellatrix.Message.ProposerIndex
				exec = block.Bellatrix.Message.Body.ExecutionPayload
			default:
				return fmt.Errorf("unknown block version for block %v: %v", i, block.Version)
			}

			validatorsMu.Lock()
			defer validatorsMu.Unlock()

			if exec != nil {
				v, exists := validatorsByIndex[proposerIndex]
				if exists {
					// only add tx fees of blocks that have been proposed from validators that have been active the whole day
					for _, tx := range exec.Transactions {
						var decTx gethTypes.Transaction
						err := decTx.UnmarshalBinary([]byte(tx))
						if err != nil {
							return err
						}
						txFee := new(big.Int).Mul(decTx.GasPrice(), new(big.Int).SetUint64(decTx.Gas()))
						v.TxFeesSumWei.Add(v.TxFeesSumWei, txFee)
					}
				}
			}

			for _, d := range deposits {
				v, exists := validatorsByPubkey[d.Data.PublicKey]
				if !exists {
					// only add deposits of validators that have been active the whole day
					continue
				}
				if GetDebugLevel() > 0 {
					log.Printf("DEBUG eth.store: extra deposit at block %d from %v: %#x: %v\n", i, v.Index, d.Data.PublicKey, d.Data.Amount)
				}
				v.DepositsSumGwei += d.Data.Amount
			}

			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var totalEffectiveBalanceGwei phase0.Gwei
	var totalStartBalanceGwei phase0.Gwei
	var totalEndBalanceGwei phase0.Gwei
	var totalDepositsSumGwei phase0.Gwei
	totalTxFeesSumWei := new(big.Int)

	for _, v := range validatorsByIndex {
		totalEffectiveBalanceGwei += v.EffectiveBalanceGwei
		totalStartBalanceGwei += v.StartBalanceGwei
		totalEndBalanceGwei += v.EndBalanceGwei
		totalDepositsSumGwei += v.DepositsSumGwei
		totalTxFeesSumWei.Add(totalTxFeesSumWei, v.TxFeesSumWei)
	}

	totalConsensusRewardsGwei := decimal.NewFromInt(int64(totalEndBalanceGwei) - int64(totalStartBalanceGwei) - int64(totalDepositsSumGwei))
	totalRewardsWei := decimal.NewFromBigInt(totalTxFeesSumWei, 0).Add(totalConsensusRewardsGwei.Mul(decimal.NewFromInt(1e9)))

	ethstoreDay := &Day{
		Day:                  day,
		DayTime:              dayTime,
		StartEpoch:           phase0.Epoch(firstEpochOfCurrentDay),
		Apr:                  decimal.NewFromInt(365).Mul(totalRewardsWei).Div(decimal.NewFromInt(int64(totalEffectiveBalanceGwei)).Mul(decimal.NewFromInt(1e9))),
		Validators:           uint64(len(validatorsByIndex)),
		EffectiveBalanceGwei: decimal.NewFromInt(int64(totalEffectiveBalanceGwei)),
		StartBalanceGwei:     decimal.NewFromInt(int64(totalStartBalanceGwei)),
		EndBalanceGwei:       decimal.NewFromInt(int64(totalEndBalanceGwei)),
		DepositsSumGwei:      decimal.NewFromInt(int64(totalDepositsSumGwei)),
		TxFeesSumWei:         decimal.NewFromBigInt(totalTxFeesSumWei, 0),
		ConsensusRewardsGwei: totalConsensusRewardsGwei,
		TotalRewardsWei:      totalRewardsWei,
	}

	if GetDebugLevel() > 0 {
		log.Printf("DEBUG eth.store: %+v\n", ethstoreDay)
	}

	return ethstoreDay, nil
}
