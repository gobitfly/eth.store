package ethstore

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/signing"
	"github.com/prysmaticlabs/prysm/v5/contracts/deposit"
	"github.com/prysmaticlabs/prysm/v5/crypto/bls"
	ethpb "github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1"
	"github.com/shopspring/decimal"
)

type pendingDepositJson struct {
	Pubkey                string `json:"pubkey"`
	WithdrawalCredentials string `json:"withdrawal_credentials"`
	Amount                string `json:"amount"`
	Signature             string `json:"signature"`
	Slot                  string `json:"slot"`
}

// excessSignature is the G2 point at infinity the protocol signs its own pending deposits with.
var excessSignature = "0xc0" + strings.Repeat("00", 95)

func pendingDeposit(validator uint64, amountGwei uint64, signature string, slot uint64) pendingDepositJson {
	return pendingDepositOf(fmt.Sprintf("%#096x", validator), amountGwei, signature, slot)
}

// The beaconcha.in API reports the deposits the queue credits during an epoch at the
// epoch's last slot. creditedSlot is one in the middle of the day; firstCreditedSlot and
// lastCreditedSlot are the first and the last the day asks the API for.
const (
	creditedSlot      = gloasTestFirstSlot + 10*32 - 1
	requestSlot       = gloasTestFirstSlot + 5
	firstCreditedSlot = gloasTestFirstSlot + 32 - 1
	lastCreditedSlot  = gloasTestEndSlot - 1
)

// apiDeposit is a deposit to a validator as the beaconcha.in API reports it once the
// queue credits it.
func apiDeposit(validator uint64, amountGwei uint64, signature string) map[string]any {
	return apiDepositOf(fmt.Sprintf("%#096x", validator), amountGwei, signature)
}

// depositRequest is a deposit request to a validator in a block.
func depositRequest(validator uint64, amountGwei uint64, signature string) map[string]any {
	return map[string]any{
		"pubkey":                 fmt.Sprintf("%#096x", validator),
		"withdrawal_credentials": "0x" + strings.Repeat("00", 32),
		"amount":                 fmt.Sprintf("%d", amountGwei),
		"signature":              signature,
		"index":                  "1",
	}
}

func apiDepositOf(pubkey string, amountGwei uint64, signature string) map[string]any {
	return map[string]any{"amount": amountGwei, "pubkey": pubkey, "signature": signature}
}

func pendingDepositOf(pubkey string, amountGwei uint64, signature string, slot uint64) pendingDepositJson {
	return pendingDepositJson{
		Pubkey:                pubkey,
		WithdrawalCredentials: "0x" + strings.Repeat("00", 32),
		Amount:                fmt.Sprintf("%d", amountGwei),
		Signature:             signature,
		Slot:                  fmt.Sprintf("%d", slot),
	}
}

