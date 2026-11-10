package ethstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	gethRPC "github.com/ethereum/go-ethereum/rpc"
	"github.com/shopspring/decimal"
)

// apiConsolidation is a consolidation as the beaconcha.in API reports it once processed.
func apiConsolidation(source, target uint64, amountGwei uint64) map[string]any {
	return map[string]any{"amount_consolidated": amountGwei, "source_index": source, "target_index": target}
}

// TestEthstoreConsolidations covers consolidations, whose credits only the beaconcha.in
// API reports. A consolidation that leaves the pending consolidations queue during the
// day moved its source's balance, unless the source was slashed.
func TestEthstoreConsolidations(t *testing.T) {
	const (
		amount    = 1_000_000_000
		finalized = gloasTestEndSlot + 2*gloasTestSlotsPerDay
		forkSlot  = gloasTestEndSlot + 64
	)
	moved := map[uint64]uint64{9: 32e9 - amount + 3200000, 5: 32e9 + amount + 3200000}
	tests := []struct {
		name            string
		scenario        gloasDayScenario
		wantDeposits    int64
		wantWithdrawals int64
		wantErr         error
		fromState       bool
	}{
		{
			name: "consolidation processed",
			scenario: gloasDayScenario{
				endBalances:         moved,
				startConsolidations: []consolidationKey{{Source: 9, Target: 5}},
				apiConsolidations:   map[uint64][]map[string]any{creditedSlot: {apiConsolidation(9, 5, amount)}},
			},
			wantDeposits:    amount,
			wantWithdrawals: amount,
		},
		{
			// the queue doesn't hold the amount a consolidation moves
			name: "consolidation processed, missing from the API",
			scenario: gloasDayScenario{
				endBalances:         moved,
				startConsolidations: []consolidationKey{{Source: 9, Target: 5}},
			},
			wantErr: ErrCreditsNotIndexed,
		},
		{
			// the source's whole balance, which is its effective balance, moves
			name: "consolidation processed, missing from the API, accounted from the state",
			scenario: gloasDayScenario{
				endBalances:         map[uint64]uint64{9: 3200000, 5: 64e9 + 3200000},
				startConsolidations: []consolidationKey{{Source: 9, Target: 5}},
			},
			fromState:       true,
			wantDeposits:    32e9,
			wantWithdrawals: 32e9,
		},
		{
			// a source below its effective balance moves what it has
			name: "consolidation of a source below its effective balance, missing from the API, accounted from the state",
			scenario: gloasDayScenario{
				startBalances:       map[uint64]uint64{9: 31e9},
				endBalances:         map[uint64]uint64{9: 3200000, 5: 63e9 + 3200000},
				startConsolidations: []consolidationKey{{Source: 9, Target: 5}},
			},
			fromState:       true,
			wantDeposits:    31e9,
			wantWithdrawals: 31e9,
		},
		{
			name: "consolidation still pending",
			scenario: gloasDayScenario{
				startConsolidations: []consolidationKey{{Source: 9, Target: 5}},
				endConsolidations:   []consolidationKey{{Source: 9, Target: 5}},
			},
		},
		{
			// the protocol drops it without moving the balance
			name: "consolidation of a slashed source",
			scenario: gloasDayScenario{
				slashed:             map[uint64]bool{9: true},
				startConsolidations: []consolidationKey{{Source: 9, Target: 5}},
			},
		},
		{
			name: "consolidation into a validator outside the day",
			scenario: gloasDayScenario{
				startConsolidations: []consolidationKey{{Source: 9, Target: gloasTestValis + 1}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetUnreportedConsolidationsFromState(tt.fromState)
			defer SetUnreportedConsolidationsFromState(false)
			blockRequests := &atomic.Int32{}
			tt.scenario.blockRequests = blockRequests
			testQueuedExcessDay(t, tt.scenario, forkSlot, finalized, RECEIPTS_MODE_BATCH, tt.wantDeposits, tt.wantWithdrawals, tt.wantErr)
			// a day the API can't account fails before it fetches the day's blocks
			if tt.wantErr != nil && blockRequests.Load() > 0 {
				t.Errorf("fetched %v blocks before failing", blockRequests.Load())
			}
		})
	}
}

// TestEthstoreGloasDepositRequests covers the deposit requests of a gloas block, which
// are those of its parent's payload.
func TestEthstoreGloasDepositRequests(t *testing.T) {
	const (
		topUp       = 2_000_000_000
		forkSlot    = gloasTestFirstSlot + 64
		requestSlot = gloasTestFirstSlot + 100
	)
	signature := "0xa1" + strings.Repeat("11", 95)
	scenario := gloasDayScenario{
		endBalances:     map[uint64]uint64{4: 32e9 + topUp + 3200000},
		depositRequests: map[uint64][]map[string]any{requestSlot: {depositRequest(4, topUp, signature)}},
	}
	testQueuedExcessDay(t, scenario, forkSlot, gloasTestEndSlot+2*gloasTestSlotsPerDay, RECEIPTS_MODE_BATCH, topUp, 0, nil)
}

// TestBeaconchainApiPaging covers a server that ignores the offset and returns the same
// full page forever.
func TestBeaconchainApiPaging(t *testing.T) {
	requests := atomic.Int32{}
	page := `{"status":"OK","data":[` + strings.TrimSuffix(strings.Repeat(`{"amount":1,"pubkey":"0x00","signature":"0x00"},`, beaconchainApiPageSize), ",") + `]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ratelimit-limit", "100000")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(page))
	}))
	defer server.Close()
	c := NewBeaconchainApiClient()
	c.SetBaseUrl(server.URL)
	if _, err := c.DepositRequests(context.Background(), "mainnet", 31); err == nil || !strings.Contains(err.Error(), "full pages") {
		t.Errorf("got error %v, want one about full pages", err)
	}
	if got := requests.Load(); got != beaconchainApiMaxPages {
		t.Errorf("got %v requests, want %v", got, beaconchainApiMaxPages)
	}
}

// TestBeaconchainApiStatus covers a response of the beaconcha.in API that isn't OK.
func TestBeaconchainApiStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ERROR: database unavailable","data":[]}`))
	}))
	defer server.Close()
	c := NewBeaconchainApiClient()
	c.SetBaseUrl(server.URL)
	if _, err := c.DepositRequests(context.Background(), "mainnet", 31); err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Errorf("DepositRequests: got error %v, want the status", err)
	}
	if _, err := c.ConsolidationRequests(context.Background(), "mainnet", 31); err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Errorf("ConsolidationRequests: got error %v, want the status", err)
	}
}

