// Copyright (C) 2023, Berachain Foundation. All rights reserved.
// See the file LICENSE for licensing terms.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
// AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
// IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
// DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
// FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
// DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
// SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
// CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
// OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package state

import (
	"context"
	"math/big"

	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"

	ethstate "github.com/berachain/stargazer/eth/core/state"
	"github.com/berachain/stargazer/lib/common"
	"github.com/berachain/stargazer/lib/crypto"
	"github.com/berachain/stargazer/lib/snapshot"
	libtypes "github.com/berachain/stargazer/lib/types"
	"github.com/berachain/stargazer/store/snapmulti"
	"github.com/berachain/stargazer/x/evm/plugins/state/events"
)

const (
	pluginRegistryKey = `statePlugin`
	EvmNamespace      = `evm`
)

var (
	// EmptyCodeHash is the code hash of an empty code
	// 0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470.
	emptyCodeHash      = crypto.Keccak256Hash(nil)
	emptyCodeHashBytes = emptyCodeHash.Bytes()
)

// The StatePlugin is a very fun and interesting part of the EVM implementation. But if you want to
// join circus you need to know the rules. So here thet are:
//
//  1. You must ensure that the StatePlugin is only ever used in a single thread, because the
//     StatePlugin is not thread safe. And there are a bunch of optimizations made that are only
//     safe to do in a single thread.
//  2. When accessing or mutating the Plugin, you must ensure that the underlying account exists.
//     In the AccountKeeper, for performance reasons, this implementation of the StateDB will not
//     create accounts that do not exist. Notably calling `SetState()` on an account that does not
//     exist is completely possible, and the StateDB will not prevent you doing so. This lazy
//     creation improves performance a ton, as it prevents calling into the ak on
//     every SSTORE. The only accounts that should ever have `SetState()` called on them are
//     accounts that represent smart contracts. Because of this assumption, the only place that we
//     explicitly create accounts is in `CreateAccount()`, since `CreateAccount()` is called when
//     deploying a smart contract.
//  3. Accounts that are sent `evmDenom` coins during an eth transaction, will have an account
//     created for them, automatically by the Bank Module. However, these accounts will have a
//     codeHash of 0x000... This is because the Bank Module does not know that the account is an
//     EVM account, and so it does not set the codeHash. This is totally fine, we just need to
//     check both for both the codeHash being zero (0x000...) as well as the codeHash being empty
//     (0x567...)
type statePlugin struct {
	libtypes.Controller[string, libtypes.Controllable[string]]

	// We maintain a context in the StateDB, so that we can pass it with the correctly
	// configured multi-store to the precompiled contracts.
	ctx sdk.Context

	// Store a reference to the multi-store, in `ctx` so that we can access it directly.
	cms ControllableMultiStore

	// Store a reference to the Precompile Log Factory, which builds Eth logs from Cosmos events
	plf events.PrecompileLogFactory

	// Store the evm store key for quick lookups to the evm store
	evmStoreKey storetypes.StoreKey

	// keepers used for balance and account information.
	ak AccountKeeper
	bk BankKeeper

	// we load the evm denom in the constructor, to prevent going to
	// the params to get it mid interpolation.
	evmDenom string // TODO: get from params ( we have a store so like why not )
}

// returns a *statePlugin using the MultiStore belonging to ctx.
func NewPlugin(
	ctx sdk.Context,
	ak AccountKeeper,
	bk BankKeeper,
	evmStoreKey storetypes.StoreKey,
	evmDenom string,
) ethstate.Plugin {
	sp := &statePlugin{
		evmStoreKey: evmStoreKey,
		ak:          ak,
		bk:          bk,
		evmDenom:    evmDenom,
	}

	// TODO: setup the PrecompileLogFactory here? or higher up?
	sp.plf = nil

	// setup the Controllable MultiStore and EventManager and attach them to the context
	sp.cms = snapmulti.NewStoreFrom(ctx.MultiStore())
	cem := events.NewManagerFrom(ctx.EventManager(), sp.plf)
	sp.ctx = ctx.WithMultiStore(sp.cms).WithEventManager(cem)

	// setup the snapshot controller
	ctrl := snapshot.NewController[string, libtypes.Controllable[string]]()
	_ = ctrl.Register(sp.cms)
	_ = ctrl.Register(cem)
	sp.Controller = ctrl

	return sp
}

// `Reset` implements `ethstate.StatePlugin`.
func (sp *statePlugin) Reset(ctx context.Context) {
	// reset the Controllable MultiStore and EventManager and attach them to the context
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	sp.cms = snapmulti.NewStoreFrom(sdkCtx.MultiStore())
	cem := events.NewManagerFrom(sdkCtx.EventManager(), sp.plf)
	sp.ctx = sdkCtx.WithMultiStore(sp.cms).WithEventManager(cem)
}

// `RegistryKey` implements `libtypes.Registrable`.
func (sp *statePlugin) RegistryKey() string {
	return pluginRegistryKey
}

