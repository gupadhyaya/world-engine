package message_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pkg.world.dev/world-engine/assert"
	"pkg.world.dev/world-engine/cardinal/txpool"

	"pkg.world.dev/world-engine/cardinal/ecs"
	"pkg.world.dev/world-engine/cardinal/testutils"
	"pkg.world.dev/world-engine/cardinal/types/entity"
	"pkg.world.dev/world-engine/sign"
)

type ScoreComponent struct {
	Score int
}

func (ScoreComponent) Name() string {
	return "score"
}

type ModifyScoreMsg struct {
	PlayerID entity.ID
	Amount   int
}

type EmptyMsgResult struct{}

func TestReadTypeNotStructs(t *testing.T) {
	defer func() {
		// test should trigger a panic. it is swallowed here.
		panicValue := recover()
		assert.Assert(t, panicValue != nil)

		defer func() {
			// deferred function should not fail
			panicValue = recover()
			assert.Assert(t, panicValue == nil)
		}()

		ecs.NewMessageType[*ModifyScoreMsg, *EmptyMsgResult]("modify_score2")
	}()
	ecs.NewMessageType[string, string]("modify_score1")
}

func TestCanQueueTransactions(t *testing.T) {
	world := testutils.NewTestWorld(t).Instance()

	// Create an entity with a score component
	assert.NilError(t, ecs.RegisterComponent[ScoreComponent](world))
	modifyScoreMsg := ecs.NewMessageType[*ModifyScoreMsg, *EmptyMsgResult]("modify_score")
	assert.NilError(t, world.RegisterMessages(modifyScoreMsg))

	wCtx := ecs.NewWorldContext(world)

	// Set up a system that allows for the modification of a player's score
	world.RegisterSystem(
		func(wCtx ecs.WorldContext) error {
			modifyScore := modifyScoreMsg.In(wCtx)
			for _, txData := range modifyScore {
				ms := txData.Msg
				err := ecs.UpdateComponent[ScoreComponent](
					wCtx, ms.PlayerID, func(s *ScoreComponent) *ScoreComponent {
						s.Score += ms.Amount
						return s
					},
				)
				if err != nil {
					return err
				}
			}
			return nil
		},
	)
	assert.NilError(t, world.LoadGameState())
	id, err := ecs.Create(wCtx, ScoreComponent{})
	assert.NilError(t, err)

	modifyScoreMsg.AddToQueue(world, &ModifyScoreMsg{id, 100})

	assert.NilError(t, ecs.SetComponent[ScoreComponent](wCtx, id, &ScoreComponent{}))

	// Verify the score is 0
	s, err := ecs.GetComponent[ScoreComponent](wCtx, id)
	assert.NilError(t, err)
	assert.Equal(t, 0, s.Score)

	// Process a game tick
	assert.NilError(t, world.Tick(context.Background()))

	// Verify the score was updated
	s, err = ecs.GetComponent[ScoreComponent](wCtx, id)
	assert.NilError(t, err)
	assert.Equal(t, 100, s.Score)

	// Tick again, but no new modifyScoreMsg was added to the queue
	assert.NilError(t, world.Tick(context.Background()))

	// Verify the score hasn't changed
	s, err = ecs.GetComponent[ScoreComponent](wCtx, id)
	assert.NilError(t, err)
	assert.Equal(t, 100, s.Score)
}

type CounterComponent struct {
	Count int
}

func (CounterComponent) Name() string {
	return "count"
}

func TestSystemsAreExecutedDuringGameTick(t *testing.T) {
	world := testutils.NewTestWorld(t).Instance()

	assert.NilError(t, ecs.RegisterComponent[CounterComponent](world))

	wCtx := ecs.NewWorldContext(world)

	world.RegisterSystem(
		func(wCtx ecs.WorldContext) error {
			search, err := wCtx.NewSearch(ecs.Exact(CounterComponent{}))
			assert.NilError(t, err)
			id := search.MustFirst(wCtx)
			return ecs.UpdateComponent[CounterComponent](
				wCtx, id, func(c *CounterComponent) *CounterComponent {
					c.Count++
					return c
				},
			)
		},
	)
	assert.NilError(t, world.LoadGameState())
	id, err := ecs.Create(wCtx, CounterComponent{})
	assert.NilError(t, err)

	for i := 0; i < 10; i++ {
		assert.NilError(t, world.Tick(context.Background()))
	}

	c, err := ecs.GetComponent[CounterComponent](wCtx, id)
	assert.NilError(t, err)
	assert.Equal(t, 10, c.Count)
}

