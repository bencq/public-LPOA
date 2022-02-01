# Lock-based Proof of Authority (LPOA)

LPOA is an improvement for POA. The main idea of LPOA is to make an agreement on which node proposes a new block, which serves as a pre-consensus process. The pre-consensus process is low-cost, and it significantly reduces the forking rate of POA, especially facing great stress.

## Introduction

This repository is an implementation of LPOA based on [go-ethereum](https://github.com/ethereum/go-ethereum) Clique. The commit level a3f0da1ac42cf78769921ebf6974e4e4c6a197ae of go-ethereum is used as the base source code.
## Building the source

This is a copy from go-ethereum.

For prerequisites please read the [Installation Instructions](https://geth.ethereum.org/docs/install-and-build/installing-geth).

After you have done with the requires, simply run

```shell
make geth
```

And then you can find the LPoA geth binary in `build/`

## Launch

To launch a LPOA node, run

```bash
./geth --datadir data --port ${PORT} --nodiscover --syncmode full --networkid 666666 --ws --ws.addr 0.0.0.0 --ws.api web3,eth --ws.origins \"*\" --allow-insecure-unlock --verbosity ${verbosity} --endpoints \"${endpoints}\" --endpointIndex ${i} --etherbases \"${etherbases}\" --overtime ${overtime} &>test${i}.log

# 'endpoints','endpointIndex', 'etherbases' and 'overtime' are newly added flags used in LPOA
# 'endpoints' is the endpoints of the nodes in the network seperated by commas. For example: "172.16.16.10:40303,172.16.16.39:40303"...
# 'endpointIndex' is the endpoint index of the current node. For example: "0"
# 'etherbases' is the etherbases of the nodes in the network seperated by commas. For exmpale: "0x68b0833C2cA1462E11048D09F3d7c9AfB05ccA42,0xC7dEB8a903CEC7e6e397962757447801D59e2d9d"...
# 'overtime' is the time period for triggering overtime, and it is represented in milliseconds. For example: "10000"
```




To build a multi-machine environment network like those in the paper, please refer to [Deployment Code](#Deployment).



## Deployment Code<span id = "Deployment"></span>

For the deployment and testing code, please refer to [https://github.com/bencq/codesLPOA](https://github.com/bencq/codesLPOA)