// ===========================================================================
// Account
// ===========================================================================

// CreateAccount implements the `StatePlugin` interface by creating a new account
// in the account keeper. It will allow accounts to be overridden.
func (sp *statePlugin) CreateAccount(addr common.Address) {
	acc := sp.ak.NewAccountWithAddress(sp.ctx, addr[:])

	// save the new account in the account keeper
	sp.ak.SetAccount(sp.ctx, acc)

	// initialize the code hash to empty
	sp.cms.GetKVStore(sp.evmStoreKey).Set(CodeHashKeyFor(addr), emptyCodeHashBytes)
}

// `Exist` implements the `StatePlugin` interface by reporting whether the given account address
// exists in the state. Notably this also returns true for suicided accounts, which is accounted
// for since, `RemoveAccount()` is not called until Commit.
func (sp *statePlugin) Exist(addr common.Address) bool {
	return sp.ak.HasAccount(sp.ctx, addr[:])
}

// =============================================================================
// Balance
// =============================================================================

// GetBalance implements `StatePlugin` interface.
func (sp *statePlugin) GetBalance(addr common.Address) *big.Int {
	// Note: bank keeper will return 0 if account/state_object is not found
	return sp.bk.GetBalance(sp.ctx, addr[:], sp.evmDenom).Amount.BigInt()
}

// AddBalance implements the `StatePlugin` interface by adding the given amount
// from the account associated with addr. If the account does not exist, it will be
// created.
func (sp *statePlugin) AddBalance(addr common.Address, amount *big.Int) {
	coins := sdk.NewCoins(sdk.NewCoin(sp.evmDenom, sdk.NewIntFromBigInt(amount)))

	// Mint the coins to the evm module account
	if err := sp.bk.MintCoins(sp.ctx, EvmNamespace, coins); err != nil {
		panic(err)
	}

	// Send the coins from the evm module account to the destination address.
	if err := sp.bk.SendCoinsFromModuleToAccount(
		sp.ctx, EvmNamespace, addr[:], coins,
	); err != nil {
		panic(err)
	}
}

// SubBalance implements the `StatePlugin` interface by subtracting the given amount
// from the account associated with addr.
func (sp *statePlugin) SubBalance(addr common.Address, amount *big.Int) {
	coins := sdk.NewCoins(sdk.NewCoin(sp.evmDenom, sdk.NewIntFromBigInt(amount)))

	// Send the coins from the source address to the evm module account.
	if err := sp.bk.SendCoinsFromAccountToModule(
		sp.ctx, addr[:], EvmNamespace, coins,
	); err != nil {
		panic(err)
	}

	// Burn the coins from the evm module account.
	if err := sp.bk.BurnCoins(sp.ctx, EvmNamespace, coins); err != nil {
		panic(err)
	}
}

// `TransferBalance` sends the given amount from one account to another. It will
// error if the sender does not have enough funds to send.
func (sp *statePlugin) TransferBalance(from, to common.Address, amount *big.Int) {
	coins := sdk.NewCoins(sdk.NewCoin(sp.evmDenom, sdk.NewIntFromBigInt(amount)))

	// Send the coins from the source address to the destination address.
	if err := sp.bk.SendCoins(sp.ctx, from[:], to[:], coins); err != nil {
		// This is safe to panic as the error is only returned if the sender does
		// not have enough funds to send, which should be guarded by `CanTransfer`.
		panic(err)
	}
}

// =============================================================================
// Nonce
// =============================================================================

// GetNonce implements the `StatePlugin` interface by returning the nonce
// of an account.
func (sp *statePlugin) GetNonce(addr common.Address) uint64 {
	acc := sp.ak.GetAccount(sp.ctx, addr[:])
	if acc == nil {
		return 0
	}
	return acc.GetSequence()
}

// SetNonce implements the `StatePlugin` interface by setting the nonce
// of an account.
func (sp *statePlugin) SetNonce(addr common.Address, nonce uint64) {
	// get the account or create a new one if doesn't exist
	acc := sp.ak.GetAccount(sp.ctx, addr[:])
	if acc == nil {
		acc = sp.ak.NewAccountWithAddress(sp.ctx, addr[:])
	}

	if err := acc.SetSequence(nonce); err != nil {
		panic(err)
	}

	sp.ak.SetAccount(sp.ctx, acc)
}

// =============================================================================
// Code
// =============================================================================

// GetCodeHash implements the `StatePlugin` interface by returning
// the code hash of account.
func (sp *statePlugin) GetCodeHash(addr common.Address) common.Hash {
	if !sp.ak.HasAccount(sp.ctx, addr[:]) {
		// if account at addr does not exist, return zeros
		return common.Hash{}
	}

	ch := sp.cms.GetKVStore(sp.evmStoreKey).Get(CodeHashKeyFor(addr))
	if ch == nil {
		// account exists but does not have a codehash, return empty
		return emptyCodeHash
	}

	return common.BytesToHash(ch)
}