func TestTransactionAreAppliedToSomeEntities(t *testing.T) {
	world := testutils.NewTestWorld(t).Instance()
	assert.NilError(t, ecs.RegisterComponent[ScoreComponent](world))

	modifyScoreMsg := ecs.NewMessageType[*ModifyScoreMsg, *EmptyMsgResult]("modify_score")
	assert.NilError(t, world.RegisterMessages(modifyScoreMsg))

	world.RegisterSystem(
		func(wCtx ecs.WorldContext) error {
			modifyScores := modifyScoreMsg.In(wCtx)
			for _, msData := range modifyScores {
				ms := msData.Msg
				err := ecs.UpdateComponent[ScoreComponent](
					wCtx, ms.PlayerID, func(s *ScoreComponent) *ScoreComponent {
						s.Score += ms.Amount
						return s
					},
				)
				assert.Check(t, err == nil)
			}
			return nil
		},
	)
	assert.NilError(t, world.LoadGameState())

	wCtx := ecs.NewWorldContext(world)
	ids, err := ecs.CreateMany(wCtx, 100, ScoreComponent{})
	assert.NilError(t, err)
	// Entities at index 5, 10 and 50 will be updated with some values
	modifyScoreMsg.AddToQueue(
		world, &ModifyScoreMsg{
			PlayerID: ids[5],
			Amount:   105,
		},
	)
	modifyScoreMsg.AddToQueue(
		world, &ModifyScoreMsg{
			PlayerID: ids[10],
			Amount:   110,
		},
	)
	modifyScoreMsg.AddToQueue(
		world, &ModifyScoreMsg{
			PlayerID: ids[50],
			Amount:   150,
		},
	)

	assert.NilError(t, world.Tick(context.Background()))

	for i, id := range ids {
		wantScore := 0
		switch i {
		case 5:
			wantScore = 105
		case 10:
			wantScore = 110
		case 50:
			wantScore = 150
		}
		s, err := ecs.GetComponent[ScoreComponent](wCtx, id)
		assert.NilError(t, err)
		assert.Equal(t, wantScore, s.Score)
	}
}

// TestAddToQueueDuringTickDoesNotTimeout verifies that we can add a transaction to the transaction
// queue during a game tick, and the call does not block.
func TestAddToQueueDuringTickDoesNotTimeout(t *testing.T) {
	world := testutils.NewTestWorld(t).Instance()

	modScore := ecs.NewMessageType[*ModifyScoreMsg, *EmptyMsgResult]("modify_Score")
	assert.NilError(t, world.RegisterMessages(modScore))

	inSystemCh := make(chan struct{})
	// This system will block forever. This will give us a never-ending game tick that we can use
	// to verify that the addition of more transactions doesn't block.
	world.RegisterSystem(
		func(ecs.WorldContext) error {
			<-inSystemCh
			select {}
		},
	)
	assert.NilError(t, world.LoadGameState())

	modScore.AddToQueue(world, &ModifyScoreMsg{})

	// Start a tick in the background.
	go func() {
		assert.Check(t, nil == world.Tick(context.Background()))
	}()
	// Make sure we're actually in the System. It will now block forever.
	inSystemCh <- struct{}{}

	// Make sure we can call AddToQueue again in a reasonable amount of time
	timeout := time.After(500 * time.Millisecond)
	doneWithAddToQueue := make(chan struct{})
	go func() {
		modScore.AddToQueue(world, &ModifyScoreMsg{})
		doneWithAddToQueue <- struct{}{}
	}()

	select {
	case <-doneWithAddToQueue:
	// happy path
	case <-timeout:
		t.Fatal("timeout while trying to AddToQueue")
	}
}

