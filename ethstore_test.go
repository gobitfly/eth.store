package ethstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/shopspring/decimal"
)

func TestEthstore(t *testing.T) {
	// in this scenario we want to calculate the eth.store-apr for day 10
	// for simplicity there are only 33 validators with indices 0 to 32
	// all validators have the same start-, end- and effective-balance (respectively: 32 Eth, 32.0032 Eth and 32 Eth)
	// every validator proposed the same amount of blocks during day 10 (besides validator 0 who did not propose a block during day 10)
	// all blocks have the same txFeeSum of 10000 Gwei
	// validator 0 exited on the last epoch of day 9
	// validator 1 exited on the last epoch of day 10
	// validator 2 activated on the second epoch of day 10
	// validator 3 activated on the last epoch of day 10 and deposited 32 Eth extra during day 10
	// validator 4 deposited 100 Eth extra during day 10
	// therefore only 29 validators (indices 4 to 32) should be considered when calculating the eth.store, which is: 365 * (sumOfEndBalances - sumOfStartBalances - sumOfExtraDeposits + sumOfTxFees) / sumOfEffbalancesAtStart
	// given our scenario this should result in 365 * (28*32.0032e18+1*32.0032e18+32e18 - 29*32e18 - 1*32e18 + 10000e9*32*225*29/32) / (32e18*29) = 0.0621640625
	// explaining the numbers:
	// - 365 is the number of days in a year (we ignore leap-years for apr-calculation of eth.store)
	// - 28*32.0032e18+1*32.0032e18+32e18 = sumOfEndBalances = 28 validators each with a balance of 32.0032 eth at the end of the day and one validator deposited extra 100 eth - which is added to the endBalance of the validator
	// - 29*32e18 = sumOfStartBalances = 29 validators each with 32 eth start balance
	// - 1*32e18 = sumOfExtraDeposits = 1 validator deposited 100 eth extra in the set of validators that is considered for the calculation, note that the other deposit should not be considered
	// - 10000e9*32*225*29/32 = sumOfTxFees = 10000 Gwei tx-fee for txs in 32*225 blocks (32 blocks in 225 epochs), but only 29 of the 32 validators who actually propose blocks are in the eth.store validator-set
	// - 32e18*29 = sumOfEffectiveBalances = 29 validators have each an effective balance of 32 eth at the start of the eth.store-day
	// - 0.0621640625 = eth.store-apr = according to the eth.store-calculation validators will earn 6.22% interest in a year

	mocks := map[string]string{
		"/eth/v1/beacon/genesis":           `{"data":{"genesis_time":"1606824023","genesis_validators_root":"0x4b363db94e286120d76eb905340fdd4e54bfe9f06bf33ff6cf5ad27f511bfe95","genesis_fork_version":"0x00000000"}}`,
		"/eth/v1/beacon/headers/finalized": `{"data":{"root":"0x3aee29bcfa7a9fdf01394a3dce74ae063c89023df71867ad1555f1e494d138ee","canonical":true,"header":{"message":{"slot":"4485760","proposer_index":"44643","parent_root":"0x4a451b6a4962bcbd619ee1f0b6a7d85dded49f049877de325122e21350e5d6f2","state_root":"0xf12219d8bcdb7ed125da01e4f7aa30754bff2c9fc0bf57dd728c0b02bb847a92","body_root":"0x31f4433e6e260a0fac6e80ad3f9df1998fbbab269408601a6da7a5d32ccbb258"},"signature":"0x8ccb90ff41ec1f82975fb12384f3d44194b27403f1454e878e9c07c9951df33968556e2ce0dfb8ce42e2e0bbac8c80e211d35d01617712292805bc8d9ac2e3429f821953cfc1dbb9d9ea359cd37b39850f4e29c81fc3d67e150985c609d4e826"}}}`,
		"/eth/v1/config/spec":              `{"data":{"CONFIG_NAME":"mainnet","PRESET_BASE":"mainnet","TERMINAL_TOTAL_DIFFICULTY":"115792089237316195423570985008687907853269984665640564039457584007913129638912","TERMINAL_BLOCK_HASH":"0x0000000000000000000000000000000000000000000000000000000000000000","TERMINAL_BLOCK_HASH_ACTIVATION_EPOCH":"18446744073709551615","SAFE_SLOTS_TO_IMPORT_OPTIMISTICALLY":"128","MIN_GENESIS_ACTIVE_VALIDATOR_COUNT":"16384","MIN_GENESIS_TIME":"1606824000","GENESIS_FORK_VERSION":"0x00000000","GENESIS_DELAY":"604800","ALTAIR_FORK_VERSION":"0x01000000","ALTAIR_FORK_EPOCH":"74240","BELLATRIX_FORK_VERSION":"0x02000000","BELLATRIX_FORK_EPOCH":"18446744073709551615","SECONDS_PER_SLOT":"12","SECONDS_PER_ETH1_BLOCK":"14","MIN_VALIDATOR_WITHDRAWABILITY_DELAY":"256","SHARD_COMMITTEE_PERIOD":"256","ETH1_FOLLOW_DISTANCE":"2048","INACTIVITY_SCORE_BIAS":"4","INACTIVITY_SCORE_RECOVERY_RATE":"16","EJECTION_BALANCE":"16000000000","MIN_PER_EPOCH_CHURN_LIMIT":"4","CHURN_LIMIT_QUOTIENT":"65536","PROPOSER_SCORE_BOOST":"40","DEPOSIT_CHAIN_ID":"1","DEPOSIT_NETWORK_ID":"1","DEPOSIT_CONTRACT_ADDRESS":"0x00000000219ab540356cbb839cbe05303d7705fa","MAX_COMMITTEES_PER_SLOT":"64","TARGET_COMMITTEE_SIZE":"128","MAX_VALIDATORS_PER_COMMITTEE":"2048","SHUFFLE_ROUND_COUNT":"90","HYSTERESIS_QUOTIENT":"4","HYSTERESIS_DOWNWARD_MULTIPLIER":"1","HYSTERESIS_UPWARD_MULTIPLIER":"5","SAFE_SLOTS_TO_UPDATE_JUSTIFIED":"8","MIN_DEPOSIT_AMOUNT":"1000000000","MAX_EFFECTIVE_BALANCE":"32000000000","EFFECTIVE_BALANCE_INCREMENT":"1000000000","MIN_ATTESTATION_INCLUSION_DELAY":"1","SLOTS_PER_EPOCH":"32","MIN_SEED_LOOKAHEAD":"1","MAX_SEED_LOOKAHEAD":"4","EPOCHS_PER_ETH1_VOTING_PERIOD":"64","SLOTS_PER_HISTORICAL_ROOT":"8192","MIN_EPOCHS_TO_INACTIVITY_PENALTY":"4","EPOCHS_PER_HISTORICAL_VECTOR":"65536","EPOCHS_PER_SLASHINGS_VECTOR":"8192","HISTORICAL_ROOTS_LIMIT":"16777216","VALIDATOR_REGISTRY_LIMIT":"1099511627776","BASE_REWARD_FACTOR":"64","WHISTLEBLOWER_REWARD_QUOTIENT":"512","PROPOSER_REWARD_QUOTIENT":"8","INACTIVITY_PENALTY_QUOTIENT":"67108864","MIN_SLASHING_PENALTY_QUOTIENT":"128","PROPORTIONAL_SLASHING_MULTIPLIER":"1","MAX_PROPOSER_SLASHINGS":"16","MAX_ATTESTER_SLASHINGS":"2","MAX_ATTESTATIONS":"128","MAX_DEPOSITS":"16","MAX_VOLUNTARY_EXITS":"16","INACTIVITY_PENALTY_QUOTIENT_ALTAIR":"50331648","MIN_SLASHING_PENALTY_QUOTIENT_ALTAIR":"64","PROPORTIONAL_SLASHING_MULTIPLIER_ALTAIR":"2","SYNC_COMMITTEE_SIZE":"512","EPOCHS_PER_SYNC_COMMITTEE_PERIOD":"256","MIN_SYNC_COMMITTEE_PARTICIPANTS":"1","RANDOM_SUBNETS_PER_VALIDATOR":"1","EPOCHS_PER_RANDOM_SUBNET_SUBSCRIPTION":"256","DOMAIN_DEPOSIT":"0x03000000","DOMAIN_SELECTION_PROOF":"0x05000000","DOMAIN_BEACON_ATTESTER":"0x01000000","BLS_WITHDRAWAL_PREFIX":"0x00","TARGET_AGGREGATORS_PER_COMMITTEE":"16","DOMAIN_BEACON_PROPOSER":"0x00000000","DOMAIN_VOLUNTARY_EXIT":"0x04000000","DOMAIN_RANDAO":"0x02000000","DOMAIN_AGGREGATE_AND_PROOF":"0x06000000"}}`,
		"/eth/v1/config/deposit_contract":  `{"data":{"chain_id":"1","address":"0x00000000219ab540356cbb839cbe05303d7705fa"}}`,
		"/eth/v1/config/fork_schedule":     `{"data":[{"previous_version":"0x00000000","current_version":"0x00000000","epoch":"0"},{"previous_version":"0x00000000","current_version":"0x01000000","epoch":"74240"}]}`,
		"/eth/v1/node/version":             `{"data":{"version":"Lighthouse/v2.3.1-564d7da/x86_64-linux"}}`,
		"/eth/v2/beacon/blocks/0":          `{"version":"phase0","data":{"message":{"slot":"0","proposer_index":"0","parent_root":"0x0000000000000000000000000000000000000000000000000000000000000000","state_root":"0x7e76880eb67bbdc86250aa578958e9d0675e64e714337855204fb5abaaf82c2b","body":{"randao_reveal":"0x000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000","eth1_data":{"deposit_root":"0x0000000000000000000000000000000000000000000000000000000000000000","deposit_count":"0","block_hash":"0x0000000000000000000000000000000000000000000000000000000000000000"},"graffiti":"0x0000000000000000000000000000000000000000000000000000000000000000","proposer_slashings":[],"attester_slashings":[],"attestations":[],"deposits":[],"voluntary_exits":[]}},"signature":"0x000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"}}`,
	}

	type MockValidator struct {
		Index     string `json:"index"`
		Balance   string `json:"balance"`
		Status    string `json:"status"`
		Validator struct {
			Pubkey                     string `json:"pubkey"`
			WithdrawalCredentials      string `json:"withdrawal_credentials"`
			EffectiveBalance           string `json:"effective_balance"`
			Slashed                    bool   `json:"slashed"`
			ActivationEligibilityEpoch string `json:"activation_eligibility_epoch"`
			ActivationEpoch            string `json:"activation_epoch"`
			ExitEpoch                  string `json:"exit_epoch"`
			WithdrawableEpoch          string `json:"withdrawable_epoch"`
		} `json:"validator"`
	}

	type MockValidatorsResponse struct {
		Data []MockValidator `json:"data"`
	}

	txFeeGweiPerBlock := uint64(10000)
	numValis := 33

	mockStartValidators := MockValidatorsResponse{make([]MockValidator, numValis)}
	for i := 0; i < numValis; i++ {
		v := MockValidator{}
		v.Index = fmt.Sprintf("%d", i)
		v.Balance = "32000000000"
		v.Status = "active_ongoing"
		v.Validator.Pubkey = fmt.Sprintf("%#096x", i)
		v.Validator.WithdrawalCredentials = fmt.Sprintf("%#064x", i)
		v.Validator.EffectiveBalance = "32000000000"
		v.Validator.Slashed = false
		v.Validator.ActivationEligibilityEpoch = "18446744073709551615"
		v.Validator.ActivationEpoch = "0"
		v.Validator.ExitEpoch = "18446744073709551615"
		v.Validator.WithdrawableEpoch = "18446744073709551615"
		mockStartValidators.Data[i] = v
	}

	mockEndValidators := MockValidatorsResponse{make([]MockValidator, numValis)}
	for i := 0; i < numValis; i++ {
		v := MockValidator{}
		v.Index = fmt.Sprintf("%d", i)
		v.Balance = "32003200000"
		v.Status = "active_ongoing"
		v.Validator.Pubkey = fmt.Sprintf("%#096x", i)
		v.Validator.WithdrawalCredentials = fmt.Sprintf("%#064x", i)
		v.Validator.EffectiveBalance = "32000000000"
		v.Validator.Slashed = false
		v.Validator.ActivationEligibilityEpoch = "18446744073709551615"
		v.Validator.ActivationEpoch = "0"
		v.Validator.ExitEpoch = "18446744073709551615"
		v.Validator.WithdrawableEpoch = "18446744073709551615"
		mockEndValidators.Data[i] = v
	}

	// validator 0 exited on the last epoch of day 9
	mockStartValidators.Data[0].Validator.ExitEpoch = fmt.Sprintf("%d", 10*225-1)
	mockStartValidators.Data[0].Status = "exited_unslashed"
	mockEndValidators.Data[0].Validator.ExitEpoch = fmt.Sprintf("%d", 10*225-1)
	mockEndValidators.Data[0].Status = "exited_unslashed"
	mockEndValidators.Data[0].Balance = "32000000000"

	// validator 1 exited on the last epoch of day 10
	mockEndValidators.Data[1].Validator.ExitEpoch = fmt.Sprintf("%d", 11*225-1)
	mockEndValidators.Data[1].Status = "exited_unslashed"

	// validator 2 activated on the second epoch of day 10
	mockStartValidators.Data[2].Validator.ActivationEpoch = fmt.Sprintf("%d", 10*225+1)
	mockStartValidators.Data[2].Status = "pending_queued"
	mockEndValidators.Data[2].Validator.ActivationEpoch = fmt.Sprintf("%d", 10*225+1)
	mockEndValidators.Data[2].Status = "active_ongoing"

	// validator 3 activated on the last epoch of day 10 and deposited 100 Eth extra during day 10
	mockStartValidators.Data[3].Validator.ActivationEpoch = fmt.Sprintf("%d", 11*225-1)
	mockStartValidators.Data[3].Status = "pending_queued"

	// validator 4 deposited 100 Eth extra during day 10
	mockStartValidators.Data[4].Validator.Pubkey = "0xb07210c8839f03532d8b7e27a1b0ec9503454fa29a2cbe563896636757214247699420553ce51f78fa9d72d79d0a2fc1"
	mockEndValidators.Data[4].Validator.Pubkey = "0xb07210c8839f03532d8b7e27a1b0ec9503454fa29a2cbe563896636757214247699420553ce51f78fa9d72d79d0a2fc1"
	mockEndValidators.Data[4].Balance = "64003200000"

	mockStartValidatorsJson, err := json.Marshal(&mockStartValidators)
	if err != nil {
		t.Error(err)
	}

	mockEndValidatorsJson, err := json.Marshal(&mockEndValidators)
	if err != nil {
		t.Error(err)
	}

	mocks["/eth/v1/beacon/states/72000/validators"] = string(mockStartValidatorsJson)
	mocks["/eth/v1/beacon/states/79200/validators"] = string(mockEndValidatorsJson)

	validator4DidExtraDeposit := false
	for i := 10 * 225 * 32; i < 11*225*32; i++ {
		proposer := i%(numValis-1) + 1 // validator with index 0 does not propose blocks on this day
		deposits := "[]"
		if proposer == 4 && !validator4DidExtraDeposit {
			// validator 4 deposited 100 Eth extra during day 10
			validator4DidExtraDeposit = true
			deposits = `[{ "proof": [
				"0x31ed74479712c2491d2e7bff9ed80ded0b04b0942aa8d8b6ca88b395a8a8304f",
				"0x039558b9726ffe6b11821479dfa67015ec1410d188ca24737023a6f8e2d53beb",
				"0x7f80693b5faa17634e22839a297d666deeedb5cca5d10557c3c170fde0bb394b",
				"0xb780adf27e299d624ed5457e1b167f10d179e2e59d5a8ec7ba224ad718a372dc",
				"0x128e971feb79575a43068178dfa3303cb3422dfb7f4ffe20da5dafba4606c393",
				"0xfa1d156f4b0259b8978f2bc46f508cc19715425309ac22bd728b33ea5f9cdf3a",
				"0xa4fe1d1ddcfc3b32cc0a75cd1dfa64bc7c03d249a9ffd7cb185616b34aef9b98",
				"0x1d858dab8772a3f5b75b2a79aa34e2d8166ad9b1d2dd21ffab2d334fab655af6",
				"0x26846476fd5fc54a5d43385167c95144f2643f533cc85bb9d16b782f8d7db193",
				"0x506d86582d252405b840018792cad2bf1259f1ef5aa5f887e13cb2f0094f51e1",
				"0xc78d288053ee9804305df45757ae117aa8c753fd37828aca98c8ffc0e6d6786d",
				"0xb9310c29b98283c7c26147228bbfa149fc07bca20972aad5c01f0a7010a02403",
				"0xbab071abf3bdba4a30f6f3bbaa0f6ca98a7d8c17596b47d1908324c40a14ca50",
				"0xdf6af5f5bbdb6be9ef8aa618e4bf8073960867171e29676f8b284dea6a08a85e",
				"0xb58d900f5e182e3c50ef74969ea16c7726c549757cc23523c369587da7293784",
				"0xd49a7502ffcfb0340b1d7885688500ca308161a7f96b62df9d083b71fcc8f2bb",
				"0x8fe6b1689256c0d385f42f5bbe2027a22c1996e110ba97c171d3e5948de92beb",
				"0x8d0d63c39ebade8509e0ae3c9c3876fb5fa112be18f905ecacfecb92057603ab",
				"0xd47152335d9460f2b6fb7aba05ced32a52e9f46659ccd3daa2059661d75a6308",
				"0xf893e908917775b62bff23294dbbe3a1cd8e6cc1c35b4801887b646a6f81f17f",
				"0xcddba7b592e3133393c16194fac7431abf2f5485ed711db282183c819e08ebaa",
				"0x8a8d7fe3af8caa085a7639a832001457dfb9128a8061142ad0335629ff23ff9c",
				"0xfeb3c337d7a51a6fbf00b9e34c52e1c9195c969bd4e7a0bfd51d5c5bed9c1167",
				"0xe71f0aa83cc32edfbefa9f4d3e0174ca85182eec9f3a09f6a6c0df6377a510d7",
				"0x31206fa80a50bb6abe29085058f16212212a60eec8f049fecb92d8c8e0a84bc0",
				"0x21352bfecbeddde993839f614c3dac0a3ee37543f9b412b16199dc158e23b544",
				"0x619e312724bb6d7c3153ed9de791d764a366b389af13c58bf8a8d90481a46765",
				"0x7cdd2986268250628d0c10e385c58c6191e6fbe05191bcc04f133f2cea72c1c4",
				"0x848930bd7ba8cac54661072113fb278869e07bb8587f91392933374d017bcbe1",
				"0x8869ff2c22b28cc10510d9853292803328be4fb0e80495e8bb8d271f5b889636",
				"0xb5fe28e79f1b850f8658246ce9b6a1e7b49fc06db7143e8fe0b4f2b0c5523a5c",
				"0x985e929f70af28d0bdd1a90a808f977f597c7c778c489e98d3bd8910d31ac0f7",
				"0xab1c040000000000000000000000000000000000000000000000000000000000"
			],
			"data": {
				"pubkey": "0xb07210c8839f03532d8b7e27a1b0ec9503454fa29a2cbe563896636757214247699420553ce51f78fa9d72d79d0a2fc1",
				"withdrawal_credentials": "0x000278f69b54118f70ff1ae310e465f0e3676ff585ddcbba448d4c9317d4cf32",
				"amount": "32000000000",
				"signature": "0xa70b7440dd48d5b0d11e530c63ba307dfa07a011b695e8f0621555e6af85e365da6f7de39f61ad5f13ee9f8b9d5c10990d52cb993eb5ad2e7f0cf7f96a33bc596444972ca5d99e134bbb166fc720a8ca04f3ee9027756f91afacf8d6603cd392"
			} }]`
		}
		mocks[fmt.Sprintf("/eth/v2/beacon/blocks/%d", i)] = fmt.Sprintf(`{"version":"bellatrix","data":{"message":{"slot":"%d","proposer_index":"%d","parent_root":"0xae77f6e0db57769b5ec6c16c4ef7489ddd47728d98297833b5a1692afc5072cb","state_root":"0x3c900df8e277bade69a1c29a93f9442940fc5e43a96c60dfc33d0f0a54a73af6","body":{"randao_reveal":"0x886b31ed2d6caead1e6632dcaec7edb113789f81dbc101160f903ad72c01429203c15ae75e00bd6987ca5ec79750f9c6040a7805284b24f5b3fa8131579c743e592033de069345ccb4b9a99fd73712d8b2276791847282dbfb7634fcb050ae80","eth1_data":{"deposit_root":"0x9df92d765b5aa041fd4bbe8d5878eb89290efa78e444c1a603eecfae2ea05fa4","deposit_count":"403","block_hash":"0x4d0d1732d9a72d2127ab2ad120e66da738cab3369239ec9debd7aea3b89f9812"},"graffiti":"0x0000000000000000000000000000000000000000000000000000000000000000","proposer_slashings":[],"attester_slashings":[],"attestations":[{"aggregation_bits":"0xf7fa6fffbcbbbf6f","data":{"slot":"357843","index":"0","beacon_block_root":"0xae77f6e0db57769b5ec6c16c4ef7489ddd47728d98297833b5a1692afc5072cb","source":{"epoch":"11181","root":"0xa0d0f93cc58e7e0a6b08c600d2a8054dc41fbadd8aba116e6e8cb1a1870321d0"},"target":{"epoch":"11182","root":"0x82cf146d63ea46194fb6ea4e2c99b244aea76cf8c6546ae09a749a0406d78823"}},"signature":"0xad7d675b775c89fb5c1605f1c91bb595e4feb0a2a0440b23aacfbc6d95daa02e761e8ad48a6cf0dd041d65250a97bf1200e879212f389173cdb2c5792d977411aa44f62eb79e71447f00f2eb02c3aacb4fdc4e939a5d7d01a2198ccdb758b641"}],"deposits":%s,"voluntary_exits":[],"sync_aggregate":{"sync_committee_bits":"0xf74edf53ffdb7f7f7db76efef7fcfb6eff7ffeffbff7f7fddf3f57f7d7fff1b7b7fb3e7bffffff5afe7fffff7fcb437fdffee3efd6dff76df766ffffd7fffff1","sync_committee_signature":"0x98fef94f6488bcb1d1c47517e28683d280c36cfd3caa37403e40a72b0500de7ce84f234760edc17a2bd1031db194570d17af1eb253d4d117f88b39e30ee0ab7c00db268db8369188600a9665708ddd34701840ca1bc1b3c646641b60eda2019d"},"execution_payload":{"parent_hash":"0xca7e7e7fcf3ef35a569c1647d56b11873664e3972d17c5dc339af901230166d5","fee_recipient":"0x8b0c2c4c8eb078bc6c01f48523764c8942c0c6c4","state_root":"0x65ff6f9be55e066f1ed9f5f899752e174c31793034260389316c0ae897483512","receipts_root":"0x1544df33845496bdab8cb97867ec0c6e060ed6690e54c85ae4cb9cc58ddc00dd","logs_bloom":"0x08000000000000000000000000000000000000000000000000000000000000000000100000000000000000000000000000000000000200000000000000000000000000004000000000000001002000000000000001000000000000000000000000000000020000000100000000000800000000000000000000000000000000080000000000000000000000000000000000000480000000008000000000000000000000001040000000000000000000000000000000000000000000000000000000000000000000000400000000000000004000000001000000000000000020000000000000000000000000000000000000000000000000000000000000000010","prev_randao":"0x3c3397f7c670538c30a11f6c5733e66af09f9a34ab0ef31b0ffa63314b79099f","block_number":"1663387","gas_limit":"30000000","gas_used":"230800","timestamp":"1660027728","extra_data":"0x","base_fee_per_gas":"10","block_hash":"0x8145108c4ba0bd6507019ee9ef1eaa225daa0fd220bfea44f5e1d3b58c313875","transactions":["%#x"]}}},"signature":"0x8b0c109f0148cd7979bc8101f35e909c8b24e08fbfb0a36491270f2d3889c08b71ab83f59f005eff75272627e569f2d91769524dd5790f918955315534e245ad65423fe45f6fb749d9d4cc593c6f56388eef6c5b123b0f7cb526cbdf7fa053c8"}}`, i, proposer, deposits, createTx(txFeeGweiPerBlock))
	}

	bnServer := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mock, exists := mocks[r.URL.Path]
			if !exists {
				t.Errorf("mock does not exist for request: %v", r.URL.Path)
			}
			w.Write([]byte(mock))
		}),
	)
	defer bnServer.Close()

	elServer := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			effectiveGasPrice := hexutil.EncodeUint64(100)
			gasUsed := hexutil.EncodeUint64(1e11 + 23080)
			d := []byte(fmt.Sprintf(`[{ "jsonrpc": "2.0", "result": { "blockHash": "0xf76daa365606f130e620444e48512cca421318cfebc5b1152a5494c7ef6fe6fc", "blockNumber": "0x712208", "contractAddress": null, "cumulativeGasUsed": "0x1a8c4", "effectiveGasPrice": "%s", "from": "0x9709ae4129ed4bb3fa6678e83a9976b7cc81abd1", "gasUsed": "%s", "logs": [ { "address": "0xc3511006c04ef1d78af4c8e0e74ec18a6e64ff9e", "topics": [ "0x9dbb0e7dda3e09710ce75b801addc87cf9d9c6c581641b3275fca409ad086c62", "0x0000000000000000000000009709ae4129ed4bb3fa6678e83a9976b7cc81abd1", "0x06c20d147026151ea2785419a4070f32ad0f7884d18dd53d68477a58e556c753" ], "data": "0x00000000000000000000000000000000000000000000000002c68af0bb140000", "blockNumber": "0x712208", "transactionHash": "0xa515aea9c1b298c2947454902af1738af230030553943ba5cc738cbabfca9a4e", "transactionIndex": "0x0", "blockHash": "0xf76daa365606f130e620444e48512cca421318cfebc5b1152a5494c7ef6fe6fc", "logIndex": "0x0", "removed": false }, { "address": "0xde29d060d45901fb19ed6c6e959eb22d8626708e", "topics": [ "0x7d3450d4f5138e54dcb21a322312d50846ead7856426fb38778f8ef33aeccc01", "0x000000000000000000000000c3511006c04ef1d78af4c8e0e74ec18a6e64ff9e", "0x073314940630fd6dcda0d772d4c972c4e0a9946bef9dabf4ef84eda8ef542b82", "0x02d757788a8d8d6f21d1cd40bce38a8222d70654214e96ff95d8086e684fbee5" ], "data": "0x0000000000000000000000000000000000000000000000000000000000000040000000000000000000000000000000000000000000000000000000000002c0bb000000000000000000000000000000000000000000000000000000000000000306c20d147026151ea2785419a4070f32ad0f7884d18dd53d68477a58e556c75300000000000000000000000000000000000000000000000002c68af0bb1400000000000000000000000000000000000000000000000000000000000000000000", "blockNumber": "0x712208", "transactionHash": "0xa515aea9c1b298c2947454902af1738af230030553943ba5cc738cbabfca9a4e", "transactionIndex": "0x0", "blockHash": "0xf76daa365606f130e620444e48512cca421318cfebc5b1152a5494c7ef6fe6fc", "logIndex": "0x1", "removed": false } ], "logsBloom": "0x00000000000000000000000000000000002000000000000000000000000080040000002000000000001000001004000000000000001008100000000000000000000000000000000000000200000000000000000000002000000000040000000000000000020000000000000000000000000000000000000000000000000000000000000000800000000000000000000000001000022000000000000008000000000000000000000000000000000000000000000000200000000000000000000000000000008020000000000004000000000000000080000000000420000000000000000000000080000000000000000000000000000000000000000000000000", "status": "0x1", "to": "0xc3511006c04ef1d78af4c8e0e74ec18a6e64ff9e", "transactionHash": "0xa515aea9c1b298c2947454902af1738af230030553943ba5cc738cbabfca9a4e", "transactionIndex": "0x0", "type": "0x2" }, "id": 0 }]`, effectiveGasPrice, gasUsed))
			w.Write(d)
		}),
	)
	defer elServer.Close()

	// SetDebugLevel(1)
	day, _, err := Calculate(context.Background(), bnServer.URL, elServer.URL, "10", 1)
	if err != nil {
		t.Error(err)
	}

	t.Logf("%+v", *day)

	extraDepositsWei := decimal.NewFromInt(32e9).Mul(decimal.NewFromInt(1e9))
	endWei := decimal.NewFromInt(29 * 320032e5).Mul(decimal.NewFromInt(1e9)).Add(extraDepositsWei)
	startWei := decimal.NewFromInt(29 * 32e9).Mul(decimal.NewFromInt(1e9))
	consWei := endWei.Sub(startWei).Sub(extraDepositsWei)
	execWei := decimal.NewFromInt(29 * 10000 * 225).Mul(decimal.NewFromInt(1e9))
	eff := decimal.NewFromInt(29 * 32e9).Mul(decimal.NewFromInt(1e9))
	apr := decimal.NewFromInt(365).Mul(consWei.Add(execWei)).Div(eff)

	if day.Day.String() != "10" {
		t.Errorf("wrong Day: %v != %v", day.Day.String(), 10)
	}
	if !day.Apr.Equal(apr) {
		t.Errorf("wrong Apr: %v != %v", day.Apr, apr)
	}
	if day.Validators.IntPart() != 29 {
		t.Errorf("wrong Validators: %v != %v", day.Validators, 29)
	}
	if day.StartEpoch.IntPart() != 2250 {
		t.Errorf("wrong StartEpoch: %v != %v", day.StartEpoch, 2250)
	}
	if !day.StartBalanceGwei.Equal(startWei.Div(decimal.NewFromInt(1e9))) {
		t.Errorf("wrong StartBalanceGwei: %v != %v", day.StartBalanceGwei, startWei.Div(decimal.NewFromInt(1e9)))
	}
	if !day.EndBalanceGwei.Equal(endWei.Div(decimal.NewFromInt(1e9))) {
		t.Errorf("wrong EndBalanceGwei: %v != %v", day.EndBalanceGwei, endWei.Div(decimal.NewFromInt(1e9)))
	}
	if !day.DepositsSumGwei.Equal(extraDepositsWei.Div(decimal.NewFromInt(1e9))) {
		t.Errorf("wrong DepositsSumGwei: %v != %v", day.DepositsSumGwei, extraDepositsWei.Div(decimal.NewFromInt(1e9)))
	}
	if !day.ConsensusRewardsGwei.Equal(consWei.Div(decimal.NewFromInt(1e9))) {
		t.Errorf("wrong ConsensusRewardsGwei: %v != %v", day.ConsensusRewardsGwei, 92800000)
	}
	if !day.TxFeesSumWei.Equal(execWei) {
		t.Errorf("wrong TxFeesSumWei: %v != %v", day.TxFeesSumWei, execWei)
	}
	if !day.TotalRewardsWei.Equal(consWei.Add(execWei)) {
		t.Errorf("wrong TotalRewardsWei: %v != %v", day.TotalRewardsWei, consWei.Add(execWei))
	}
}

func createTx(feeGwei uint64) []byte {
	privateKey, err := crypto.HexToECDSA("fad9c8855b740a0b7ed4c221dbad0f33a83a49cad6b3fe8d5817ac83d38b6a19")
	if err != nil {
		log.Fatal(err)
	}
	value := big.NewInt(1e18)
	gasLimit := uint64(feeGwei)
	toAddress := common.HexToAddress("0x4592d8f8d7b001e72cb26a73e4fa1806a51ac79d")
	var data []byte
	tx := types.NewTransaction(1, toAddress, value, gasLimit, new(big.Int).SetInt64(1e9), data)
	chainID := new(big.Int).SetInt64(11155111)
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), privateKey)
	if err != nil {
		log.Fatal(err)
	}
	b, err := signedTx.MarshalBinary()
	if err != nil {
		log.Fatal(err)
	}
	return b
}
