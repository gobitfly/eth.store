package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	ethstore "github.com/gobitfly/eth.store"
	"github.com/gobitfly/eth.store/version"
)

var opts struct {
	Days       string
	Validators string
	ApiAddress string
	ApiTimeout time.Duration
	Json       bool
	DebugLevel uint64
	Version    bool
}

func main() {
	flag.StringVar(&opts.Days, "days", "", "days to calculate eth.store for, format: \"1-3\" or \"1,4,6\"")
	flag.StringVar(&opts.Validators, "validators", "", "validator-sets to compare ethstore with, format: \"<validatorSetName>:<validatorIndex>,..;..\"")
	flag.StringVar(&opts.ApiAddress, "api.address", "http://localhost:4000", "address of the conensus-node-api")
	flag.DurationVar(&opts.ApiTimeout, "api.timeout", time.Second*120, "timeout duration for the consensus-node-api")
	flag.BoolVar(&opts.Json, "json", false, "format output as json")
	flag.Uint64Var(&opts.DebugLevel, "debug", 0, "set debug-level (higher level will increase verbosity)")
	flag.BoolVar(&opts.Version, "version", false, "print version and exit")
	flag.Parse()

	if opts.Version {
		fmt.Println(version.Version)
		return
	}

	ethstore.SetApiTimeout(opts.ApiTimeout)
	ethstore.SetDebugLevel(opts.DebugLevel)

	validatorSets := map[string][]uint64{}
	if opts.Validators != "" {
		splitSemicolon := strings.Split(opts.Validators, ";")
		for _, s := range splitSemicolon {
			splitDoublecolon := strings.Split(s, ":")
			if len(splitDoublecolon) != 2 {
				log.Fatalf("error parsing validators-flag")
			}
			set := []uint64{}
			splitComma := strings.Split(splitDoublecolon[1], ",")
			for _, ss := range splitComma {
				idx, err := strconv.ParseUint(ss, 10, 64)
				if err != nil {
					log.Fatalf("error parsing validators-flag: %v", err)
				}
				set = append(set, idx)
			}
			validatorSets[splitDoublecolon[0]] = set
		}
	}

	if strings.ContainsAny(opts.Days, "-") {
		daysSplit := strings.Split(opts.Days, "-")
		fromDay, err := strconv.ParseUint(daysSplit[0], 10, 64)
		if err != nil {
			log.Fatalf("error parsing days-flag: %v", err)
		}
		toDay, err := strconv.ParseUint(daysSplit[1], 10, 64)
		if err != nil {
			log.Fatalf("error parsing days-flag: %v", err)
		}
		if toDay < fromDay {
			log.Fatalf("error parsing days-flag: toDay < fromDay")
		}
		for i := fromDay; i <= toDay; i++ {
			calculate(fmt.Sprintf("%d", i), validatorSets)
		}
	} else if strings.ContainsAny(opts.Days, ",") {
		daysSplit := strings.Split(opts.Days, ",")
		for _, dayStr := range daysSplit {
			calculate(dayStr, validatorSets)
		}
	} else {
		calculate(opts.Days, validatorSets)
	}
}

func calculate(dayStr string, validatorSets map[string][]uint64) {
	ethstoreDay, err := ethstore.Calculate(context.Background(), opts.ApiAddress, dayStr, validatorSets)
	if err != nil {
		log.Fatalf("error calculating ethstore: %v", err)
	}
	if opts.Json {
		data, err := json.Marshal(ethstoreDay)
		if err != nil {
			log.Fatalf("error marshaling json: %v", err)
		}
		fmt.Printf("%s\n", data)
		return
	}
	fmt.Printf("day: %v (epoch %v, %v), network.apr: %v", ethstoreDay.Day, ethstoreDay.StartEpoch, ethstoreDay.DayTime, ethstoreDay.Apr.StringFixed(9))
	for name, set := range ethstoreDay.ValidatorSets {
		fmt.Printf(", %s.apr: %v (%v vs network)", name, set.Apr.StringFixed(8), set.Apr.Div(ethstoreDay.Apr).StringFixed(2))
	}
	fmt.Printf("\n")
}
