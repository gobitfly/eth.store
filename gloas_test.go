package ethstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/shopspring/decimal"
)

func testHash(n uint64) phase0.Hash32 {
	var h phase0.Hash32
	for i := 0; i < 8; i++ {
		h[31-i] = byte(n >> (8 * i))
	}
	h[0] = 0xee
	return h
}

// chain builds gloas blocks for the given slots. A slot in empty has its
// payload withheld, so the next block builds on the last FULL payload.
func chain(parent phase0.Hash32, slots []uint64, empty map[uint64]bool) []*payloadBlock {
	blocks := []*payloadBlock{}
	latest := parent
	for _, s := range slots {
		blocks = append(blocks, &payloadBlock{Slot: s, Gloas: true, BlockHash: testHash(s), ParentBlockHash: latest})
		if !empty[s] {
			latest = testHash(s)
		}
	}
	return blocks
}

func TestResolveGloasPayloads(t *testing.T) {
	parent := testHash(0)
	tests := []struct {
		name        string
		blocks      []*payloadBlock
		wantFull    map[uint64]bool
		wantPayers  map[uint64]uint64
		wantPending []uint64
	}{
		{
			name:        "every payload full: each block pays its own withdrawals",
			blocks:      chain(parent, []uint64{1, 2, 3}, nil),
			wantFull:    map[uint64]bool{1: true, 2: true},
			wantPayers:  map[uint64]uint64{1: 1, 2: 2},
			wantPending: []uint64{3},
		},
		{
			name:        "empty payload: its withdrawals are paid by the next full payload",
			blocks:      chain(parent, []uint64{1, 2, 3, 4}, map[uint64]bool{2: true}),
			wantFull:    map[uint64]bool{1: true, 2: false, 3: true},
			wantPayers:  map[uint64]uint64{1: 1, 2: 3},
			wantPending: []uint64{4},
		},
		{
			name:        "missed slots and two empty payloads in a row",
			blocks:      chain(parent, []uint64{1, 4, 5, 7, 9}, map[uint64]bool{4: true, 5: true}),
			wantFull:    map[uint64]bool{1: true, 4: false, 5: false, 7: true},
			wantPayers:  map[uint64]uint64{1: 1, 4: 7},
			wantPending: []uint64{9},
		},
		{
			name:        "last payload empty: its withdrawals stay pending",
			blocks:      chain(parent, []uint64{1, 2}, map[uint64]bool{2: true}),
			wantFull:    map[uint64]bool{1: true},
			wantPayers:  map[uint64]uint64{1: 1},
			wantPending: []uint64{2},
		},
		{
			name: "fork boundary: the first gloas block builds on the last pre-gloas payload",
			blocks: append(
				[]*payloadBlock{{Slot: 1, BlockHash: testHash(1)}},
				chain(testHash(1), []uint64{2, 3}, nil)...,
			),
			wantFull:    map[uint64]bool{2: true},
			wantPayers:  map[uint64]uint64{2: 2},
			wantPending: []uint64{3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			full, payers, pending := resolveGloasPayloads(parent, tt.blocks)
			if !reflect.DeepEqual(full, tt.wantFull) {
				t.Errorf("full: got %v, want %v", full, tt.wantFull)
			}
			if !reflect.DeepEqual(payers, tt.wantPayers) {
				t.Errorf("payers: got %v, want %v", payers, tt.wantPayers)
			}
			if !reflect.DeepEqual(pending, tt.wantPending) {
				t.Errorf("pending: got %v, want %v", pending, tt.wantPending)
			}
		})
	}
}

