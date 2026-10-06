package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// PaperCloseTestResult describes an isolated simulation. No real orders or
// swaps are submitted, and the source state and ledger are never rewritten.
type PaperCloseTestResult struct {
	Symbol               string    `json:"symbol"`
	SourceSnapshotAt     time.Time `json:"source_snapshot_at"`
	TestDir              string    `json:"test_dir"`
	ExitQuoteUSDT        float64   `json:"exit_quote_usdt"`
	MarkPrice            float64   `json:"mark_price"`
	FundingUSDT          float64   `json:"funding_usdt"`
	FeesAndGasUSDT       float64   `json:"fees_and_gas_usdt"`
	EstimatedPnLUSDT     float64   `json:"estimated_pnl_usdt"`
	OriginalPositionUSDT float64   `json:"original_position_usdt"`
}

type paperCloseQuoteChain struct {
	chainGateway
	tokenAddress string
	quoteUSDT    float64
}

func (c paperCloseQuoteChain) SellQuote(_ context.Context, t TokenConfig, _ float64) (float64, error) {
	if !strings.EqualFold(t.BSCAddress, c.tokenAddress) {
		return 0, errors.New("test quote token does not match the saved position")
	}
	return c.quoteUSDT, nil
}

func (paperCloseQuoteChain) SwapExactInput(context.Context, string, string, uint8, uint8, float64, float64) (float64, common.Hash, error) {
	return 0, common.Hash{}, errors.New("paper close test cannot broadcast a swap")
}

func (paperCloseQuoteChain) SwapExactInputTracked(context.Context, string, string, uint8, uint8, float64, float64, func(common.Hash) error) (float64, common.Hash, error) {
	return 0, common.Hash{}, errors.New("paper close test cannot broadcast a swap")
}

type paperCloseReadOnlyBinance struct{ binanceGateway }

func (paperCloseReadOnlyBinance) MarketOrder(context.Context, string, string, float64, bool) (OrderFill, error) {
	return OrderFill{}, errors.New("paper close test cannot submit a futures order")
}

func (paperCloseReadOnlyBinance) MarketOrderTracked(context.Context, string, string, float64, bool, string) (OrderFill, error) {
	return OrderFill{}, errors.New("paper close test cannot submit a futures order")
}

func (paperCloseReadOnlyBinance) ConfigureSymbol(context.Context, string) error {
	return errors.New("paper close test cannot configure a futures symbol")
}

