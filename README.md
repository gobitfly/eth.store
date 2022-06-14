# ETH.STORE

The Transparent Ethereum Staking Reward Reference Rate represents the average rate of return per effectively staked Ether on Ethereum on a given day.

## usage

```bash
# build and install binary from source via go
go install github.com/gobitfly/eth.store
eth.store -api.address="http://some-beacon-node:4000" -days="497-499"
day: 497 (epoch 111825, 2022-04-12 12:00:23 +0000 UTC), network.apr: 0.04908389
day: 498 (epoch 112050, 2022-04-13 12:00:23 +0000 UTC), network.apr: 0.04901101
day: 499 (epoch 112275, 2022-04-14 12:00:23 +0000 UTC), network.apr: 0.04889888

# build and run docker-image
git clone github.com/gobitfly/eth.store
cd eth.store
docker build . -t eth.store
docker run --network=host eth.store -api.address="http://some-beacon-node:4000" -days="1,10"
{"day":1,"dayTime":"2020-12-02T12:00:23Z","apr":"0.179092080939544","validators":21786,"startEpoch":225,"effectiveBalance":697152000000000,"startBalance":697605887541204,"endBalance":697979954397125,"depositsSum":32000000000,"validatorSets":{}}
{"day":10,"dayTime":"2020-12-11T12:00:23Z","apr":"0.1622832991187628","validators":29871,"startEpoch":2250,"effectiveBalance":955872000000000,"startBalance":960110038369385,"endBalance":960535030319235,"depositsSum":0,"validatorSets":{}}

# use pre-built docker-image
docker run --network=host gobitfly/eth.store:latest -api.address="http://some-beacon-node:4000" -days="finalized" -validators="myValidators:400000,400001;otherValidators:1,2,3"
day: 559 (epoch 125775, 2022-06-13 12:00:23 +0000 UTC), network.apr: 0.04504914, myValidators.apr: 0.03851903 (0.86 vs network), otherValidators.apr: 0.03842060 (0.85 vs network)
```
