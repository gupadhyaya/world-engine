package cardinal

import (
	"context"
	"errors"
	"testing"

	"pkg.world.dev/world-engine/evm/x/shard/types"
	"pkg.world.dev/world-engine/sign"
)

type DummyAdapter struct{}

func (d *DummyAdapter) Submit(_ context.Context, _ *sign.Transaction, _, _ uint64) error {
	return nil
}

func (d *DummyAdapter) QueryTransactions(_ context.Context, _ *types.QueryTransactionsRequest,
) (*types.QueryTransactionsResponse, error) {
	return nil, errors.New("this function should never get called")
}

func TestOptionFunctionSignatures(_ *testing.T) {
	// This test is designed to keep API compatibility. If a compile error happens here it means a function signature to
	// public facing functions was changed.
	WithAdapter(&DummyAdapter{})
	WithReceiptHistorySize(1)
	WithTickChannel(nil)
	WithTickDoneChannel(nil)
	WithStoreManager(nil)
	WithEventHub(nil)
	WithLoggingEventHub(nil)
	WithDisableSignatureVerification() //nolint:staticcheck //this test just looks for compile errors
}