// TestEthstoreGloas calculates day 10 of a devnet that is post-gloas from
// genesis. Every slot has a block, proposed by validator slot%10, and every
// payload is FULL except the ones at firstSlot, emptySlot and endSlot.
//
//   - validator 1's withdrawal is debited at firstSlot, so it is already part of
//     the start balance and must not count, although the payload of
//     firstSlot+1 pays it inside the day.
//   - validator 2's withdrawal is debited at emptySlot and paid in the payload
//     of emptySlot+1. It counts.
//   - validator 3's withdrawal is debited at endSlot and paid in the payload of
//     endSlot+1, after the day. It counts, and needs the lookahead up to
//     endSlot+2, the block that shows the payload of endSlot+1 is FULL.
//   - a builder payment (BUILDER_INDEX_FLAG | 5) matches no validator.
//   - every FULL payload in [firstSlot,endSlot) pays its proposer txFeeWei; the
//     EMPTY payload at emptySlot pays nothing.
func TestEthstoreGloas(t *testing.T) {
	const (
		day          = 10
		slotsPerDay  = 7200
		firstSlot    = day * slotsPerDay
		endSlot      = (day + 1) * slotsPerDay
		emptySlot    = firstSlot + 100
		numValis     = 10
		txFeeWei     = 21000 * 3
		builderFlag  = uint64(1) << 40
		finalizedSlt = endSlot + 2*slotsPerDay
	)
	withdrawals := map[uint64][]elWithdrawal{
		firstSlot + 1: {{Index: 1, ValidatorIndex: 1, Amount: 5e9}},
		emptySlot + 1: {{Index: 2, ValidatorIndex: 2, Amount: 7e9}, {Index: 3, ValidatorIndex: hexutil.Uint64(builderFlag | 5), Amount: 1e9}},
		endSlot + 1:   {{Index: 4, ValidatorIndex: 3, Amount: 11e9}},
	}
	empty := map[uint64]bool{firstSlot: true, emptySlot: true, endSlot: true}

	template, err := os.ReadFile("testdata/gloas_block.json")
	if err != nil {
		t.Fatal(err)
	}
	blockJson := func(slot, parentSlot uint64, parentBlockHash phase0.Hash32) string {
		var b map[string]any
		if err := json.Unmarshal(template, &b); err != nil {
			t.Fatal(err)
		}
		msg := b["data"].(map[string]any)["message"].(map[string]any)
		msg["slot"] = fmt.Sprintf("%d", slot)
		msg["proposer_index"] = fmt.Sprintf("%d", slot%numValis)
		msg["parent_root"] = fmt.Sprintf("%#x", testHash(parentSlot))
		bid := msg["body"].(map[string]any)["signed_execution_payload_bid"].(map[string]any)["message"].(map[string]any)
		bid["slot"] = fmt.Sprintf("%d", slot)
		bid["block_hash"] = fmt.Sprintf("%#x", testHash(slot))
		bid["parent_block_hash"] = fmt.Sprintf("%#x", parentBlockHash)
		bid["parent_block_root"] = fmt.Sprintf("%#x", testHash(parentSlot))
		out, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	mocks := map[string]string{
		"/eth/v1/beacon/genesis":           `{"data":{"genesis_time":"1606824023","genesis_validators_root":"0x4b363db94e286120d76eb905340fdd4e54bfe9f06bf33ff6cf5ad27f511bfe95","genesis_fork_version":"0x10733183"}}`,
		"/eth/v1/beacon/headers/finalized": fmt.Sprintf(`{"data":{"root":"0x3aee29bcfa7a9fdf01394a3dce74ae063c89023df71867ad1555f1e494d138ee","canonical":true,"header":{"message":{"slot":"%d","proposer_index":"1","parent_root":"0x4a451b6a4962bcbd619ee1f0b6a7d85dded49f049877de325122e21350e5d6f2","state_root":"0xf12219d8bcdb7ed125da01e4f7aa30754bff2c9fc0bf57dd728c0b02bb847a92","body_root":"0x31f4433e6e260a0fac6e80ad3f9df1998fbbab269408601a6da7a5d32ccbb258"},"signature":"0x8ccb90ff41ec1f82975fb12384f3d44194b27403f1454e878e9c07c9951df33968556e2ce0dfb8ce42e2e0bbac8c80e211d35d01617712292805bc8d9ac2e3429f821953cfc1dbb9d9ea359cd37b39850f4e29c81fc3d67e150985c609d4e826"}}}`, finalizedSlt),
		"/eth/v1/config/spec":              `{"data":{"CONFIG_NAME":"devnet","PRESET_BASE":"mainnet","GENESIS_FORK_VERSION":"0x10733183","ELECTRA_FORK_EPOCH":"0","FULU_FORK_EPOCH":"0","GLOAS_FORK_EPOCH":"0","SECONDS_PER_SLOT":"12","SLOTS_PER_EPOCH":"32","DOMAIN_DEPOSIT":"0x03000000","DEPOSIT_CHAIN_ID":"7091047534","DEPOSIT_NETWORK_ID":"7091047534","DEPOSIT_CONTRACT_ADDRESS":"0x00000000219ab540356cbb839cbe05303d7705fa"}}`,
		"/eth/v1/config/deposit_contract":  `{"data":{"chain_id":"7091047534","address":"0x00000000219ab540356cbb839cbe05303d7705fa"}}`,
		"/eth/v1/config/fork_schedule":     `{"data":[{"previous_version":"0x10733183","current_version":"0x10733183","epoch":"0"}]}`,
		"/eth/v1/node/version":             `{"data":{"version":"Lighthouse/v8.2.2-4b1f3c2/x86_64-linux"}}`,
		"/eth/v1/node/syncing":             `{"data":{"head_slot":"100000","sync_distance":"0","is_syncing":false,"is_optimistic":false,"el_offline":false}}`,
	}

	stateJson := func(slot uint64, balance uint64) string {
		vals, bals := []string{}, []string{}
		for i := 0; i < numValis; i++ {
			vals = append(vals, fmt.Sprintf(`{"pubkey":"%#096x","withdrawal_credentials":"%#064x","effective_balance":"32000000000","slashed":false,"activation_eligibility_epoch":"0","activation_epoch":"0","exit_epoch":"18446744073709551615","withdrawable_epoch":"18446744073709551615"}`, i, i))
			bals = append(bals, fmt.Sprintf(`"%d"`, balance))
		}
		return phase0StateJson(fmt.Sprintf("%d", slot), []byte("["+strings.Join(vals, ",")+"]"), []byte("["+strings.Join(bals, ",")+"]"))
	}
	mocks[fmt.Sprintf("/eth/v2/debug/beacon/states/%d", firstSlot)] = stateJson(firstSlot, 32e9)
	mocks[fmt.Sprintf("/eth/v2/debug/beacon/states/%d", endSlot)] = stateJson(endSlot, 32e9+3200000)

	// the parent of the first block is reached by root, and is itself FULL
	mocks[fmt.Sprintf("/eth/v2/beacon/blocks/%#x", testHash(firstSlot-1))] = blockJson(firstSlot-1, firstSlot-2, testHash(firstSlot-2))
	latest := testHash(firstSlot - 1)
	for s := uint64(firstSlot); s <= endSlot+2; s++ {
		mocks[fmt.Sprintf("/eth/v2/beacon/blocks/%d", s)] = blockJson(s, s-1, latest)
		if !empty[s] {
			latest = testHash(s)
		}
	}
	for e := uint64(firstSlot/32 + 1); e <= endSlot/32; e++ {
		mocks[fmt.Sprintf("/api/v1/slot/%d/deposit_requests", e*32-1)] = `{"status":"OK","data":[]}`
		mocks[fmt.Sprintf("/api/v1/slot/%d/consolidation_requests", e*32-1)] = `{"status":"OK","data":[]}`
	}

	bnServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mock, exists := mocks[r.URL.Path]
		if !exists {
			t.Logf("no mock for %v %v", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ratelimit-limit", "100000")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(mock))
	}))
	defer bnServer.Close()

	type rpcReq struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params []any           `json:"params"`
	}
	slotOfHash := map[string]uint64{}
	for s := uint64(firstSlot - 2); s <= endSlot+2; s++ {
		if !empty[s] {
			slotOfHash[fmt.Sprintf("%#x", testHash(s))] = s
		}
	}
	answer := func(req rpcReq) string {
		switch req.Method {
		case "eth_getBlockByHash":
			slot, exists := slotOfHash[req.Params[0].(string)]
			if !exists {
				return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, req.ID)
			}
			ws := withdrawals[slot]
			if ws == nil {
				ws = []elWithdrawal{}
			}
			wsJson, _ := json.Marshal(ws)
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"hash":"%#x","number":"%s","baseFeePerGas":"0x7","gasUsed":"0x4e20","transactions":["%#x"],"withdrawals":%s}}`, req.ID, testHash(slot), hexutil.EncodeUint64(slot), testHash(slot+1e9), wsJson)
		case "eth_getTransactionReceipt":
			// gasUsed 21000 at effectiveGasPrice 10 with a base fee of 7 leaves a priority fee of 21000*3.
			// The header reports 20000 gas: Amsterdam headers report less gas than their receipts sum
			// to, so the burn must not be taken from the header.
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"effectiveGasPrice":"0xa","gasUsed":"0x5208","cumulativeGasUsed":"0x5208","logsBloom":"0x","status":"0x1","transactionIndex":"0x0","type":"0x2"}}`, req.ID)
		}
		t.Errorf("unexpected el method %v", req.Method)
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unexpected"}}`, req.ID)
	}
	elServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) > 0 && raw[0] == '[' {
			reqs := []rpcReq{}
			json.Unmarshal(raw, &reqs)
			res := []string{}
			for _, req := range reqs {
				res = append(res, answer(req))
			}
			w.Write([]byte("[" + strings.Join(res, ",") + "]"))
			return
		}
		var req rpcReq
		json.Unmarshal(raw, &req)
		w.Write([]byte(answer(req)))
	}))
	defer elServer.Close()

	SetBeaconchainApiBaseUrl(bnServer.URL)
	defer SetBeaconchainApiBaseUrl("")

	res, _, err := Calculate(context.Background(), bnServer.URL, elServer.URL, fmt.Sprintf("%d", day), 8, RECEIPTS_MODE_BATCH)
	if err != nil {
		t.Fatal(err)
	}

	wantWithdrawalsGwei := decimal.NewFromInt(7e9 + 11e9)
	if !res.WithdrawalsSumGwei.Equal(wantWithdrawalsGwei) {
		t.Errorf("WithdrawalsSumGwei: got %v, want %v", res.WithdrawalsSumGwei, wantWithdrawalsGwei)
	}
	fullPayloadsInWindow := int64(slotsPerDay - 2)
	wantTxFeesWei := decimal.NewFromBigInt(new(big.Int).Mul(big.NewInt(fullPayloadsInWindow), big.NewInt(txFeeWei)), 0)
	if !res.TxFeesSumWei.Equal(wantTxFeesWei) {
		t.Errorf("TxFeesSumWei: got %v, want %v", res.TxFeesSumWei, wantTxFeesWei)
	}
	wantConsensusGwei := decimal.NewFromInt(numValis*3200000 + 7e9 + 11e9)
	if !res.ConsensusRewardsGwei.Equal(wantConsensusGwei) {
		t.Errorf("ConsensusRewardsGwei: got %v, want %v", res.ConsensusRewardsGwei, wantConsensusGwei)
	}
}