// TestTransactionsAreExecutedAtNextTick verifies that while a game tick is taking place, new transactions
// are added to some queue that is not processed until the NEXT tick.
func TestTransactionsAreExecutedAtNextTick(t *testing.T) {
	world := testutils.NewTestWorld(t).Instance()
	modScoreMsg := ecs.NewMessageType[*ModifyScoreMsg, *EmptyMsgResult]("modify_score")
	assert.NilError(t, world.RegisterMessages(modScoreMsg))
	ctx := context.Background()
	tickStart := make(chan time.Time)
	tickDone := make(chan uint64)
	world.StartGameLoop(ctx, tickStart, tickDone)

	modScoreCountCh := make(chan int)

	// Create two system that report how many instances of the ModifyScoreMsg exist in the
	// transaction queue. These counts should be the same for each tick. modScoreCountCh is an unbuffered channel
	// so these systems will block while writing to modScoreCountCh. This allows the test to ensure that we can run
	// commands mid-tick.
	world.RegisterSystem(
		func(wCtx ecs.WorldContext) error {
			modScores := modScoreMsg.In(wCtx)
			modScoreCountCh <- len(modScores)
			return nil
		},
	)

	world.RegisterSystem(
		func(wCtx ecs.WorldContext) error {
			modScores := modScoreMsg.In(wCtx)
			modScoreCountCh <- len(modScores)
			return nil
		},
	)
	assert.NilError(t, world.LoadGameState())

	modScoreMsg.AddToQueue(world, &ModifyScoreMsg{})

	// Start the game tick. The tick will block while waiting to write to modScoreCountCh
	tickStart <- time.Now()

	// In the first system, we should see 1 modify score transaction
	count := <-modScoreCountCh
	assert.Equal(t, 1, count)

	// Add two transactions mid-tick.
	modScoreMsg.AddToQueue(world, &ModifyScoreMsg{})
	modScoreMsg.AddToQueue(world, &ModifyScoreMsg{})

	// The tick is still not over, so we should still only see 1 modify score transaction
	count = <-modScoreCountCh
	assert.Equal(t, 1, count)

	// Block until the tick has completed.
	<-tickDone

	// Start the next tick.
	tickStart <- time.Now()

	// This second tick shold find 2 ModifyScore transactions. They were added in the middle of the previous tick.
	count = <-modScoreCountCh
	assert.Equal(t, 2, count)
	count = <-modScoreCountCh
	assert.Equal(t, 2, count)

	// Block until the tick has completed.
	<-tickDone

	// In this final tick, we should see no modify score transactions
	tickStart <- time.Now()
	count = <-modScoreCountCh
	assert.Equal(t, 0, count)
	count = <-modScoreCountCh
	assert.Equal(t, 0, count)
	<-tickDone
}

// TestIdenticallyTypedTransactionCanBeDistinguished verifies that two transactions of the same type
// can be distinguished if they were added with different MessageType[T]s.
func TestIdenticallyTypedTransactionCanBeDistinguished(t *testing.T) {
	world := testutils.NewTestWorld(t).Instance()
	type NewOwner struct {
		Name string
	}

	alpha := ecs.NewMessageType[NewOwner, EmptyMsgResult]("alpha_msg")
	beta := ecs.NewMessageType[NewOwner, EmptyMsgResult]("beta_msg")
	assert.NilError(t, world.RegisterMessages(alpha, beta))

	alpha.AddToQueue(world, NewOwner{"alpha"})
	beta.AddToQueue(world, NewOwner{"beta"})

	world.RegisterSystem(
		func(wCtx ecs.WorldContext) error {
			newNames := alpha.In(wCtx)
			assert.Check(t, len(newNames) == 1, "expected 1 transaction, not %d", len(newNames))
			assert.Check(t, newNames[0].Msg.Name == "alpha")

			newNames = beta.In(wCtx)
			assert.Check(t, len(newNames) == 1, "expected 1 transaction, not %d", len(newNames))
			assert.Check(t, newNames[0].Msg.Name == "beta")
			return nil
		},
	)
	assert.NilError(t, world.LoadGameState())

	assert.NilError(t, world.Tick(context.Background()))
}

func TestCannotRegisterDuplicateTransaction(t *testing.T) {
	msg := ecs.NewMessageType[ModifyScoreMsg, EmptyMsgResult]("modify_score")
	world := testutils.NewTestWorld(t).Instance()
	assert.Check(t, nil != world.RegisterMessages(msg, msg))
}

func TestCannotCallRegisterTransactionsMultipleTimes(t *testing.T) {
	msg := ecs.NewMessageType[ModifyScoreMsg, EmptyMsgResult]("modify_score")
	world := testutils.NewTestWorld(t).Instance()
	assert.NilError(t, world.RegisterMessages(msg))
	assert.Check(t, nil != world.RegisterMessages(msg))
}