// TestEthstoreDayChecks covers days that can't be calculated.
func TestEthstoreDayChecks(t *testing.T) {
	const forkSlot = gloasTestEndSlot + 64
	t.Run("no day finalized yet", func(t *testing.T) {
		_, _, _, err := runGloasDay(t, gloasDayScenario{forkSlot: forkSlot, finalizedSlot: 100}, RECEIPTS_MODE_BATCH)
		if !errors.Is(err, ErrNoFinalizedDay) {
			t.Fatalf("got error %v, want %v", err, ErrNoFinalizedDay)
		}
	})
	t.Run("every validator exits", func(t *testing.T) {
		exited := map[uint64]bool{}
		for i := uint64(0); i < gloasTestValis; i++ {
			exited[i] = true
		}
		_, _, _, err := runGloasDay(t, gloasDayScenario{forkSlot: forkSlot, finalizedSlot: gloasTestEndSlot + 2*gloasTestSlotsPerDay, exited: exited}, RECEIPTS_MODE_BATCH)
		if err == nil || !strings.Contains(err.Error(), "no validator") {
			t.Fatalf("got error %v, want one about no validator", err)
		}
	})
}

func TestLastFinalizedDay(t *testing.T) {
	tests := []struct {
		finalizedSlot uint64
		wantDay       uint64
		wantErr       error
	}{
		{finalizedSlot: 0, wantErr: ErrNoFinalizedDay},
		{finalizedSlot: 7199, wantErr: ErrNoFinalizedDay},
		{finalizedSlot: 7200, wantDay: 0},
		{finalizedSlot: 14399, wantDay: 0},
		{finalizedSlot: 14400, wantDay: 1},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("slot %d", tt.finalizedSlot), func(t *testing.T) {
			day, err := lastFinalizedDay(tt.finalizedSlot, 7200)
			if !errors.Is(err, tt.wantErr) || err == nil && day != tt.wantDay {
				t.Errorf("got day %v, error %v; want day %v, error %v", day, err, tt.wantDay, tt.wantErr)
			}
		})
	}
}

func TestApr(t *testing.T) {
	if got := apr(decimal.NewFromInt(1e18), 0); !got.Equal(decimal.Zero) {
		t.Errorf("apr without an effective balance: got %v, want 0", got)
	}
	// 0.0032 ETH a day on 32 ETH
	if got, want := apr(decimal.NewFromInt(3200000e9), 32e9), decimal.RequireFromString("0.0365"); !got.Equal(want) {
		t.Errorf("apr: got %v, want %v", got, want)
	}
}

// TestBatchRequestReceiptsElementError covers a batch whose call succeeds while one of
// its receipts fails.
func TestBatchRequestReceiptsElementError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs := []struct {
			ID json.RawMessage `json:"id"`
		}{}
		if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
			t.Error(err)
		}
		res := []string{}
		for i, req := range reqs {
			if i == 1 {
				res = append(res, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"receipt not available"}}`, req.ID))
				continue
			}
			res = append(res, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"effectiveGasPrice":"0xa","gasUsed":"0x5208"}}`, req.ID))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[" + strings.Join(res, ",") + "]"))
	}))
	defer server.Close()
	client, err := gethRPC.Dial(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = batchRequestReceipts(context.Background(), client, []common.Hash{{1}, {2}})
	if err == nil || !strings.Contains(err.Error(), "receipt not available") {
		t.Fatalf("got error %v, want the receipt's error", err)
	}
}
