package ethstore

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/attestantio/go-eth2-client/http"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"
)

var client *http.Service
var debugLevel = uint64(0)
var timeout = time.Second * 120
var timeoutMu = sync.Mutex{}

type Day struct {
	Day              uint64          `json:"day"`
	DayTime          time.Time       `json:"dayTime"`
	Apr              decimal.Decimal `json:"apr"`
	Validators       uint64          `json:"validators"`
	StartEpoch       phase0.Epoch    `json:"startEpoch"`
	EffectiveBalance phase0.Gwei     `json:"effectiveBalance"`
	StartBalance     phase0.Gwei     `json:"startBalance"`
	EndBalance       phase0.Gwei     `json:"endBalance"`
	DepositsSum      phase0.Gwei     `json:"depositsSum"`
	ValidatorSets    map[string]*Day `json:"validatorSets"`
}

type Validator struct {
	Index            phase0.ValidatorIndex
	Pubkey           phase0.BLSPubKey
	EffectiveBalance phase0.Gwei
	StartBalance     phase0.Gwei
	EndBalance       phase0.Gwei
	DepositsSum      phase0.Gwei
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

func Calculate(ctx context.Context, address string, dayStr string, validatorSetsStr map[string][]uint64) (*Day, error) {
	service, err := http.New(ctx, http.WithAddress(address), http.WithTimeout(GetApiTimeout()), http.WithLogLevel(zerolog.WarnLevel))
	if err != nil {
		return nil, err
	}
	client := service.(*http.Service)

	validatorSets := map[string]map[phase0.ValidatorIndex]bool{}
	for name, m := range validatorSetsStr {
		validatorSets[name] = map[phase0.ValidatorIndex]bool{}
		for _, idx := range m {
			validatorSets[name][phase0.ValidatorIndex(idx)] = true
		}
	}

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
		if uint64(val.Validator.ExitEpoch) <= lastEpochOfCurrentDay {
			continue
		}
		v := &Validator{
			Index:            val.Index,
			Pubkey:           val.Validator.PublicKey,
			EffectiveBalance: val.Validator.EffectiveBalance,
			EndBalance:       val.Balance,
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
		v.StartBalance = bal
	}

	g := new(errgroup.Group)
	maxRoutinesGuard := make(chan int, 10) // max 10 routines at a time
	validatorsMu := sync.Mutex{}

	// get all deposits of all active validators in the slot interval [firstSlotOfCurrentDay,lastSlotOfCurrentDay)
	for i := firstSlotOfCurrentDay; i < lastSlotOfCurrentDay; i++ {
		maxRoutinesGuard <- 1
		i := i
		if GetDebugLevel() > 0 && (lastSlotOfCurrentDay-i)%1000 == 0 {
			log.Printf("DEBUG eth.store: checking blocks for deposits: %.0f%% (%v of %v-%v)\n", 100*float64(i-firstSlotOfCurrentDay)/float64(lastSlotOfCurrentDay-firstSlotOfCurrentDay), i, firstSlotOfCurrentDay, lastSlotOfCurrentDay)
		}
		g.Go(func() error {
			defer func() {
				<-maxRoutinesGuard
			}()
			block, err := client.SignedBeaconBlock(ctx, fmt.Sprintf("%d", i))
			if err != nil {
				return err
			}
			if block == nil {
				return nil
			}
			var deposits []*phase0.Deposit
			switch block.Version {
			case spec.DataVersionPhase0:
				deposits = block.Phase0.Message.Body.Deposits
			case spec.DataVersionAltair:
				deposits = block.Altair.Message.Body.Deposits
			case spec.DataVersionBellatrix:
				deposits = block.Bellatrix.Message.Body.Deposits
			default:
				return fmt.Errorf("unknown block version for block %v: %v", i, block.Version)
			}

			validatorsMu.Lock()
			defer validatorsMu.Unlock()

			for _, d := range deposits {
				v, exists := validatorsByPubkey[d.Data.PublicKey]
				if !exists {
					continue
				}
				if GetDebugLevel() > 0 {
					log.Printf("DEBUG eth.store: extra deposit at block %d from %v: %#x: %v\n", i, v.Index, d.Data.PublicKey, d.Data.Amount)
				}
				v.DepositsSum += d.Data.Amount
			}

			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var totalEffectiveBalance phase0.Gwei
	var totalStartBalance phase0.Gwei
	var totalEndBalance phase0.Gwei
	var totalDepositsSum phase0.Gwei
	validatorSetDays := map[string]*Day{}
	for name := range validatorSets {
		validatorSetDays[name] = &Day{}
	}
	for _, v := range validatorsByIndex {
		totalEffectiveBalance += v.EffectiveBalance
		totalStartBalance += v.StartBalance
		totalEndBalance += v.EndBalance
		totalDepositsSum += v.DepositsSum

		for name, set := range validatorSets {
			if _, exists := set[v.Index]; exists {
				validatorSetDays[name].Day = day
				validatorSetDays[name].DayTime = dayTime
				validatorSetDays[name].StartEpoch = phase0.Epoch(firstEpochOfCurrentDay)
				validatorSetDays[name].Validators++
				validatorSetDays[name].EffectiveBalance += v.EffectiveBalance
				validatorSetDays[name].StartBalance += v.StartBalance
				validatorSetDays[name].EndBalance += v.EndBalance
				validatorSetDays[name].DepositsSum += v.DepositsSum
			}
		}
	}

	for _, set := range validatorSetDays {
		setConsensusRewards := int64(set.EndBalance) - int64(set.StartBalance) - int64(set.DepositsSum)
		set.Apr = decimal.NewFromInt(365).Mul(decimal.NewFromInt(setConsensusRewards)).Div(decimal.NewFromInt(int64(set.EffectiveBalance)))
	}

	totalConsensusRewards := int64(totalEndBalance) - int64(totalStartBalance) - int64(totalDepositsSum)

	ethstoreDay := &Day{
		Day:              day,
		DayTime:          dayTime,
		StartEpoch:       phase0.Epoch(firstEpochOfCurrentDay),
		Apr:              decimal.NewFromInt(365).Mul(decimal.NewFromInt(totalConsensusRewards)).Div(decimal.NewFromInt(int64(totalEffectiveBalance))),
		Validators:       uint64(len(validatorsByIndex)),
		EffectiveBalance: totalEffectiveBalance,
		StartBalance:     totalStartBalance,
		EndBalance:       totalEndBalance,
		DepositsSum:      totalDepositsSum,
		ValidatorSets:    validatorSetDays,
	}

	if GetDebugLevel() > 0 {
		fmt.Printf("DEBUG eth.store: %+v\n", ethstoreDay)
		for name, set := range validatorSetDays {
			fmt.Printf("DEBUG eth.store: %v: %+v\n", name, set)
		}
	}

	return ethstoreDay, nil
}