func TestCanEncodeDecodeEVMTransactions(t *testing.T) {
	// the msg we are going to test against
	type FooMsg struct {
		X, Y uint64
		Name string
	}

	msg := FooMsg{1, 2, "foo"}
	// set up the Message.
	iMsg := ecs.NewMessageType[FooMsg, EmptyMsgResult]("FooMsg", ecs.WithMsgEVMSupport[FooMsg, EmptyMsgResult])
	bz, err := iMsg.ABIEncode(msg)
	assert.NilError(t, err)

	// decode the evm bytes
	fooMsg, err := iMsg.DecodeEVMBytes(bz)
	assert.NilError(t, err)

	// we should be able to cast back to our concrete Go struct.
	f, ok := fooMsg.(FooMsg)
	assert.Equal(t, ok, true)
	assert.DeepEqual(t, f, msg)
}

func TestCannotDecodeEVMBeforeSetEVM(t *testing.T) {
	type foo struct{}
	msg := ecs.NewMessageType[foo, EmptyMsgResult]("foo")
	_, err := msg.DecodeEVMBytes([]byte{})
	assert.ErrorIs(t, err, ecs.ErrEVMTypeNotSet)
}

func TestCannotHaveDuplicateTransactionNames(t *testing.T) {
	type SomeMsg struct {
		X, Y, Z int
	}
	type OtherMsg struct {
		Alpha, Beta string
	}
	world := testutils.NewTestWorld(t).Instance()
	alphaMsg := ecs.NewMessageType[SomeMsg, EmptyMsgResult]("name_match")
	betaMsg := ecs.NewMessageType[OtherMsg, EmptyMsgResult]("name_match")
	assert.ErrorIs(t, world.RegisterMessages(alphaMsg, betaMsg), ecs.ErrDuplicateMessageName)
}

func TestCanGetTransactionErrorsAndResults(t *testing.T) {
	type MoveMsg struct {
		DeltaX, DeltaY int
	}
	type MoveMsgResult struct {
		EndX, EndY int
	}
	world := testutils.NewTestWorld(t).Instance()

	// Each transaction now needs an input and an output
	moveMsg := ecs.NewMessageType[MoveMsg, MoveMsgResult]("move")
	assert.NilError(t, world.RegisterMessages(moveMsg))

	wantFirstError := errors.New("this is a transaction error")
	wantSecondError := errors.New("another transaction error")
	wantDeltaX, wantDeltaY := 99, 100

	world.RegisterSystem(
		func(wCtx ecs.WorldContext) error {
			// This new In function returns a triplet of information:
			// 1) The transaction input
			// 2) An ID that uniquely identifies this specific transaction
			// 3) The signature
			// This function would replace both "In" and "TxsAndSigsIn"
			txData := moveMsg.In(wCtx)
			assert.Equal(t, 1, len(txData), "expected 1 move transaction")
			tx := txData[0]
			// The input for the transaction is found at tx.Val
			assert.Equal(t, wantDeltaX, tx.Msg.DeltaX)
			assert.Equal(t, wantDeltaY, tx.Msg.DeltaY)

			// AddError will associate an error with the tx.TxHash. Multiple errors can be
			// associated with a transaction.
			moveMsg.AddError(wCtx, tx.Hash, wantFirstError)
			moveMsg.AddError(wCtx, tx.Hash, wantSecondError)

			// SetResult sets the output for the transaction. Only one output can be set
			// for a tx.TxHash (the last assigned result will clobber other results)
			moveMsg.SetResult(wCtx, tx.Hash, MoveMsgResult{42, 42})
			return nil
		},
	)
	assert.NilError(t, world.LoadGameState())
	_ = moveMsg.AddToQueue(world, MoveMsg{99, 100})

	// Tick the game so the transaction is processed
	assert.NilError(t, world.Tick(context.Background()))

	tick := world.CurrentTick() - 1
	receipts, err := world.GetTransactionReceiptsForTick(tick)
	assert.NilError(t, err)
	assert.Equal(t, 1, len(receipts))
	r := receipts[0]
	assert.Equal(t, 2, len(r.Errs))
	assert.ErrorIs(t, wantFirstError, r.Errs[0])
	assert.ErrorIs(t, wantSecondError, r.Errs[1])
	got, ok := r.Result.(MoveMsgResult)
	assert.Check(t, ok)
	assert.Equal(t, MoveMsgResult{42, 42}, got)
}