// signedLegacyDeposit returns a fresh key's pubkey and a legacy deposit to it with a
// valid signature on the devnet of runGloasDay, so that eth.store does not skip it.
func signedLegacyDeposit(t *testing.T, amountGwei uint64) (string, map[string]any) {
	t.Helper()
	key, err := bls.RandKey()
	if err != nil {
		t.Fatal(err)
	}
	data := &ethpb.Deposit_Data{
		PublicKey:             key.PublicKey().Marshal(),
		WithdrawalCredentials: make([]byte, 32),
		Amount:                amountGwei,
	}
	domain, err := signing.ComputeDomain([4]byte{0x03}, []byte{0x10, 0x73, 0x31, 0x83}, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	root, err := signing.ComputeSigningRoot(&ethpb.DepositMessage{PublicKey: data.PublicKey, WithdrawalCredentials: data.WithdrawalCredentials, Amount: data.Amount}, domain)
	if err != nil {
		t.Fatal(err)
	}
	data.Signature = key.Sign(root[:]).Marshal()
	if err := deposit.VerifyDepositSignature(data, domain); err != nil {
		t.Fatalf("test deposit has an invalid signature: %v", err)
	}
	proof := make([]string, 33)
	for i := range proof {
		proof[i] = "0x" + strings.Repeat("00", 32)
	}
	return hexutil.Encode(data.PublicKey), map[string]any{
		"proof": proof,
		"data": map[string]any{
			"pubkey":                 hexutil.Encode(data.PublicKey),
			"withdrawal_credentials": hexutil.Encode(data.WithdrawalCredentials),
			"amount":                 fmt.Sprintf("%d", data.Amount),
			"signature":              hexutil.Encode(data.Signature),
		},
	}
}

// TestEthstoreQueuedExcess covers balance that moves through the pending deposits
// queue. Switching a validator to compounding queues its balance above 32 ETH, and the
// Electra fork did so for validators that already had compounding credentials; the
// queue credits it back later, weeks later on mainnet but often the same day on a
// testnet with a short queue. That excess is read from the queues of the two states.
// From Electra on, a legacy deposit is only queued when its block includes it. The
// beaconcha.in API reports every deposit the queue credits, the excess included, and
// a sender may sign a deposit with the point at infinity the excess is signed with.
// None of these is a reward: every validator earns 0.0032 ETH whatever the queue does.
func TestEthstoreQueuedExcess(t *testing.T) {
	const (
		excess    = 1_000_000_000
		topUp     = 2_000_000_000
		finalized = gloasTestEndSlot + 2*gloasTestSlotsPerDay
		forkSlot  = gloasTestEndSlot + 64
	)
	realSignature := "0xa1" + strings.Repeat("11", 95)
	legacyPubkey, legacyDeposit := signedLegacyDeposit(t, 1e9)
	legacySignature := legacyDeposit["data"].(map[string]any)["signature"].(string)
	tests := []struct {
		name            string
		scenario        gloasDayScenario
		wantDeposits    int64
		wantWithdrawals int64
		wantErr         error
	}{
		{
			name: "switch to compounding queues the excess",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 + excess},
				endQueue:      []pendingDepositJson{pendingDeposit(4, excess, excessSignature, 0)},
			},
			wantWithdrawals: excess,
		},
		{
			name: "queued excess credited back",
			scenario: gloasDayScenario{
				endBalances: map[uint64]uint64{4: 32e9 + excess + 3200000},
				startQueue:  []pendingDepositJson{pendingDeposit(4, excess, excessSignature, 0)},
				apiDeposits: map[uint64][]map[string]any{lastCreditedSlot: {apiDeposit(4, excess, excessSignature)}},
			},
			wantDeposits: excess,
		},
		{
			// the API's indexer can lag behind finality
			name: "queued excess credited back, missing from the API",
			scenario: gloasDayScenario{
				endBalances: map[uint64]uint64{4: 32e9 + excess + 3200000},
				startQueue:  []pendingDepositJson{pendingDeposit(4, excess, excessSignature, 0)},
			},
			wantDeposits: excess,
		},
		{
			name: "queued top-up credited",
			scenario: gloasDayScenario{
				endBalances: map[uint64]uint64{4: 32e9 + topUp + 3200000},
				startQueue:  []pendingDepositJson{pendingDeposit(4, topUp, realSignature, gloasTestFirstSlot-100)},
				apiDeposits: map[uint64][]map[string]any{creditedSlot: {apiDeposit(4, topUp, realSignature)}},
			},
			wantDeposits: topUp,
		},
		{
			// the queues show the credit, which would otherwise be a reward
			name: "queued top-up credited, missing from the API",
			scenario: gloasDayScenario{
				endBalances: map[uint64]uint64{4: 32e9 + topUp + 3200000},
				startQueue:  []pendingDepositJson{pendingDeposit(4, topUp, realSignature, gloasTestFirstSlot-100)},
			},
			wantDeposits: topUp,
		},
		{
			// it counts towards the balance a switch could queue
			name: "queued top-up missing from the API before a same-day round trip",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 - excess/10},
				endBalances:   map[uint64]uint64{4: 32e9 - excess/10 + topUp + 3200000},
				startQueue:    []pendingDepositJson{pendingDeposit(4, topUp, realSignature, gloasTestFirstSlot-100)},
				apiDeposits:   map[uint64][]map[string]any{creditedSlot: {apiDeposit(4, topUp-excess/10, excessSignature)}},
			},
			wantDeposits: topUp,
		},
		{
			// a short queue credits a top-up the day its block requests it
			name: "top-up requested and credited the same day",
			scenario: gloasDayScenario{
				endBalances:     map[uint64]uint64{4: 32e9 + topUp + 3200000},
				depositRequests: map[uint64][]map[string]any{requestSlot: {depositRequest(4, topUp, realSignature)}},
				apiDeposits:     map[uint64][]map[string]any{creditedSlot: {apiDeposit(4, topUp, realSignature)}},
			},
			wantDeposits: topUp,
		},
		{
			// neither queue shows it, only the block that requested it
			name: "top-up requested and credited the same day, missing from the API",
			scenario: gloasDayScenario{
				endBalances:     map[uint64]uint64{4: 32e9 + topUp + 3200000},
				depositRequests: map[uint64][]map[string]any{requestSlot: {depositRequest(4, topUp, realSignature)}},
			},
			wantDeposits: topUp,
		},
		{
			name: "top-up requested and still queued",
			scenario: gloasDayScenario{
				depositRequests: map[uint64][]map[string]any{requestSlot: {depositRequest(4, topUp, realSignature)}},
				endQueue:        []pendingDepositJson{pendingDeposit(4, topUp, realSignature, requestSlot)},
			},
		},
		{
			// the block at firstSlot is part of the start state, the one at endSlot of the end state
			name: "top-ups requested at the day's first and end slot",
			scenario: gloasDayScenario{
				endBalances: map[uint64]uint64{4: 32e9 + topUp + 3200000},
				depositRequests: map[uint64][]map[string]any{
					gloasTestFirstSlot: {depositRequest(4, topUp, realSignature)},
					gloasTestEndSlot:   {depositRequest(4, 2*topUp, realSignature)},
				},
				startQueue: []pendingDepositJson{pendingDeposit(4, topUp, realSignature, gloasTestFirstSlot)},
				endQueue:   []pendingDepositJson{pendingDeposit(4, 2*topUp, realSignature, gloasTestEndSlot)},
			},
			wantDeposits: topUp,
		},
		{
			// the API's indexer missed the round trip; the block shows the credit was a
			// deposit request, so it can't be the excess
			name: "top-up signed with the point at infinity requested the same day next to a round trip missing from the API",
			scenario: gloasDayScenario{
				switched:        map[uint64]bool{4: true},
				startBalances:   map[uint64]uint64{4: 32e9 + excess},
				endBalances:     map[uint64]uint64{4: 32e9 + excess + excess + excess/10 + 3200000},
				depositRequests: map[uint64][]map[string]any{requestSlot: {depositRequest(4, excess+excess/10, excessSignature)}},
				apiDeposits:     map[uint64][]map[string]any{creditedSlot: {apiDeposit(4, excess+excess/10, excessSignature)}},
			},
			wantDeposits: excess + excess/10,
		},
		{
			name: "queued top-up postponed",
			scenario: gloasDayScenario{
				startQueue: []pendingDepositJson{pendingDeposit(4, topUp, realSignature, gloasTestFirstSlot-100)},
				endQueue:   []pendingDepositJson{pendingDeposit(4, topUp, realSignature, gloasTestFirstSlot-100)},
			},
		},
		{
			name: "queued deposit to a validator outside the day credited, missing from the API",
			scenario: gloasDayScenario{
				startQueue: []pendingDepositJson{pendingDeposit(gloasTestValis+1, topUp, realSignature, gloasTestFirstSlot-100)},
			},
		},
		{
			// the API's rows of a slot come in pages
			name: "more credits in an epoch than one page of the API",
			scenario: gloasDayScenario{
				endBalances: map[uint64]uint64{6: 32e9 + 250 + 3200000},
				apiDeposits: map[uint64][]map[string]any{creditedSlot: manyApiDeposits(6, 250, realSignature)},
			},
			wantDeposits: 250,
		},
		{
			// the switch comes after the top-up lifted the balance above 32 ETH
			name: "same-day round trip after a top-up signed with the point at infinity",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 - excess/10},
				endBalances:   map[uint64]uint64{4: 32e9 - excess/10 + topUp + 3200000},
				apiDeposits: map[uint64][]map[string]any{
					firstCreditedSlot: {apiDeposit(4, topUp, excessSignature)},
					creditedSlot:      {apiDeposit(4, topUp-excess/10, excessSignature)},
				},
			},
			wantDeposits: topUp,
		},
		{
			// the API's indexer missed the round trip; the start queue shows the top-up
			// was a deposit request, so it can't be the excess
			name: "queued top-up signed with the point at infinity next to a round trip missing from the API",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 + excess},
				endBalances:   map[uint64]uint64{4: 32e9 + excess + excess + excess/10 + 3200000},
				startQueue:    []pendingDepositJson{pendingDeposit(4, excess+excess/10, excessSignature, gloasTestFirstSlot-100)},
				apiDeposits:   map[uint64][]map[string]any{creditedSlot: {apiDeposit(4, excess+excess/10, excessSignature)}},
			},
			wantDeposits: excess + excess/10,
		},
		{
			name: "top-up signed with the point at infinity beyond the tolerance of the excess",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 + excess},
				endBalances:   map[uint64]uint64{4: 32e9 + excess + excess + 3*excess/10 + 3200000},
				apiDeposits:   map[uint64][]map[string]any{creditedSlot: {apiDeposit(4, excess+3*excess/10, excessSignature)}},
			},
			wantDeposits: excess + 3*excess/10,
		},
		{
			// the closest credit is the round trip, whatever the order
			name: "same-day round trip and a top-up signed with the point at infinity just inside the tolerance",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 + excess},
				endBalances:   map[uint64]uint64{4: 32e9 + excess + excess + excess/5 + 3200000},
				apiDeposits: map[uint64][]map[string]any{
					firstCreditedSlot: {apiDeposit(4, excess+excess/5, excessSignature)},
					creditedSlot:      {apiDeposit(4, excess, excessSignature)},
				},
			},
			wantDeposits: excess + excess/5,
		},
		{
			// neither state shows the excess, only the API's credit does
			name: "excess queued and credited back the same day",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 + excess},
				endBalances:   map[uint64]uint64{4: 32e9 + excess + 3200000},
				apiDeposits:   map[uint64][]map[string]any{creditedSlot: {apiDeposit(4, excess, excessSignature)}},
			},
		},
		{
			name: "top-ups signed with the point at infinity",
			scenario: gloasDayScenario{
				endBalances: map[uint64]uint64{5: 32e9 + topUp + 3200000, 6: 32e9 + 2*topUp + 3200000},
				apiDeposits: map[uint64][]map[string]any{
					firstCreditedSlot: {apiDeposit(5, topUp, excessSignature)},
					lastCreditedSlot:  {apiDeposit(6, 2*topUp, excessSignature)},
				},
			},
			wantDeposits: 3 * topUp,
		},
		{
			// the top-up is credited before the switch, which queues it with the excess
			name: "switch after a top-up signed with the point at infinity",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 + excess},
				endQueue:      []pendingDepositJson{pendingDeposit(4, excess+topUp, excessSignature, 0)},
				apiDeposits:   map[uint64][]map[string]any{creditedSlot: {apiDeposit(4, topUp, excessSignature)}},
			},
			wantDeposits:    topUp,
			wantWithdrawals: excess + topUp,
		},
		{
			name: "excess credited back the same day as a top-up signed with the point at infinity",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 + excess},
				endBalances:   map[uint64]uint64{4: 32e9 + excess + topUp + 3200000},
				apiDeposits: map[uint64][]map[string]any{
					creditedSlot:     {apiDeposit(4, topUp, excessSignature)},
					lastCreditedSlot: {apiDeposit(4, excess, excessSignature)},
				},
			},
			wantDeposits: topUp,
		},
		{
			// no excess is queued below the minimum activation balance
			name: "switch without excess and a top-up signed with the point at infinity",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 - 1e7},
				endBalances:   map[uint64]uint64{4: 32e9 - 1e7 + topUp + 3200000},
				apiDeposits:   map[uint64][]map[string]any{creditedSlot: {apiDeposit(4, topUp, excessSignature)}},
			},
			wantDeposits: topUp,
		},
		{
			// the API's indexer can lag behind finality
			name: "same-day round trip missing from the API next to a top-up signed with the point at infinity",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 + excess},
				endBalances:   map[uint64]uint64{4: 32e9 + excess + topUp + 3200000},
				apiDeposits:   map[uint64][]map[string]any{creditedSlot: {apiDeposit(4, topUp, excessSignature)}},
			},
			wantDeposits: topUp,
		},
		{
			// the switch comes after a top-up the same day, which it queues with the excess
			name: "same-day round trip after a top-up",
			scenario: gloasDayScenario{
				switched:      map[uint64]bool{4: true},
				startBalances: map[uint64]uint64{4: 32e9 + 1e7},
				endBalances:   map[uint64]uint64{4: 32e9 + 1e7 + 6e8 + 3200000},
				apiDeposits: map[uint64][]map[string]any{
					creditedSlot:     {apiDeposit(4, 6e8, realSignature)},
					lastCreditedSlot: {apiDeposit(4, 6e8+1e7, excessSignature)},
				},
			},
			wantDeposits: 6e8,
		},
		{
			// a withdrawal before the switch leaves 0.9 ETH of the 1 ETH excess to queue, and
			// a top-up signed with the point at infinity is as far from it on the other side:
			// the smaller credit is taken for the round trip
			name: "same-day round trip and a top-up equally far from the excess",
			scenario: gloasDayScenario{
				switched:        map[uint64]bool{4: true},
				startBalances:   map[uint64]uint64{4: 32e9 + excess},
				endBalances:     map[uint64]uint64{4: 32e9 + excess - 1e8 + 11e8 + 3200000},
				fuluWithdrawals: map[uint64][]elWithdrawal{gloasTestFirstSlot + 100: {{Index: 1, ValidatorIndex: 4, Amount: 1e8}}},
				apiDeposits: map[uint64][]map[string]any{
					creditedSlot:     {apiDeposit(4, 9e8, excessSignature)},
					lastCreditedSlot: {apiDeposit(4, 11e8, excessSignature)},
				},
			},
			wantDeposits:    11e8,
			wantWithdrawals: 1e8,
		},
		{
			// Gnosis keeps 1 GNO when it queues the excess
			name: "same-day round trip with the spec's minimum activation balance",
			scenario: gloasDayScenario{
				minActivationBalance: 1e9,
				switched:             map[uint64]bool{4: true},
				startBalances:        map[uint64]uint64{4: 1e9 + 5e8},
				endBalances:          map[uint64]uint64{4: 1e9 + 5e8 + 1e7 + 3200000},
				apiDeposits: map[uint64][]map[string]any{
					creditedSlot:     {apiDeposit(4, 1e7, excessSignature)},
					lastCreditedSlot: {apiDeposit(4, 5e8, excessSignature)},
				},
			},
			wantDeposits: 1e7,
		},
		{
			// the fork queues the excess of a validator with compounding credentials, and
			// the queue credits it back the same day
			name: "electra queues the excess of a compounding validator",
			scenario: gloasDayScenario{
				electraForkSlot: gloasTestFirstSlot + 320,
				compounding:     map[uint64]bool{4: true},
				startBalances:   map[uint64]uint64{4: 32e9 + excess},
				endBalances:     map[uint64]uint64{4: 32e9 + excess + 3200000},
				apiDeposits:     map[uint64][]map[string]any{lastCreditedSlot: {apiDeposit(4, excess, excessSignature)}},
			},
		},
		{
			name: "excess queued before the day and still queued",
			scenario: gloasDayScenario{
				startQueue: []pendingDepositJson{pendingDeposit(4, excess, excessSignature, 0)},
				endQueue:   []pendingDepositJson{pendingDeposit(4, excess, excessSignature, 0)},
			},
		},
		{
			// deposit requests are accounted from the beaconcha.in API when they are credited
			name: "queued deposits are not excess",
			scenario: gloasDayScenario{
				endQueue: []pendingDepositJson{
					pendingDeposit(5, 1e9, realSignature, gloasTestFirstSlot+10),
					pendingDeposit(6, 1e9, realSignature, 0),
					pendingDeposit(7, 1e9, excessSignature, gloasTestFirstSlot+10),
				},
			},
		},
		{
			// queued with slot 0 and the point at infinity like the excess, by a validator
			// that didn't switch to compounding
			name: "legacy deposit signed with the point at infinity is not accounted before it is credited",
			scenario: gloasDayScenario{
				endQueue: []pendingDepositJson{pendingDeposit(5, 1e9, excessSignature, 0)},
			},
		},
		{
			name: "legacy deposit included after Electra is not accounted before it is credited",
			scenario: gloasDayScenario{
				pubkeys:      map[uint64]string{6: legacyPubkey},
				fuluDeposits: map[uint64][]map[string]any{gloasTestFirstSlot + 5: {legacyDeposit}},
				endQueue:     []pendingDepositJson{pendingDepositOf(legacyPubkey, 1e9, legacySignature, 0)},
			},
		},
		{
			name: "legacy deposit credited",
			scenario: gloasDayScenario{
				endBalances: map[uint64]uint64{6: 32e9 + 1e9 + 3200000},
				startQueue:  []pendingDepositJson{pendingDeposit(6, 1e9, realSignature, 0)},
				apiDeposits: map[uint64][]map[string]any{creditedSlot: {apiDeposit(6, 1e9, realSignature)}},
			},
			wantDeposits: 1e9,
		},
		{
			// the state at firstSlot predates Electra and has no queue to ask for
			name: "electra activates during the day",
			scenario: gloasDayScenario{
				electraForkSlot: gloasTestFirstSlot + 320,
				switched:        map[uint64]bool{4: true},
				startBalances:   map[uint64]uint64{4: 32e9 + excess},
				endQueue:        []pendingDepositJson{pendingDeposit(4, excess, excessSignature, 0)},
			},
			wantWithdrawals: excess,
		},
		{
			name: "excess of a validator outside the day",
			scenario: gloasDayScenario{
				endQueue: []pendingDepositJson{pendingDeposit(gloasTestValis+1, excess, excessSignature, 0)},
			},
		},
	}
	for _, tt := range tests {
		for _, mode := range []int{RECEIPTS_MODE_BATCH, RECEIPTS_MODE_SINGLE} {
			t.Run(fmt.Sprintf("%s/receipts mode %d", tt.name, mode), func(t *testing.T) {
				testQueuedExcessDay(t, tt.scenario, forkSlot, finalized, mode, tt.wantDeposits, tt.wantWithdrawals, tt.wantErr)
			})
		}
	}
}

