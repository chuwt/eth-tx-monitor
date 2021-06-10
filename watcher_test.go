package watcher

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"math/big"
	"testing"
	"time"
)

func TestWatcher(t *testing.T) {
	logConf := zap.NewProductionConfig()
	encoder := zap.NewProductionEncoderConfig()
	encoder.EncodeTime = zapcore.ISO8601TimeEncoder
	encoder.EncodeLevel = zapcore.CapitalColorLevelEncoder

	logConf.EncoderConfig = encoder
	logConf.Encoding = "console"
	logger, _ := logConf.Build()

	backend, err := ethclient.Dial("https://data-seed-prebsc-2-s2.binance.org:8545")
	if err != nil {
		t.Fatal(err)
	}
	m := NewMonitor(logger, backend, time.Second, cancellationDepth)
	block, err := m.backend.BlockNumber(m.ctx)
	nonce, err := backend.NonceAt(
		m.ctx,
		common.HexToAddress("0x56b56C2efEf8233dbDd6b5Cae27E4b14036Be6b0"),
		new(big.Int).SetUint64(block),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("nonce", nonce)

	txC, errC, err := m.WatchTransaction(
		common.HexToHash("0xffb31151b0c9c6ed705527e363c84ef4578a08e36891a96c69b6b3bc02741ff3"),
		common.HexToAddress("0x56b56C2efEf8233dbDd6b5Cae27E4b14036Be6b0"),
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case tx := <-txC:
		txBytes, _ := tx.MarshalJSON()
		t.Log(string(txBytes))
	case err := <-errC:
		t.Log(err)
	}
}
