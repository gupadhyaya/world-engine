package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/rotisserie/eris"
)

type TransactionReceiptsReply struct {
	StartTick uint64     `json:"startTick"`
	EndTick   uint64     `json:"endTick"`
	Receipts  []*Receipt `json:"receipts"`
}

type Receipt struct {
	TxHash string         `json:"txHash"`
	Result map[string]any `json:"result"`
	Errors []string       `json:"errors"`
}

// receiptsDispatcher continually polls Cardinal for transaction receipts and dispatches them to any subscribed
// channels. The subscribed channels are stored in the sync.Map.
type receiptsDispatcher struct {
	ch chan *Receipt
	m  *sync.Map
}

func newReceiptsDispatcher() *receiptsDispatcher {
	return &receiptsDispatcher{
		ch: make(receiptChan),
		m:  &sync.Map{},
	}
}

// subscribe allows for the sending of receipts to the given channel. Each given session can
// only be associated with a single channel.
func (r *receiptsDispatcher) subscribe(session string, ch receiptChan) {
	r.m.Store(session, ch)
}

// dispatch continually drains r.ch (receipts from cardinal) and sends copies to all subscribed channels.
// This function is meant to be called in a goroutine. Pushed receipts will not block when sending.
func (r *receiptsDispatcher) dispatch(_ runtime.Logger) {
	for receipt := range r.ch {
		r.m.Range(func(key, value any) bool {
			ch, _ := value.(receiptChan)
			// avoid blocking r.ch by making a best-effort delivery here.
			select {
			case ch <- receipt:
			default:
			}
			return true
		})
	}
}

// pollReceipts calls the cardinal backend to get any new transaction receipts. It never returns, so
// it should be called in a goroutine.
func (r *receiptsDispatcher) pollReceipts(log runtime.Logger) {
	timeBetweenBatched := time.Second
	startTick := uint64(0)
	var err error
	log.Debug("fetching batch of receipts: %d", startTick)
	for {
		startTick, err = r.streamBatchOfReceipts(log, startTick)
		if err != nil {
			log.Error("problem when fetching batch of receipts: %v", eris.ToString(eris.Wrap(err, ""), true))
		}
		time.Sleep(timeBetweenBatched)
	}
}

func (r *receiptsDispatcher) streamBatchOfReceipts(_ runtime.Logger, startTick uint64) (
	newStartTick uint64, err error,
) {
	newStartTick = startTick
	reply, err := r.getBatchOfReceiptsFromCardinal(startTick)
	if err != nil {
		return newStartTick, err
	}

	for _, rec := range reply.Receipts {
		r.ch <- rec
	}
	return reply.EndTick, nil
}

type txReceiptRequest struct {
	StartTick uint64 `json:"startTick"`
}

func (r *receiptsDispatcher) getBatchOfReceiptsFromCardinal(startTick uint64) (
	reply *TransactionReceiptsReply, err error) {
	request := txReceiptRequest{
		StartTick: startTick,
	}
	buf, err := json.Marshal(request)
	if err != nil {
		return nil, eris.Wrap(err, "")
	}
	ctx := context.Background()
	url := makeHTTPURL(transactionReceiptsEndpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, eris.Wrap(err, "")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := doRequest(req)
	if err != nil {
		return nil, eris.Wrapf(err, "failed to query %q", url)
	}
	defer resp.Body.Close()

	reply = &TransactionReceiptsReply{}

	if err = json.NewDecoder(resp.Body).Decode(reply); err != nil {
		return nil, eris.Wrap(err, "")
	}
	return reply, nil
}