// manyApiDeposits returns n credits of 1 Gwei to a validator as the beaconcha.in API reports them.
func manyApiDeposits(validator uint64, n int, signature string) []map[string]any {
	deposits := []map[string]any{}
	for i := 0; i < n; i++ {
		deposits = append(deposits, apiDeposit(validator, 1, signature))
	}
	return deposits
}

func testQueuedExcessDay(t *testing.T, scenario gloasDayScenario, forkSlot, finalized uint64, mode int, wantDeposits, wantWithdrawals int64, wantErr error) {
	t.Helper()
	scenario.forkSlot = forkSlot
	scenario.finalizedSlot = finalized
	res, validators, _, err := runGloasDay(t, scenario, mode)
	if wantErr != nil {
		if !errors.Is(err, wantErr) {
			t.Fatalf("got error %v, want %v", err, wantErr)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if want := decimal.NewFromInt(gloasTestValis * 3200000); !res.ConsensusRewardsGwei.Equal(want) {
		t.Errorf("ConsensusRewardsGwei: got %v, want %v", res.ConsensusRewardsGwei, want)
	}
	for index, v := range validators {
		if want := decimal.NewFromInt(3200000); !v.ConsensusRewardsGwei.Equal(want) {
			t.Errorf("validator %d ConsensusRewardsGwei: got %v, want %v", index, v.ConsensusRewardsGwei, want)
		}
	}
	if want := decimal.NewFromInt(wantDeposits); !res.DepositsSumGwei.Equal(want) {
		t.Errorf("DepositsSumGwei: got %v, want %v", res.DepositsSumGwei, want)
	}
	if want := decimal.NewFromInt(wantWithdrawals); !res.WithdrawalsSumGwei.Equal(want) {
		t.Errorf("WithdrawalsSumGwei: got %v, want %v", res.WithdrawalsSumGwei, want)
	}
}

// TestCreditEpochs covers the epochs whose credits a day asks the API for. A day that
// doesn't end on an epoch boundary, as with 8-second slots, must stop before the epoch
// whose transition comes after its end state.
func TestCreditEpochs(t *testing.T) {
	tests := []struct {
		name                string
		firstSlot, endSlot  uint64
		slotsPerEpoch       uint64
		wantFirst, wantLast uint64
		wantNumberOfEpochs  int
	}{
		{name: "12-second slots", firstSlot: 72000, endSlot: 79200, slotsPerEpoch: 32, wantFirst: 2251, wantLast: 2475, wantNumberOfEpochs: 225},
		{name: "8-second slots", firstSlot: 21600, endSlot: 32400, slotsPerEpoch: 32, wantFirst: 676, wantLast: 1012, wantNumberOfEpochs: 337},
		{name: "day starting mid-epoch", firstSlot: 32400, endSlot: 43200, slotsPerEpoch: 32, wantFirst: 1013, wantLast: 1350, wantNumberOfEpochs: 338},
		{name: "5-second slots, 16-slot epochs", firstSlot: 17280, endSlot: 34560, slotsPerEpoch: 16, wantFirst: 1081, wantLast: 2160, wantNumberOfEpochs: 1080},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			epochs := creditEpochs(tt.firstSlot, tt.endSlot, tt.slotsPerEpoch)
			if len(epochs) != tt.wantNumberOfEpochs || epochs[0] != tt.wantFirst || epochs[len(epochs)-1] != tt.wantLast {
				t.Errorf("got %d epochs %v..%v, want %d epochs %v..%v", len(epochs), epochs[0], epochs[len(epochs)-1], tt.wantNumberOfEpochs, tt.wantFirst, tt.wantLast)
			}
		})
	}
}