func TestSystemCanFindErrorsFromEarlierSystem(t *testing.T) {
	type MsgIn struct {
		Number int
	}
	type MsgOut struct {
		Number int
	}
	world := testutils.NewTestWorld(t).Instance()
	numTx := ecs.NewMessageType[MsgIn, MsgOut]("number")
	assert.NilError(t, world.RegisterMessages(numTx))
	wantErr := errors.New("some transaction error")
	systemCalls := 0
	world.RegisterSystem(
		func(wCtx ecs.WorldContext) error {
			systemCalls++
			txs := numTx.In(wCtx)
			assert.Equal(t, 1, len(txs))
			hash := txs[0].Hash
			_, _, ok := numTx.GetReceipt(wCtx, hash)
			assert.Check(t, !ok)
			numTx.AddError(wCtx, hash, wantErr)
			return nil
		},
	)

	world.RegisterSystem(
		func(wCtx ecs.WorldContext) error {
			systemCalls++
			txs := numTx.In(wCtx)
			assert.Equal(t, 1, len(txs))
			hash := txs[0].Hash
			_, errs, ok := numTx.GetReceipt(wCtx, hash)
			assert.Check(t, ok)
			assert.Equal(t, 1, len(errs))
			assert.ErrorIs(t, wantErr, errs[0])
			return nil
		},
	)
	assert.NilError(t, world.LoadGameState())

	_ = numTx.AddToQueue(world, MsgIn{100})

	assert.NilError(t, world.Tick(context.Background()))
	assert.Equal(t, 2, systemCalls)
}

func TestSystemCanClobberTransactionResult(t *testing.T) {
	type MsgIn struct {
		Number int
	}
	type MsgOut struct {
		Number int
	}
	world := testutils.NewTestWorld(t).Instance()
	numTx := ecs.NewMessageType[MsgIn, MsgOut]("number")
	assert.NilError(t, world.RegisterMessages(numTx))
	systemCalls := 0

	firstResult := MsgOut{1234}
	secondResult := MsgOut{5678}
	world.RegisterSystem(
		func(wCtx ecs.WorldContext) error {
			systemCalls++
			txs := numTx.In(wCtx)
			assert.Equal(t, 1, len(txs))
			hash := txs[0].Hash
			_, _, ok := numTx.GetReceipt(wCtx, hash)
			assert.Check(t, !ok)
			numTx.SetResult(wCtx, hash, firstResult)
			return nil
		},
	)

	world.RegisterSystem(
		func(wCtx ecs.WorldContext) error {
			systemCalls++
			txs := numTx.In(wCtx)
			assert.Equal(t, 1, len(txs))
			hash := txs[0].Hash
			out, errs, ok := numTx.GetReceipt(wCtx, hash)
			assert.Check(t, ok)
			assert.Equal(t, 0, len(errs))
			assert.Equal(t, MsgOut{1234}, out)
			numTx.SetResult(wCtx, hash, secondResult)
			return nil
		},
	)
	assert.NilError(t, world.LoadGameState())

	_ = numTx.AddToQueue(world, MsgIn{100})

	assert.NilError(t, world.Tick(context.Background()))

	prevTick := world.CurrentTick() - 1
	receipts, err := world.GetTransactionReceiptsForTick(prevTick)
	assert.NilError(t, err)
	assert.Equal(t, 1, len(receipts))
	r := receipts[0]
	assert.Equal(t, 0, len(r.Errs))
	gotResult, ok := r.Result.(MsgOut)
	assert.Check(t, ok)
	assert.Equal(t, secondResult, gotResult)
}

func TestCopyTransactions(t *testing.T) {
	type FooMsg struct {
		X int
	}
	txq := txpool.NewTxQueue()
	txq.AddTransaction(1, FooMsg{X: 3}, &sign.Transaction{PersonaTag: "foo"})
	txq.AddTransaction(2, FooMsg{X: 4}, &sign.Transaction{PersonaTag: "bar"})

	copyTxq := txq.CopyTransactions()
	assert.Equal(t, copyTxq.GetAmountOfTxs(), 2)
	assert.Equal(t, txq.GetAmountOfTxs(), 0)
}

func TestNewTransactionPanicsIfNoName(t *testing.T) {
	type Foo struct{}
	require.Panics(
		t, func() {
			ecs.NewMessageType[Foo, Foo]("")
		},
	)
}