// runPaperCloseTest takes a saved position snapshot, obtains fresh executable
// sell and futures quotes, then runs the ordinary paper close path on a copy.
func runPaperCloseTest(ctx context.Context, cfg Config, symbol string, binance binanceGateway, chain chainGateway) (PaperCloseTestResult, error) {
	if cfg.Mode != "paper" {
		return PaperCloseTestResult{}, errors.New("manual close test requires paper mode")
	}
	if symbol == "" || symbol != strings.ToUpper(symbol) || strings.TrimSpace(symbol) != symbol {
		return PaperCloseTestResult{}, errors.New("symbol must be an uppercase Binance contract symbol")
	}
	sourceState, err := os.ReadFile(filepath.Join(cfg.StateDir, "state.json"))
	if err != nil {
		return PaperCloseTestResult{}, fmt.Errorf("read paper state: %w", err)
	}
	var snapshot BotState
	if err := json.Unmarshal(sourceState, &snapshot); err != nil {
		return PaperCloseTestResult{}, fmt.Errorf("decode paper state: %w", err)
	}
	if snapshot.UpdatedAt.IsZero() || time.Since(snapshot.UpdatedAt) > time.Duration(cfg.Risk.MaxDataAgeSeconds)*time.Second || snapshot.UpdatedAt.After(time.Now().Add(time.Minute)) {
		return PaperCloseTestResult{}, errors.New("saved paper state is stale; wait for a fresh bot cycle before testing")
	}
	position := snapshot.Positions[symbol]
	if position == nil {
		return PaperCloseTestResult{}, fmt.Errorf("%s is not an open paper position", symbol)
	}
	if len(snapshot.PendingOperations) != 0 {
		return PaperCloseTestResult{}, errors.New("paper state has unfinished operations")
	}
	if position.TokenQty <= 0 || position.ShortQty <= 0 || position.SpotCostUSDT <= 0 {
		return PaperCloseTestResult{}, errors.New("saved paper position has invalid quantities")
	}
	marketMap, err := binance.Markets(ctx)
	if err != nil {
		return PaperCloseTestResult{}, fmt.Errorf("load Binance market: %w", err)
	}
	market, ok := marketMap[symbol]
	if !ok || market.MarkPrice <= 0 || market.IndexPrice <= 0 || market.UpdatedAt.IsZero() || time.Since(market.UpdatedAt) > time.Duration(cfg.Risk.MaxDataAgeSeconds)*time.Second || market.UpdatedAt.After(time.Now().Add(time.Minute)) {
		return PaperCloseTestResult{}, fmt.Errorf("fresh Binance mark and index prices are unavailable for %s", symbol)
	}
	token, err := immutablePositionToken(position, TokenConfig{Symbol: symbol})
	if err != nil {
		return PaperCloseTestResult{}, err
	}
	quote, err := chain.SellQuote(ctx, token, position.TokenQty)
	if err != nil {
		return PaperCloseTestResult{}, fmt.Errorf("executable OKX DEX sell quote failed: %w", err)
	}
	if quote <= 0 || math.IsNaN(quote) || math.IsInf(quote, 0) {
		return PaperCloseTestResult{}, errors.New("executable OKX DEX sell quote is invalid")
	}
	testDir, err := os.MkdirTemp(cfg.StateDir, "paper-close-test-")
	if err != nil {
		return PaperCloseTestResult{}, fmt.Errorf("create isolated paper test directory: %w", err)
	}
	if err := writeFileSynced(filepath.Join(testDir, "state.json"), sourceState, 0600); err != nil {
		return PaperCloseTestResult{}, fmt.Errorf("copy paper state into test directory: %w", err)
	}
	testState, err := loadState(testDir)
	if err != nil {
		return PaperCloseTestResult{}, fmt.Errorf("load isolated paper state: %w", err)
	}
	testCfg := cfg
	testCfg.StateDir = testDir
	testCfg.Alerts = AlertConfig{}
	testEngine := &Engine{
		cfg:               testCfg,
		binance:           paperCloseReadOnlyBinance{binanceGateway: binance},
		chain:             paperCloseQuoteChain{chainGateway: chain, tokenAddress: position.TokenAddress, quoteUSDT: quote},
		alert:             NewAlerter(AlertConfig{}),
		state:             testState,
		tokens:            map[string]TokenConfig{symbol: token},
		positionDecisions: map[string]string{},
	}
	if err := testEngine.closePosition(ctx, testState.Positions[symbol], market, "manual_paper_test"); err != nil {
		return PaperCloseTestResult{}, err
	}
	rows, err := readLedger(testDir, 1)
	if err != nil || len(rows) != 1 || rows[0]["event"] != "close" {
		return PaperCloseTestResult{}, fmt.Errorf("isolated close ledger was not recorded: %v", err)
	}
	if err := saveState(testDir, testState); err != nil {
		return PaperCloseTestResult{}, fmt.Errorf("save isolated paper state: %w", err)
	}
	_, funding, fees := partialReductionPnL(*position, 1, quote, market.MarkPrice, cfg.Binance.TakerFeeBPS, cfg.BSC.GasReserveUSDT)
	result := PaperCloseTestResult{Symbol: symbol, SourceSnapshotAt: snapshot.UpdatedAt, TestDir: testDir, ExitQuoteUSDT: quote, MarkPrice: market.MarkPrice, FundingUSDT: funding, FeesAndGasUSDT: fees, EstimatedPnLUSDT: testState.DailyRealizedPnL - snapshot.DailyRealizedPnL, OriginalPositionUSDT: position.SpotCostUSDT}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return PaperCloseTestResult{}, err
	}
	if err := writeFileSynced(filepath.Join(testDir, "result.json"), append(encoded, '\n'), 0600); err != nil {
		return PaperCloseTestResult{}, fmt.Errorf("save isolated paper test result: %w", err)
	}
	return result, nil
}
