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

## Deployment Code

For the deployment and testing code, please refer to [https://github.com/bencq/codesLPOA](https://github.com/bencq/codesLPOA)

