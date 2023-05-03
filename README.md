![ethstore](https://user-images.githubusercontent.com/26490734/235898840-ffba747a-69ac-4750-8517-c0b3ffcb8459.png)


ETH.STORE (Ether Staking Offered Rate) represents the average financial return validators on the Ethereum network have achieved in a 24-hour period.

## usage

```bash
# build and install binary from source via go
go install github.com/gobitfly/eth.store

eth.store -h
Usage of /bin/eth.store:
  -cons.address string
    	address of the conensus-node-api (default "http://localhost:4000")
  -cons.timeout duration
    	timeout duration for the consensus-node-api (default 2m0s)
  -days string
    	days to calculate eth.store for, format: "1-3" or "1,4,6"
  -debug uint
    	set debug-level (higher level will increase verbosity)
  -exec.address string
    	address of the execution-node-api (default "http://localhost:4000")
  -exec.timeout duration
    	timeout duration for the execution-node-api (default 2m0s)
  -json
    	format output as json
  -json.file string
    	path to file to write results into, only missing days will be added
  -version
    	print version and exit


eth.store -cons.address="http://some-consensus-node:4000" -exec.address="http://some-execution-node:8545" -days="497-499"
day: 497 (2022-04-12 12:00:23 +0000 UTC), epochs: 111825-112049, validators: 341373, apr: 0.049083890, effectiveBalanceSumGwei: 10923834000000000, totalRewardsSumWei: 1468997980817000000000, consensusRewardsGwei: 1468997980817 (100%), txFeesSumWei: 0
day: 498 (2022-04-13 12:00:23 +0000 UTC), epochs: 112050-112274, validators: 342498, apr: 0.049011013, effectiveBalanceSumGwei: 10959834000000000, totalRewardsSumWei: 1471650879693000000000, consensusRewardsGwei: 1471650879693 (100%), txFeesSumWei: 0
day: 499 (2022-04-14 12:00:23 +0000 UTC), epochs: 112275-112499, validators: 343623, apr: 0.048898885, effectiveBalanceSumGwei: 10995834000000000, totalRewardsSumWei: 1473106903824000000000, consensusRewardsGwei: 1473106903824 (100%), txFeesSumWei: 0

# build and run docker-image and output json
git clone github.com/gobitfly/eth.store
cd eth.store
docker build . -t eth.store
docker run --network=host eth.store -cons.address="http://some-consensus-node:4000" -exec.address="http://some-execution-node:8545" -days="0,10" -json
[
	{
		"day": "0",
		"dayTime": "2020-12-01T12:00:23Z",
		"apr": "0.1740251707100836",
		"validators": "21062",
		"startEpoch": "0",
		"effectiveBalanceGwei": "673984000000000",
		"startBalanceGwei": "674112000000000",
		"endBalanceGwei": "674433342960701",
		"depositsSumGwei": "0",
		"consensusRewardsGwei": "321342960701",
		"txFeesSumWei": "0",
		"totalRewardsWei": "321342960701000000000"
	},
	{
		"day": "10",
		"dayTime": "2020-12-11T12:00:23Z",
		"apr": "0.1622832991187628",
		"validators": "29871",
		"startEpoch": "2250",
		"effectiveBalanceGwei": "955872000000000",
		"startBalanceGwei": "960110038369385",
		"endBalanceGwei": "960535030319235",
		"depositsSumGwei": "0",
		"consensusRewardsGwei": "424991949850",
		"txFeesSumWei": "0",
		"totalRewardsWei": "424991949850000000000"
	}
]

# use pre-built docker-image and write into json-file
docker run --network=host gobitfly/eth.store:latest -cons.address="http://some-consensus-node:4000" -exec.address="http://some-execution-node:8545" -days="613" -json.file="./ethstore.json"
day: 613 (2022-08-06 12:00:23 +0000 UTC), epochs: 137925-138149, validators: 412063, apr: 0.044632337, effectiveBalanceSumGwei: 13185905000000000, totalRewardsSumWei: 1612377406889000000000, consensusRewardsGwei: 1612377406889 (100%), txFeesSumWei: 0
cat ./etsthore.json
[
	{
		"day": "613",
		"dayTime": "2022-08-06T12:00:23Z",
		"apr": "0.0446323368410803",
		"validators": "412063",
		"startEpoch": "137925",
		"effectiveBalanceGwei": "13185905000000000",
		"startBalanceGwei": "13899169115750451",
		"endBalanceGwei": "13900781493157340",
		"depositsSumGwei": "0",
		"consensusRewardsGwei": "1612377406889",
		"txFeesSumWei": "0",
		"totalRewardsWei": "1612377406889000000000"
	}
]
```
