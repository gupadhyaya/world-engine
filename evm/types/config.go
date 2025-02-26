// SPDX-License-Identifier: BUSL-1.1
//
// Copyright (C) 2023, Berachain Foundation. All rights reserved.
// Use of this software is govered by the Business Source License included
// in the LICENSE file of this repository and at www.mariadb.com/bsl11.
//
// ANY USE OF THE LICENSED WORK IN VIOLATION OF THIS LICENSE WILL AUTOMATICALLY
// TERMINATE YOUR RIGHTS UNDER THIS LICENSE FOR THE CURRENT AND ALL OTHER
// VERSIONS OF THE LICENSED WORK.
//
// THIS LICENSE DOES NOT GRANT YOU ANY RIGHT IN ANY TRADEMARK OR LOGO OF
// LICENSOR OR ITS AFFILIATES (PROVIDED THAT YOU MAY USE A TRADEMARK OR LOGO OF
// LICENSOR AS EXPRESSLY REQUIRED BY THIS LICENSE).
//
// TO THE EXTENT PERMITTED BY APPLICABLE LAW, THE LICENSED WORK IS PROVIDED ON
// AN “AS IS” BASIS. LICENSOR HEREBY DISCLAIMS ALL WARRANTIES AND CONDITIONS,
// EXPRESS OR IMPLIED, INCLUDING (WITHOUT LIMITATION) WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, NON-INFRINGEMENT, AND
// TITLE.

package types

import (
	"sync"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"pkg.berachain.dev/polaris/eth/accounts"
)

var (
	// Bech32PrefixAccAddr defines the Bech32 prefix of an account's address.
	Bech32PrefixAccAddr = func(p string) string { return p }
	// Bech32PrefixAccPub defines the Bech32 prefix of an account's public key.
	Bech32PrefixAccPub = func(p string) string { return p + sdk.PrefixPublic }
	// Bech32PrefixValAddr defines the Bech32 prefix of a validator's operator address.
	Bech32PrefixValAddr = func(p string) string { return p + sdk.PrefixValidator + sdk.PrefixOperator }
	// Bech32PrefixValPub defines the Bech32 prefix of a validator's operator public key.
	Bech32PrefixValPub = func(p string) string { return p + sdk.PrefixValidator + sdk.PrefixOperator + sdk.PrefixPublic }
	// Bech32PrefixConsAddr defines the Bech32 prefix of a consensus node address.
	Bech32PrefixConsAddr = func(p string) string { return p + sdk.PrefixValidator + sdk.PrefixConsensus }
	// Bech32PrefixConsPub defines the Bech32 prefix of a consensus node public key.
	Bech32PrefixConsPub = func(p string) string { return p + sdk.PrefixValidator + sdk.PrefixConsensus + sdk.PrefixPublic }
)

var initConfig sync.Once

// SetupCosmosConfig sets up the Cosmos SDK configuration to be compatible with the semantics of etheruem.
func SetupCosmosConfig(bech32Prefix string) {
	initConfig.Do(func() {
		// set the address prefixes
		sdkCfg := sdk.GetConfig()
		SetBech32Prefixes(sdkCfg, bech32Prefix)
		SetBip44CoinType(sdkCfg)
		sdkCfg.Seal()
	})
}

// SetBech32Prefixes sets the global prefixes to be used when serializing addresses and public keys to Bech32 strings.
func SetBech32Prefixes(config *sdk.Config, p string) {
	config.SetBech32PrefixForAccount(Bech32PrefixAccAddr(p), Bech32PrefixAccPub(p))
	config.SetBech32PrefixForValidator(Bech32PrefixValAddr(p), Bech32PrefixValPub(p))
	config.SetBech32PrefixForConsensusNode(Bech32PrefixConsAddr(p), Bech32PrefixConsPub(p))
}

// SetBip44CoinType sets the global coin type to be used in hierarchical deterministic wallets.
func SetBip44CoinType(config *sdk.Config) {
	config.SetCoinType(accounts.Bip44CoinType)
	config.SetPurpose(sdk.Purpose)
}
