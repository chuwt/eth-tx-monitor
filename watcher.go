package watcher

import (
	"context"
	"errors"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"go.uber.org/zap"
	"math/big"
	"sync"
	"time"
)

const cancellationDepth = 20

var (
	ErrTransactionCancelled = errors.New("transaction cancelled")
	ErrMonitorClosed        = errors.New("monitor closed")
)

type confirmedTx struct {
	receipt types.Receipt
	watch   *transactionWatch
}

type transactionWatch struct {
	receiptC chan types.Receipt // channel to which the receipt will be written once available
	errC     chan error         // error channel (primarily for cancelled transactions)

	txHash common.Hash // hash of the transaction to watch
	sender common.Address
	nonce  uint64 // nonce of the transaction to watch
}

type Monitor struct {
	lock       sync.Mutex
	ctx        context.Context // context which is used for all backend calls
	cancelFunc context.CancelFunc
	wg         sync.WaitGroup

	backend           Backend
	pollingInterval   time.Duration
	cancellationDepth uint64

	watches    map[*transactionWatch]struct{} // active watches
	watchAdded chan struct{}                  // channel to trigger instant pending check

	log *zap.Logger
}

func NewMonitor(logger *zap.Logger, backend Backend, duration time.Duration, cancellationDepth uint64) *Monitor {
	ctx, cancelFunc := context.WithCancel(context.Background())
	m := Monitor{
		ctx:               ctx,
		cancelFunc:        cancelFunc,
		backend:           backend,
		pollingInterval:   duration,
		cancellationDepth: cancellationDepth,
		watches:           make(map[*transactionWatch]struct{}),
		watchAdded:        make(chan struct{}, 1),
		log:               logger,
	}
	m.wg.Add(1)
	go m.watchPending()

	return &m
}

// 主循环
func (m *Monitor) watchPending() {
	m.log.Info("监听启动...")
	defer m.wg.Done()
	defer func() {
		m.lock.Lock()
		defer m.lock.Unlock()

		for watch := range m.watches {
			watch.errC <- ErrMonitorClosed
		}
	}()

	var (
		lastBlock uint64 = 0
		added     bool
	)

	for {
		added = false
		select {
		case <-m.watchAdded:
			added = true
		case <-time.After(m.pollingInterval):
		case <-m.ctx.Done():
			return
		}

		if !m.hasWatches() {
			continue
		}

		block, err := m.backend.BlockNumber(m.ctx)
		if err != nil {
			m.log.Error("获取区块高度失败", zap.Error(err))
			continue
		} else if block <= lastBlock && !added {
			continue
		}
		if err = m.checkPending(block); err != nil {
			m.log.Error("检查pending交易失败", zap.Error(err))
		}

		lastBlock = block
	}
}

func (m *Monitor) checkPending(block uint64) error {
	checkWatches := m.potentiallyConfirmedWatches(block)

	var confirmedTxs []confirmedTx
	var potentiallyCancelledTxs []*transactionWatch
	// 区分出已经上链的的和未上链的
	for _, watch := range checkWatches {
		receipt, err := m.backend.TransactionReceipt(m.ctx, watch.txHash)
		if receipt != nil {
			// if we have a receipt we have a confirmation
			confirmedTxs = append(confirmedTxs, confirmedTx{
				receipt: *receipt,
				watch:   watch,
			})
		} else if err == nil || errors.Is(err, ethereum.NotFound) {
			// if both err and receipt are nil, there is no receipt
			// we also match for the special error "not found" that some clients return
			// the reason why we consider this only potentially cancelled is to catch cases where after a reorg the original transaction wins
			potentiallyCancelledTxs = append(potentiallyCancelledTxs, watch)
		} else {
			// any other error is probably a real error
			return err
		}
	}
	// 未出现的交易判断是否被取消了
	var cancelledTxs []*transactionWatch
	if len(potentiallyCancelledTxs) > 0 {
		for _, watch := range potentiallyCancelledTxs {
			oldNonce, err := m.backend.NonceAt(m.ctx, watch.sender, new(big.Int).SetUint64(block-m.cancellationDepth))
			if err != nil {
				return err
			}
			if watch.nonce <= oldNonce {
				cancelledTxs = append(cancelledTxs, watch)
			}
		}
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	for _, confirmedTx := range confirmedTxs {
		confirmedTx.watch.receiptC <- confirmedTx.receipt
		delete(m.watches, confirmedTx.watch)
	}

	for _, watch := range cancelledTxs {
		watch.errC <- ErrTransactionCancelled
		delete(m.watches, watch)
	}
	return nil

}

// 找到小于最大nonce的交易
func (m *Monitor) potentiallyConfirmedWatches(block uint64) (watches []*transactionWatch) {
	m.lock.Lock()
	defer m.lock.Unlock()

	for watch := range m.watches {
		nonce, err := m.backend.NonceAt(m.ctx, watch.sender, new(big.Int).SetUint64(block))
		if err != nil {
			continue
		}
		if watch.nonce < nonce {
			watches = append(watches, watch)
		}
	}

	return watches
}

// 添加监听的交易
func (m *Monitor) WatchTransaction(txHash common.Hash, sender common.Address, nonce uint64) (<-chan types.Receipt, <-chan error, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	// these channels will be written to at most once
	// buffer size is 1 to avoid blocking in the watch loop
	receiptC := make(chan types.Receipt, 1)
	errC := make(chan error, 1)

	m.watches[&transactionWatch{
		receiptC: receiptC,
		errC:     errC,
		txHash:   txHash,
		sender:   sender,
		nonce:    nonce,
	}] = struct{}{}

	select {
	case m.watchAdded <- struct{}{}:
	default:
	}

	m.log.Info(
		"开始监听交易",
		zap.String("txId", txHash.Hex()),
		zap.Uint64("nonce", nonce),
	)

	return receiptC, errC, nil
}

// 是否存在监听的交易
func (m *Monitor) hasWatches() bool {
	m.lock.Lock()
	defer m.lock.Unlock()
	return len(m.watches) > 0
}

func (m *Monitor) Close() error {
	m.cancelFunc()
	m.wg.Wait()
	return nil
}
