package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type paperCloseTestBinance struct {
	*operationTestBinance
	market FundingMarket
}

func TestPaperCloseTestRejectsStaleSavedPosition(t *testing.T) {
	stateDir := t.TempDir()
	state := BotState{
		Version:   currentStateVersion,
		UpdatedAt: time.Now().Add(-10 * time.Minute),
		Positions: map[string]*Position{"CAKEUSDT": {Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 10, SpotCostUSDT: 100, ShortQty: 10, ShortEntryPrice: 10}},
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	b := &paperCloseTestBinance{operationTestBinance: &operationTestBinance{}, market: FundingMarket{Symbol: "CAKEUSDT", MarkPrice: 9.5, IndexPrice: 9.5, UpdatedAt: time.Now()}}
	cfg := Config{Mode: "paper", StateDir: stateDir, Risk: RiskConfig{MaxDataAgeSeconds: 120}}
	if _, err := runPaperCloseTest(context.Background(), cfg, "CAKEUSDT", b, &operationTestChain{sellOut: 99}); err == nil {
		t.Fatal("stale paper state was accepted")
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed stale test created artifacts: entries=%v err=%v", entries, err)
	}
}

func (b *paperCloseTestBinance) Markets(context.Context) (map[string]FundingMarket, error) {
	return map[string]FundingMarket{b.market.Symbol: b.market}, nil
}

func TestPaperCloseTestUsesIsolatedStateAndExecutableQuote(t *testing.T) {
	stateDir := t.TempDir()
	position := &Position{Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 10, SpotCostUSDT: 100, ShortQty: 10, ShortEntryPrice: 10, FundingAccruedUSDT: .2}
	state := &BotState{Version: currentStateVersion, StartedAt: time.Now(), Positions: map[string]*Position{position.Symbol: position}, PendingOperations: map[string]*PendingOperation{}}
	if err := saveState(stateDir, state); err != nil {
		t.Fatal(err)
	}
	if err := appendLedger(stateDir, map[string]any{"event": "open", "symbol": position.Symbol}); err != nil {
		t.Fatal(err)
	}
	originalState, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	originalLedger, err := os.ReadFile(filepath.Join(stateDir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	b := &paperCloseTestBinance{operationTestBinance: &operationTestBinance{}, market: FundingMarket{Symbol: position.Symbol, MarkPrice: 9.5, IndexPrice: 9.5, UpdatedAt: time.Now()}}
	chain := &operationTestChain{sellOut: 99}
	cfg := Config{Mode: "paper", StateDir: stateDir, Binance: BinanceConfig{TakerFeeBPS: 5}, BSC: BSCConfig{GasReserveUSDT: .2}, Risk: RiskConfig{MaxDataAgeSeconds: 120}}

	result, err := runPaperCloseTest(context.Background(), cfg, position.Symbol, b, chain)
	if err != nil {
		t.Fatal(err)
	}
	if result.Symbol != position.Symbol || math.Abs(result.EstimatedPnLUSDT-3.9025) > 1e-9 || result.ExitQuoteUSDT != 99 {
		t.Fatalf("unexpected close result: %+v", result)
	}
	if len(b.calls) != 0 {
		t.Fatalf("paper test sent a futures order: %v", b.calls)
	}
	if got, err := os.ReadFile(filepath.Join(stateDir, "state.json")); err != nil || string(got) != string(originalState) {
		t.Fatalf("source state changed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(stateDir, "ledger.jsonl")); err != nil || string(got) != string(originalLedger) {
		t.Fatalf("source ledger changed: %v", err)
	}
	isolated, err := loadState(result.TestDir)
	if err != nil {
		t.Fatal(err)
	}
	if isolated.Positions[position.Symbol] != nil {
		t.Fatal("test position remains open in isolated state")
	}
	rows, err := readLedger(result.TestDir, 10)
	if err != nil || len(rows) != 1 || rows[0]["event"] != "close" || rows[0]["reason"] != "manual_paper_test" {
		t.Fatalf("isolated close ledger missing: rows=%v err=%v", rows, err)
	}
}
