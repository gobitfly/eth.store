package ethstore

import (
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

// TestEthstoreQueuedExcess covers balance the protocol moves between a validator
// and the pending deposits queue: switching a validator to compounding queues its
// balance above 32 ETH, and the queue credits it back weeks later. From Electra on,
// a legacy deposit is likewise only queued when its block includes it. None of
// these is a reward. Every validator earns 0.0032 ETH, so the day's consensus rewards stay
// at gloasTestValis*3200000 Gwei whatever the queue does.
func TestEthstoreQueuedExcess(t *testing.T) {
	const (
		excess    = 8_200_000_000
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
	}{
		{
			name: "switch to compounding queues the excess",
			scenario: gloasDayScenario{
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
			},
			wantDeposits: excess,
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
				},
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
			name: "legacy deposit credited from the queue",
			scenario: gloasDayScenario{
				pubkeys:     map[uint64]string{6: legacyPubkey},
				endBalances: map[uint64]uint64{6: 32e9 + 1e9 + 3200000},
				startQueue:  []pendingDepositJson{pendingDepositOf(legacyPubkey, 1e9, legacySignature, 0)},
			},
			wantDeposits: 1e9,
		},
		{
			// the state at firstSlot predates Electra and has no queue to ask for
			name: "electra activates during the day",
			scenario: gloasDayScenario{
				electraForkSlot: gloasTestFirstSlot + 320,
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
		t.Run(tt.name, func(t *testing.T) {
			tt.scenario.forkSlot = forkSlot
			tt.scenario.finalizedSlot = finalized
			res, _, err := runGloasDay(t, tt.scenario, RECEIPTS_MODE_BATCH)
			if err != nil {
				t.Fatal(err)
			}
			if want := decimal.NewFromInt(gloasTestValis * 3200000); !res.ConsensusRewardsGwei.Equal(want) {
				t.Errorf("ConsensusRewardsGwei: got %v, want %v", res.ConsensusRewardsGwei, want)
			}
			if want := decimal.NewFromInt(tt.wantDeposits); !res.DepositsSumGwei.Equal(want) {
				t.Errorf("DepositsSumGwei: got %v, want %v", res.DepositsSumGwei, want)
			}
			if want := decimal.NewFromInt(tt.wantWithdrawals); !res.WithdrawalsSumGwei.Equal(want) {
				t.Errorf("WithdrawalsSumGwei: got %v, want %v", res.WithdrawalsSumGwei, want)
			}
		})
	}
}