// GetCode implements the `StatePlugin` interface by returning
// the code of account (nil if not exists).
func (sp *statePlugin) GetCode(addr common.Address) []byte {
	codeHash := sp.GetCodeHash(addr)
	if (codeHash == common.Hash{}) || codeHash == emptyCodeHash {
		// if account at addr does not exist or the account  does not have a codehash, return nil
		return nil
	}
	return sp.cms.GetKVStore(sp.evmStoreKey).Get(CodeKeyFor(codeHash))
}

// SetCode implements the `StatePlugin` interface by setting the code hash and
// code for the given account.
func (sp *statePlugin) SetCode(addr common.Address, code []byte) {
	codeHash := crypto.Keccak256Hash(code)
	ethStore := sp.cms.GetKVStore(sp.evmStoreKey)
	ethStore.Set(CodeHashKeyFor(addr), codeHash[:])

	// store or delete code
	if len(code) == 0 {
		ethStore.Delete(CodeKeyFor(codeHash))
	} else {
		ethStore.Set(CodeKeyFor(codeHash), code)
	}
}

// GetCodeSize implements the `StatePlugin` interface by returning the size of the
// code associated with the given `StatePlugin`.
func (sp *statePlugin) GetCodeSize(addr common.Address) int {
	return len(sp.GetCode(addr))
}

// =============================================================================
// State
// =============================================================================

// `GetCommittedState` implements the `StatePlugin` interface by returning the
// committed state of slot in the given address.
func (sp *statePlugin) GetCommittedState(
	addr common.Address,
	slot common.Hash,
) common.Hash {
	return sp.getStateFromStore(sp.cms.GetCommittedKVStore(sp.evmStoreKey), addr, slot)
}

// `GetState` implements the `StatePlugin` interface by returning the current state
// of slot in the given address.
func (sp *statePlugin) GetState(addr common.Address, slot common.Hash) common.Hash {
	return sp.getStateFromStore(sp.cms.GetKVStore(sp.evmStoreKey), addr, slot)
}

// `getStateFromStore` returns the current state of the slot in the given address.
func (sp *statePlugin) getStateFromStore(
	store storetypes.KVStore,
	addr common.Address, slot common.Hash,
) common.Hash {
	if value := store.Get(SlotKeyFor(addr, slot)); value != nil {
		return common.BytesToHash(value)
	}
	return common.Hash{}
}

// `SetState` sets the state of an address.
func (sp *statePlugin) SetState(addr common.Address, key, value common.Hash) {
	// For performance reasons, we don't check to ensure the account exists before we execute.
	// This is reasonably safe since under normal operation, SetState is only ever called by the
	// SSTORE opcode in the EVM, which will only ever be called on an account that exists, since
	// it would with 100% certainty have been created by a prior Create, thus setting its code
	// hash.
	//
	// CONTRACT: never manually call SetState outside of `opSstore`, or InitGenesis.

	// If empty value is given, delete the state entry.
	if len(value) == 0 || (value == common.Hash{}) {
		sp.cms.GetKVStore(sp.evmStoreKey).Delete(SlotKeyFor(addr, key))
		return
	}

	// Set the state entry.
	sp.cms.GetKVStore(sp.evmStoreKey).Set(SlotKeyFor(addr, key), value[:])
}

// =============================================================================
// ForEachStorage
// =============================================================================

// `ForEachStorage` implements the `StatePlugin` interface by iterating through the contract state
// contract storage, the iteration order is not defined.
//
// Note: We do not support iterating through any storage that is modified before calling
// `ForEachStorage`; only committed state is iterated through.
func (sp *statePlugin) ForEachStorage(
	addr common.Address,
	cb func(key, value common.Hash) bool,
) error {
	it := sdk.KVStorePrefixIterator(
		sp.cms.GetKVStore(sp.evmStoreKey),
		StorageKeyFor(addr),
	)
	defer it.Close()

	for ; it.Valid(); it.Next() {
		committedValue := it.Value()
		if len(committedValue) > 0 {
			if !cb(common.BytesToHash(it.Key()), common.BytesToHash(committedValue)) {
				return nil // stop iteration
			}
		}
	}

	return nil
}

// `DeleteSuicides` manually deletes the given suicidal accounts.
func (sp *statePlugin) DeleteSuicides(suicides []common.Address) {
	for _, suicidalAddr := range suicides {
		acct := sp.ak.GetAccount(sp.ctx, suicidalAddr[:])
		if acct == nil {
			// handles the double suicide case
			continue
		}

		// clear storage
		_ = sp.ForEachStorage(suicidalAddr,
			func(key, _ common.Hash) bool {
				sp.SetState(suicidalAddr, key, common.Hash{})
				return true
			})

		// clear the codehash from this account
		sp.cms.GetKVStore(sp.evmStoreKey).Delete(CodeHashKeyFor(suicidalAddr))

		// remove auth account
		sp.ak.RemoveAccount(sp.ctx, acct)
	}
}