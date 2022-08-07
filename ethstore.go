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
	lastSlotOfCurrentDay := (day+1)*slotsPerDay - 1
	firstSlotOfNextDay := (day + 1) * slotsPerDay
	firstEpochOfCurrentDay := firstSlotOfCurrentDay / slotsPerEpoch
	lastEpochOfCurrentDay := lastSlotOfCurrentDay / slotsPerEpoch

	genesis, err := client.GenesisTime(ctx)
	if err != nil {
		return nil, err
	}
	dayTime := time.Unix(genesis.Unix()+int64(firstSlotOfCurrentDay)*int64(secondsPerSlot), 0)

	if GetDebugLevel() > 0 {
		log.Printf("DEBUG eth.store: calculating day %v (%v, firstEpochOfCurrentDay: %v, firstSlotOfCurrentDay: %v, firstSlotOfNextDay: %v)\n", day, dayTime, firstEpochOfCurrentDay, firstSlotOfCurrentDay, firstSlotOfNextDay)
	}

	validatorsByIndex := map[phase0.ValidatorIndex]*Validator{}
	validatorsByPubkey := map[phase0.BLSPubKey]*Validator{}

	allValidators, err := client.Validators(ctx, fmt.Sprintf("%d", firstSlotOfNextDay), nil)
	if err != nil {
		return nil, err
	}
	for _, val := range allValidators {
		if uint64(val.Validator.ActivationEpoch) > firstEpochOfCurrentDay {
			continue
		}
		if uint64(val.Validator.ExitEpoch) < lastEpochOfCurrentDay {
			continue
		}
		v := &Validator{
			Index:                val.Index,
			Pubkey:               val.Validator.PublicKey,
			EffectiveBalanceGwei: val.Validator.EffectiveBalance,
			EndBalanceGwei:       val.Balance,
			TxFeesSumWei:         new(big.Int),
		}
		validatorsByIndex[val.Index] = v
		validatorsByPubkey[val.Validator.PublicKey] = v
	}

	startBalances, err := client.ValidatorBalances(ctx, fmt.Sprintf("%d", firstSlotOfCurrentDay), nil)
	if err != nil {
		return nil, err
	}
	for idx, bal := range startBalances {
		v, exists := validatorsByIndex[idx]
		if !exists {
			continue
		}
		v.StartBalanceGwei = bal
	}

	g := new(errgroup.Group)
	g.SetLimit(10)
	validatorsMu := sync.Mutex{}

	// get all deposits and txs of all active validators in the slot interval [firstSlotOfCurrentDay,lastSlotOfCurrentDay)
	for i := firstSlotOfCurrentDay; i < lastSlotOfCurrentDay; i++ {
		i := i
		if GetDebugLevel() > 0 && (lastSlotOfCurrentDay-i)%1000 == 0 {
			log.Printf("DEBUG eth.store: checking blocks for deposits and txs: %.0f%% (%v of %v-%v)\n", 100*float64(i-firstSlotOfCurrentDay)/float64(lastSlotOfCurrentDay-firstSlotOfCurrentDay), i, firstSlotOfCurrentDay, lastSlotOfCurrentDay)
		}
		g.Go(func() error {
			block, err := client.SignedBeaconBlock(ctx, fmt.Sprintf("%d", i))
			if err != nil {
				return err
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
	totalRewardsWei := decimal.NewFromBigInt(totalTxFeesSumWei, 1).Add(totalConsensusRewardsGwei.Mul(decimal.NewFromInt(1e9)))

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
		TxFeesSumWei:         decimal.NewFromBigInt(totalTxFeesSumWei, 1),
		ConsensusRewardsGwei: totalConsensusRewardsGwei,
		TotalRewardsWei:      totalRewardsWei,
	}

	if GetDebugLevel() > 0 {
		log.Printf("DEBUG eth.store: %+v\n", ethstoreDay)
	}

	return ethstoreDay, nil
}
