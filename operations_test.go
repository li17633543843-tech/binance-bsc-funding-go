package main

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

type operationTestBinance struct {
	calls          []string
	compensateFail bool
	risks          map[string]PositionRisk
	fillPrice      float64
}

func (b *operationTestBinance) RateLimitUntil() time.Time { return time.Time{} }
func (b *operationTestBinance) Markets(context.Context) (map[string]FundingMarket, error) {
	return nil, nil
}
func (b *operationTestBinance) Symbols(context.Context) (map[string]BinanceSymbol, error) {
	return nil, nil
}
func (b *operationTestBinance) FundingIntervals(context.Context) (map[string]float64, error) {
	return nil, nil
}
func (b *operationTestBinance) FundingHistory(context.Context, string, time.Time, int) ([]FundingRecord, error) {
	return nil, nil
}
func (b *operationTestBinance) AccountRisk(context.Context) (FuturesAccountRisk, error) {
	return FuturesAccountRisk{}, nil
}
func (b *operationTestBinance) PositionRisks(context.Context) (map[string]PositionRisk, error) {
	return b.risks, nil
}
func (b *operationTestBinance) PositionMode(context.Context) (bool, error)    { return false, nil }
func (b *operationTestBinance) ConfigureSymbol(context.Context, string) error { return nil }
func (b *operationTestBinance) MarketOrder(_ context.Context, _ string, side string, qty float64, _ bool) (OrderFill, error) {
	b.calls = append(b.calls, side)
	if side == "SELL" && b.compensateFail {
		return OrderFill{}, errors.New("compensation rejected")
	}
	price := b.fillPrice
	if price == 0 {
		price = 1
	}
	return OrderFill{ExecutedQty: qty, AvgPrice: price}, nil
}
func (b *operationTestBinance) MarketOrderTracked(ctx context.Context, symbol, side string, qty float64, reduceOnly bool, clientID string) (OrderFill, error) {
	fill, err := b.MarketOrder(ctx, symbol, side, qty, reduceOnly)
	fill.ClientOrderID = clientID
	return fill, err
}
func (b *operationTestBinance) OrderByClientID(context.Context, string, string) (OrderFill, error) {
	return OrderFill{}, errors.New("order not found")
}

type operationTestChain struct {
	sellErr  error
	sellOut  float64
	balance  float64
	txStatus string
}

type stagedIncreaseTestChain struct {
	operationTestChain
	tokenAddress string
	tokenBalance float64
}

func (c *stagedIncreaseTestChain) BestQuote(_ context.Context, t TokenConfig, notional, _ float64) (ChainQuote, error) {
	return ChainQuote{Symbol: t.Symbol, InputUSDT: notional, OutputTokens: notional / 10, BuyPrice: 10, SellPrice: 9.99, BuyPriceImpactBPS: 5, SellPriceImpactBPS: 6}, nil
}

func (c *stagedIncreaseTestChain) Balance(_ context.Context, address string, _ uint8) (float64, error) {
	if strings.EqualFold(address, c.tokenAddress) {
		return c.tokenBalance, nil
	}
	return 1000, nil
}

func (c *stagedIncreaseTestChain) SwapExactInputTracked(_ context.Context, _, _ string, _, _ uint8, input, minOutput float64, onBroadcast func(common.Hash) error) (float64, common.Hash, error) {
	hash := common.HexToHash("0x1234")
	if err := onBroadcast(hash); err != nil {
		return 0, hash, err
	}
	c.tokenBalance += minOutput
	return input, hash, nil
}

type paginationBinance struct {
	*operationTestBinance
	pages int
}

type timeoutRecoveryBinance struct{ *operationTestBinance }

func (b *timeoutRecoveryBinance) MarketOrderTracked(context.Context, string, string, float64, bool, string) (OrderFill, error) {
	return OrderFill{}, context.DeadlineExceeded
}
func (b *timeoutRecoveryBinance) OrderByClientID(_ context.Context, symbol, clientID string) (OrderFill, error) {
	return OrderFill{OrderID: 77, ClientOrderID: clientID, Symbol: symbol, ExecutedQty: 5, AvgPrice: 1}, nil
}

func (b *paginationBinance) FundingHistory(_ context.Context, _ string, start time.Time, _ int) ([]FundingRecord, error) {
	b.pages++
	if b.pages == 1 {
		out := make([]FundingRecord, 1000)
		for i := range out {
			out[i] = FundingRecord{Time: start.Add(time.Duration(i+1) * time.Hour), Rate: .0001}
		}
		return out, nil
	}
	return []FundingRecord{{Time: start.Add(time.Hour), Rate: .0002}}, nil
}

func (c *operationTestChain) Close() {}
func (c *operationTestChain) TokenCatalog(context.Context) ([]OKXCatalogToken, error) {
	return nil, nil
}
func (c *operationTestChain) SearchTokens(context.Context, string) ([]OKXCatalogToken, error) {
	return nil, nil
}
func (c *operationTestChain) SearchTokenAddress(context.Context, string) ([]OKXCatalogToken, error) {
	return nil, nil
}
func (c *operationTestChain) DepthCapacity(context.Context, TokenConfig, Config) (float64, ChainQuote, error) {
	return 0, ChainQuote{}, nil
}
func (c *operationTestChain) PositionExitCapacity(context.Context, TokenConfig, float64, float64, float64) (float64, PositionExitQuote, error) {
	return 0, PositionExitQuote{}, nil
}
func (c *operationTestChain) BestQuote(context.Context, TokenConfig, float64, float64) (ChainQuote, error) {
	return ChainQuote{}, nil
}
func (c *operationTestChain) SellQuote(context.Context, TokenConfig, float64) (float64, error) {
	return c.sellOut, c.sellErr
}
func (c *operationTestChain) Balance(context.Context, string, uint8) (float64, error) {
	return c.balance, nil
}
func (c *operationTestChain) SwapExactInput(context.Context, string, string, uint8, uint8, float64, float64) (float64, common.Hash, error) {
	return 0, common.Hash{}, nil
}
func (c *operationTestChain) SwapExactInputTracked(ctx context.Context, a, b string, c1, c2 uint8, f1, f2 float64, onBroadcast func(common.Hash) error) (float64, common.Hash, error) {
	hash := common.HexToHash("0x1")
	if onBroadcast != nil {
		if err := onBroadcast(hash); err != nil {
			return 0, hash, err
		}
	}
	out, _, err := c.SwapExactInput(ctx, a, b, c1, c2, f1, f2)
	return out, hash, err
}
func (c *operationTestChain) TransactionStatus(context.Context, common.Hash) (string, error) {
	if c.txStatus == "" {
		return "success", nil
	}
	return c.txStatus, nil
}

func operationTestEngine(t *testing.T, compensateFail bool) (*Engine, *operationTestBinance) {
	t.Helper()
	b := &operationTestBinance{compensateFail: compensateFail}
	e := &Engine{
		cfg:     Config{Mode: "live", StateDir: t.TempDir(), Binance: BinanceConfig{Leverage: 5, MarginType: "CROSSED"}, Risk: RiskConfig{MaxDataAgeSeconds: 120}, BSC: BSCConfig{USDTAddress: "0x55d398326f99059ff775485246999027b3197955", USDTDecimals: 18}},
		binance: b,
		chain:   &operationTestChain{sellErr: errors.New("DEX unavailable"), balance: 100},
		alert:   NewAlerter(AlertConfig{}),
		state:   &BotState{Version: currentStateVersion, Positions: map[string]*Position{}, PendingOperations: map[string]*PendingOperation{}},
		symbols: map[string]BinanceSymbol{"CAKEUSDT": {Symbol: "CAKEUSDT", StepSize: .001, MinQty: .001}},
	}
	return e, b
}

func TestRebalanceIncreasesProbePositionWhenDepthHistoryMatures(t *testing.T) {
	now := time.Now()
	e := &Engine{
		cfg: Config{
			Mode:     "paper",
			StateDir: t.TempDir(),
			Strategy: StrategyConfig{TargetPositions: 25, EntryConfirmationScans: 1},
			Risk:     RiskConfig{TotalCapitalUSDT: 1000, MaxDataAgeSeconds: 120, ReferenceQuoteUSDT: 10, MaxEntryChainPriceImpactBPS: 20},
		},
		chain: &depthHistoryTestChain{},
		alert: NewAlerter(AlertConfig{}),
		state: &BotState{
			Positions:          map[string]*Position{"CAKEUSDT": {Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 7.5, SpotCostUSDT: 75, ShortQty: 7.5, ShortEntryPrice: 10, OpenedAt: now.Add(-time.Hour), DepthHistoryStage: "quarter"}},
			EntryConfirmations: map[string]int{},
			CooldownUntil:      map[string]time.Time{},
			PendingOperations:  map[string]*PendingOperation{},
		},
		tokens:  map[string]TokenConfig{"CAKEUSDT": {Symbol: "CAKE", BinanceSymbol: "CAKEUSDT", BSCAddress: "0x1111111111111111111111111111111111111111", Decimals: 18}},
		symbols: map[string]BinanceSymbol{"CAKEUSDT": {Symbol: "CAKEUSDT", StepSize: .001, MinQty: .001}},
	}
	market := FundingMarket{Symbol: "CAKEUSDT", MarkPrice: 10, IndexPrice: 10, UpdatedAt: now}
	opportunity := Opportunity{Symbol: "CAKEUSDT", TargetNotionalUSDT: 150, FullTargetNotionalUSDT: 300, FundingAPRPercent: 30, Chain: ChainQuote{InputUSDT: 150, OutputTokens: 15, BuyPrice: 10, BuyPriceImpactBPS: 5, SellPriceImpactBPS: 6}, Market: market, DepthHistory: DepthHistoryAssessment{Stage: "half", AllocationFraction: .5}}
	e.rebalance(context.Background(), now, map[string]FundingMarket{"CAKEUSDT": market}, []Opportunity{opportunity})
	p := e.state.Positions["CAKEUSDT"]
	if p == nil || math.Abs(p.SpotCostUSDT-150) > 1e-9 || math.Abs(p.TokenQty-15) > 1e-9 || math.Abs(p.ShortQty-15) > 1e-9 {
		t.Fatalf("mature depth history did not scale the existing probe position: %+v", p)
	}
	if p.DepthHistoryStage != "half" {
		t.Fatalf("mature depth history stage was not saved: %+v", p)
	}
}

func TestRebalanceDoesNotRegrowLegacyDepthReducedPosition(t *testing.T) {
	now := time.Now()
	e := &Engine{
		cfg: Config{
			Mode:     "paper",
			StateDir: t.TempDir(),
			Strategy: StrategyConfig{TargetPositions: 25, EntryConfirmationScans: 1},
			Risk:     RiskConfig{TotalCapitalUSDT: 1000, MaxDataAgeSeconds: 120, ReferenceQuoteUSDT: 10, MaxEntryChainPriceImpactBPS: 20},
		},
		chain: &depthHistoryTestChain{},
		alert: NewAlerter(AlertConfig{}),
		state: &BotState{
			Positions:          map[string]*Position{"CAKEUSDT": {Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 7.5, SpotCostUSDT: 75, ShortQty: 7.5, ShortEntryPrice: 10, OpenedAt: now.Add(-time.Hour)}},
			EntryConfirmations: map[string]int{},
			CooldownUntil:      map[string]time.Time{},
			PendingOperations:  map[string]*PendingOperation{},
		},
		tokens:  map[string]TokenConfig{"CAKEUSDT": {Symbol: "CAKE", BinanceSymbol: "CAKEUSDT", BSCAddress: "0x1111111111111111111111111111111111111111", Decimals: 18}},
		symbols: map[string]BinanceSymbol{"CAKEUSDT": {Symbol: "CAKEUSDT", StepSize: .001, MinQty: .001}},
	}
	market := FundingMarket{Symbol: "CAKEUSDT", MarkPrice: 10, IndexPrice: 10, UpdatedAt: now}
	opportunity := Opportunity{Symbol: "CAKEUSDT", TargetNotionalUSDT: 150, FullTargetNotionalUSDT: 300, FundingAPRPercent: 30, Chain: ChainQuote{InputUSDT: 150, OutputTokens: 15, BuyPrice: 10, BuyPriceImpactBPS: 5, SellPriceImpactBPS: 6}, Market: market, DepthHistory: DepthHistoryAssessment{Stage: "half", AllocationFraction: .5}}
	e.rebalance(context.Background(), now, map[string]FundingMarket{"CAKEUSDT": market}, []Opportunity{opportunity})
	p := e.state.Positions["CAKEUSDT"]
	if p == nil || math.Abs(p.SpotCostUSDT-75) > 1e-9 {
		t.Fatalf("legacy reduced position was incorrectly regrown: %+v", p)
	}
}

func TestLiveDepthStageIncreaseCombinesBothLegsAndClearsJournal(t *testing.T) {
	address := "0x1111111111111111111111111111111111111111"
	b := &operationTestBinance{fillPrice: 10}
	c := &stagedIncreaseTestChain{tokenAddress: address, tokenBalance: 7.5}
	now := time.Now()
	p := &Position{Symbol: "CAKEUSDT", TokenAddress: address, TokenDecimals: 18, TokenQty: 7.5, SpotCostUSDT: 75, ShortQty: 7.5, ShortEntryPrice: 10, OpenedAt: now.Add(-time.Hour), DepthHistoryStage: "quarter"}
	e := &Engine{
		cfg:                Config{Mode: "live", StateDir: t.TempDir(), Binance: BinanceConfig{Leverage: 5}, BSC: BSCConfig{USDTAddress: "0x55d398326f99059ff775485246999027b3197955", USDTDecimals: 18}, Strategy: StrategyConfig{MaxEntryBasisBPS: 100}, Risk: RiskConfig{TotalCapitalUSDT: 1000, ReferenceQuoteUSDT: 10, MaxEntryChainPriceImpactBPS: 20, MaxFuturesMarginUsePercent: 100}},
		binance:            b,
		chain:              c,
		alert:              NewAlerter(AlertConfig{}),
		state:              &BotState{Version: currentStateVersion, Positions: map[string]*Position{"CAKEUSDT": p}, PendingOperations: map[string]*PendingOperation{}},
		tokens:             map[string]TokenConfig{"CAKEUSDT": {Symbol: "CAKE", BinanceSymbol: "CAKEUSDT", BSCAddress: address, Decimals: 18}},
		symbols:            map[string]BinanceSymbol{"CAKEUSDT": {Symbol: "CAKEUSDT", StepSize: .001, MinQty: .001}},
		accountRisk:        FuturesAccountRisk{AvailableBalance: 1000},
		chainAvailableUSDT: 1000,
	}
	o := Opportunity{Symbol: "CAKEUSDT", TargetNotionalUSDT: 150, Market: FundingMarket{MarkPrice: 10}, DepthHistory: DepthHistoryAssessment{Stage: "half", AllocationFraction: .5}}
	if err := e.increasePosition(context.Background(), p, o, "depth_history_stage_half"); err != nil {
		t.Fatal(err)
	}
	if math.Abs(p.SpotCostUSDT-150) > 1e-9 || math.Abs(p.TokenQty-15) > 1e-9 || math.Abs(p.ShortQty-15) > 1e-9 || p.DepthHistoryStage != "half" || len(e.state.PendingOperations) != 0 || len(b.calls) != 1 || b.calls[0] != "SELL" {
		t.Fatalf("live staged increase was not atomically reflected: position=%+v pending=%+v calls=%v", p, e.state.PendingOperations, b.calls)
	}
}

func TestStartupRecoveryCompletesConfirmedDepthStageIncrease(t *testing.T) {
	e, b := operationTestEngine(t, false)
	e.chain = &operationTestChain{balance: 15, txStatus: "success"}
	b.risks = map[string]PositionRisk{"CAKEUSDT": {Symbol: "CAKEUSDT", PositionAmount: -15, EntryPrice: 10, MarkPrice: 10}}
	p := &Position{Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 7.5, SpotCostUSDT: 75, ShortQty: 7.5, ShortEntryPrice: 10, OpenedAt: time.Now().Add(-time.Hour), DepthHistoryStage: "quarter"}
	e.state.Positions[p.Symbol] = p
	op := &PendingOperation{ID: "increase-1", Kind: "increase", Stage: "futures_status_unknown", Symbol: p.Symbol, TokenAddress: p.TokenAddress, TokenDecimals: p.TokenDecimals, BalanceBefore: 7.5, BalanceAfter: 15, ChainTxHash: common.HexToHash("0x1234").Hex(), OriginalTokenQty: 7.5, OriginalShortQty: 7.5, OriginalSpotCostUSDT: 75, OriginalShortEntryPrice: 10, TargetNotionalUSDT: 75, PlannedTokenQty: 7.5, PlannedFuturesQty: 7.5, DepthHistoryStage: "half", CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now()}
	e.state.PendingOperations[op.ID] = op
	if err := e.recoverPendingOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if math.Abs(p.SpotCostUSDT-150) > 1e-9 || math.Abs(p.TokenQty-15) > 1e-9 || math.Abs(p.ShortQty-15) > 1e-9 || p.DepthHistoryStage != "half" || len(e.state.PendingOperations) != 0 {
		t.Fatalf("confirmed staged increase was not recovered: position=%+v pending=%+v", p, e.state.PendingOperations)
	}
}

func TestLiveReductionRestoresFuturesWhenSpotLegFails(t *testing.T) {
	e, b := operationTestEngine(t, false)
	p := &Position{Symbol: "CAKEUSDT", TokenQty: 10, ShortQty: 10}
	token := TokenConfig{BinanceSymbol: p.Symbol, BSCAddress: "0x1111111111111111111111111111111111111111", Decimals: 18}
	if _, _, err := e.executeLiveReduction(context.Background(), p, token, .5, "test"); err == nil {
		t.Fatal("spot failure must be reported")
	}
	if len(b.calls) != 2 || b.calls[0] != "BUY" || b.calls[1] != "SELL" {
		t.Fatalf("expected close then compensation, got %v", b.calls)
	}
	if len(e.state.PendingOperations) != 0 {
		t.Fatalf("successful compensation left pending operation: %+v", e.state.PendingOperations)
	}
}

func TestLiveReductionDoesNotTradeWhenIntentCannotBePersisted(t *testing.T) {
	e, b := operationTestEngine(t, false)
	blockedPath := t.TempDir() + string(os.PathSeparator) + "not-a-directory"
	if err := os.WriteFile(blockedPath, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	e.cfg.StateDir = blockedPath
	p := &Position{Symbol: "CAKEUSDT", TokenQty: 10, ShortQty: 10}
	token := TokenConfig{BinanceSymbol: p.Symbol, BSCAddress: "0x1111111111111111111111111111111111111111", Decimals: 18}
	if _, _, err := e.executeLiveReduction(context.Background(), p, token, .5, "test"); err == nil {
		t.Fatal("unwritable operation journal was accepted")
	}
	if len(b.calls) != 0 {
		t.Fatalf("external order was sent before durable intent: %v", b.calls)
	}
}

func TestLiveReductionBlocksNewRiskWhenCompensationFails(t *testing.T) {
	e, _ := operationTestEngine(t, true)
	p := &Position{Symbol: "CAKEUSDT", TokenQty: 10, ShortQty: 10}
	token := TokenConfig{BinanceSymbol: p.Symbol, BSCAddress: "0x1111111111111111111111111111111111111111", Decimals: 18}
	if _, _, err := e.executeLiveReduction(context.Background(), p, token, .5, "test"); err == nil {
		t.Fatal("double-leg failure must be reported")
	}
	if len(e.state.PendingOperations) != 1 {
		t.Fatalf("unresolved exposure was not journaled: %+v", e.state.PendingOperations)
	}
	if reason := e.riskIncreaseBlockReason(time.Now(), map[string]FundingMarket{}); reason == "" {
		t.Fatal("unresolved exposure did not block new risk")
	}
}

func TestStartupReconciliationAcceptsMatchingRealBalances(t *testing.T) {
	e, b := operationTestEngine(t, false)
	e.cfg.Risk.MaxHedgeDriftPercent = 3
	e.state.Positions["CAKEUSDT"] = &Position{Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 10, ShortQty: 10}
	e.tokens = map[string]TokenConfig{"CAKEUSDT": {Symbol: "CAKE", BinanceSymbol: "CAKEUSDT", BSCAddress: "0x1111111111111111111111111111111111111111", Decimals: 18}}
	e.chain = &operationTestChain{balance: 10}
	b.risks = map[string]PositionRisk{"CAKEUSDT": {Symbol: "CAKEUSDT", PositionSide: "BOTH", PositionAmount: -10, Leverage: 5, MarginType: "CROSS"}}
	if err := e.reconcileLiveState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.accountReconciled {
		t.Fatal("matching account was not marked reconciled")
	}
}

func TestStartupReconciliationRejectsMissingShort(t *testing.T) {
	e, _ := operationTestEngine(t, false)
	e.cfg.Risk.MaxHedgeDriftPercent = 3
	e.state.Positions["CAKEUSDT"] = &Position{Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 10, ShortQty: 10}
	e.tokens = map[string]TokenConfig{"CAKEUSDT": {Symbol: "CAKE", BinanceSymbol: "CAKEUSDT", BSCAddress: "0x1111111111111111111111111111111111111111", Decimals: 18}}
	e.chain = &operationTestChain{balance: 10}
	if err := e.reconcileLiveState(context.Background()); err == nil {
		t.Fatal("missing futures leg was accepted")
	}
	if e.accountReconciled {
		t.Fatal("mismatched account was marked reconciled")
	}
}

func TestFundingHistoryPaginationLoadsMoreThanOneThousandSettlements(t *testing.T) {
	b := &paginationBinance{operationTestBinance: &operationTestBinance{}}
	e := &Engine{binance: b}
	records, err := e.fetchFundingHistory(context.Background(), "FASTUSDT", time.Now().Add(-90*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1001 || b.pages != 2 {
		t.Fatalf("pagination stopped early: records=%d pages=%d", len(records), b.pages)
	}
}

func TestTrackedOrderRecoversAcceptedFillAfterSubmitTimeout(t *testing.T) {
	e, _ := operationTestEngine(t, false)
	e.binance = &timeoutRecoveryBinance{operationTestBinance: &operationTestBinance{}}
	token := TokenConfig{BSCAddress: "0x1111111111111111111111111111111111111111", Decimals: 18}
	op, err := e.beginOperation("reduce", "CAKEUSDT", "test", token)
	if err != nil {
		t.Fatal(err)
	}
	fill, err := e.submitTrackedMarketOrder(context.Background(), op, "futures-reduce", "BUY", 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if fill.OrderID != 77 || fill.ExecutedQty != 5 || op.BinanceClientOrderID == "" || op.BinanceOrderID != 77 {
		t.Fatalf("accepted order was not recovered by client id: fill=%+v op=%+v", fill, op)
	}
}

func TestPendingReductionRecoveryUsesReceiptAndRealBalances(t *testing.T) {
	e, b := operationTestEngine(t, false)
	e.chain = &operationTestChain{balance: 5, txStatus: "success"}
	b.risks = map[string]PositionRisk{"CAKEUSDT": {Symbol: "CAKEUSDT", PositionAmount: -5}}
	e.state.Positions["CAKEUSDT"] = &Position{Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 10, ShortQty: 10, SpotCostUSDT: 100}
	op := &PendingOperation{ID: "recover-reduce", Kind: "reduce", Stage: "chain_status_unknown", Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, OriginalTokenQty: 10, OriginalShortQty: 10, PlannedTokenQty: 5, ChainTxHash: common.HexToHash("0x1").Hex()}
	e.state.PendingOperations[op.ID] = op
	if err := e.recoverPendingOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := e.state.Positions["CAKEUSDT"]
	if p == nil || p.TokenQty != 5 || p.ShortQty != 5 || len(e.state.PendingOperations) != 0 {
		t.Fatalf("recovery did not align saved position: p=%+v pending=%+v", p, e.state.PendingOperations)
	}
}

func TestPendingOpenRecoveryRebuildsPositionWithoutDuplicateOrder(t *testing.T) {
	e, b := operationTestEngine(t, false)
	e.chain = &operationTestChain{balance: 10, txStatus: "success"}
	b.risks = map[string]PositionRisk{"CAKEUSDT": {Symbol: "CAKEUSDT", PositionAmount: -10, MarkPrice: 2}}
	op := &PendingOperation{ID: "recover-open", Kind: "open", Stage: "spot_status_unknown", Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TargetNotionalUSDT: 20, BalanceBefore: 0, ChainTxHash: common.HexToHash("0x1").Hex(), CreatedAt: time.Now().Add(-time.Minute)}
	e.state.PendingOperations[op.ID] = op
	if err := e.recoverPendingOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := e.state.Positions["CAKEUSDT"]
	if p == nil || p.TokenQty != 10 || p.ShortQty != 10 || len(b.calls) != 0 || len(e.state.PendingOperations) != 0 {
		t.Fatalf("open recovery duplicated or lost a leg: p=%+v calls=%v pending=%+v", p, b.calls, e.state.PendingOperations)
	}
}

func TestPendingOpenRecoveryDoesNotDuplicateUnknownBinanceOrder(t *testing.T) {
	e, b := operationTestEngine(t, false)
	e.chain = &operationTestChain{balance: 10, txStatus: "success"}
	op := &PendingOperation{ID: "recover-open-unknown-order", Kind: "open", Stage: "futures_status_unknown", Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TargetNotionalUSDT: 20, PlannedTokenQty: 10, BalanceBefore: 0, ChainTxHash: common.HexToHash("0x1").Hex(), BinanceClientOrderID: "fb-existing-order", CreatedAt: time.Now().Add(-time.Minute)}
	e.state.PendingOperations[op.ID] = op
	if err := e.recoverPendingOperations(context.Background()); err == nil {
		t.Fatal("unknown Binance order status was treated as absent")
	}
	if len(b.calls) != 0 || len(e.state.PendingOperations) != 1 {
		t.Fatalf("unknown Binance order caused duplicate trading: calls=%v pending=%+v", b.calls, e.state.PendingOperations)
	}
}

func TestPendingOpenRecoveryDoesNotHedgeUnattributedTokenBalance(t *testing.T) {
	e, b := operationTestEngine(t, false)
	e.chain = &operationTestChain{balance: 10}
	op := &PendingOperation{ID: "recover-open-no-hash", Kind: "open", Stage: "spot_submitting", Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TargetNotionalUSDT: 20, PlannedTokenQty: 10, BalanceBefore: 0, CreatedAt: time.Now().Add(-time.Minute)}
	e.state.PendingOperations[op.ID] = op
	if err := e.recoverPendingOperations(context.Background()); err == nil {
		t.Fatal("unattributed token balance was treated as the bot's confirmed purchase")
	}
	if len(b.calls) != 0 || len(e.state.PendingOperations) != 1 {
		t.Fatalf("unattributed balance caused an automatic hedge: calls=%v pending=%+v", b.calls, e.state.PendingOperations)
	}
}

func TestPendingOpenRollbackSuccessClearsJournalWithoutOpeningShort(t *testing.T) {
	e, b := operationTestEngine(t, false)
	e.chain = &operationTestChain{balance: 0, txStatus: "success"}
	op := &PendingOperation{ID: "recover-open-rollback", Kind: "open", Stage: "spot_rollback_status_unknown", Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, PlannedTokenQty: 10, BalanceBefore: 0, ChainTxHash: common.HexToHash("0x1").Hex(), CompensationChainTxHash: common.HexToHash("0x2").Hex(), CreatedAt: time.Now().Add(-time.Minute)}
	e.state.PendingOperations[op.ID] = op
	if err := e.recoverPendingOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 0 || len(e.state.Positions) != 0 || len(e.state.PendingOperations) != 0 {
		t.Fatalf("confirmed rollback caused duplicate trading: calls=%v positions=%+v pending=%+v", b.calls, e.state.Positions, e.state.PendingOperations)
	}
}

func TestPendingOpenRollbackPendingKeepsRiskLocked(t *testing.T) {
	e, b := operationTestEngine(t, false)
	e.chain = &operationTestChain{balance: 10, txStatus: "pending"}
	op := &PendingOperation{ID: "recover-open-rollback", Kind: "open", Stage: "spot_rollback_status_unknown", Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, PlannedTokenQty: 10, BalanceBefore: 0, ChainTxHash: common.HexToHash("0x1").Hex(), CompensationChainTxHash: common.HexToHash("0x2").Hex(), CreatedAt: time.Now().Add(-time.Minute)}
	e.state.PendingOperations[op.ID] = op
	if err := e.recoverPendingOperations(context.Background()); err == nil {
		t.Fatal("pending rollback receipt was treated as complete")
	}
	if len(b.calls) != 0 || len(e.state.PendingOperations) != 1 {
		t.Fatalf("pending rollback did not remain fail-closed: calls=%v pending=%+v", b.calls, e.state.PendingOperations)
	}
	if reason := e.riskIncreaseBlockReason(time.Now(), map[string]FundingMarket{}); reason == "" {
		t.Fatal("pending rollback did not block new risk")
	}
}

func TestLiveHedgeCorrectionUsesDurableTrackedOrder(t *testing.T) {
	e, b := operationTestEngine(t, false)
	e.cfg.Risk.MaxHedgeDriftPercent = 3
	p := &Position{Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 10, ShortQty: 8}
	e.state.Positions[p.Symbol] = p
	if err := e.correctHedgeDrift(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 1 || b.calls[0] != "SELL" || p.ShortQty != 10 || len(e.state.PendingOperations) != 0 {
		t.Fatalf("tracked hedge correction failed: calls=%v position=%+v pending=%+v", b.calls, p, e.state.PendingOperations)
	}
}

func TestPendingHedgeRecoveryUsesActualBinancePosition(t *testing.T) {
	e, b := operationTestEngine(t, false)
	e.cfg.Risk.MaxHedgeDriftPercent = 3
	p := &Position{Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 10, ShortQty: 8}
	e.state.Positions[p.Symbol] = p
	b.risks = map[string]PositionRisk{"CAKEUSDT": {Symbol: "CAKEUSDT", PositionAmount: -10}}
	op := &PendingOperation{ID: "recover-hedge", Kind: "hedge", Stage: "futures_status_unknown", Symbol: p.Symbol, TokenAddress: p.TokenAddress, TokenDecimals: p.TokenDecimals, OriginalTokenQty: 10, OriginalShortQty: 8, PlannedFuturesQty: 2, BinanceSide: "SELL", BinanceClientOrderID: "fb-existing-hedge", CreatedAt: time.Now().Add(-time.Minute)}
	e.state.PendingOperations[op.ID] = op
	if err := e.recoverPendingOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.ShortQty != 10 || len(b.calls) != 0 || len(e.state.PendingOperations) != 0 {
		t.Fatalf("hedge recovery duplicated or lost the fill: position=%+v calls=%v pending=%+v", p, b.calls, e.state.PendingOperations)
	}
}

func TestPendingHedgeRecoveryDoesNotRepeatUnknownOrder(t *testing.T) {
	e, b := operationTestEngine(t, false)
	e.cfg.Risk.MaxHedgeDriftPercent = 3
	p := &Position{Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 10, ShortQty: 8}
	e.state.Positions[p.Symbol] = p
	b.risks = map[string]PositionRisk{"CAKEUSDT": {Symbol: "CAKEUSDT", PositionAmount: -8}}
	op := &PendingOperation{ID: "recover-hedge-unknown", Kind: "hedge", Stage: "futures_status_unknown", Symbol: p.Symbol, TokenAddress: p.TokenAddress, TokenDecimals: p.TokenDecimals, OriginalTokenQty: 10, OriginalShortQty: 8, PlannedFuturesQty: 2, BinanceSide: "SELL", BinanceClientOrderID: "fb-existing-hedge", CreatedAt: time.Now().Add(-time.Minute)}
	e.state.PendingOperations[op.ID] = op
	if err := e.recoverPendingOperations(context.Background()); err == nil {
		t.Fatal("unknown hedge order status was treated as absent")
	}
	if len(b.calls) != 0 || len(e.state.PendingOperations) != 1 {
		t.Fatalf("unknown hedge order caused duplicate trading: calls=%v pending=%+v", b.calls, e.state.PendingOperations)
	}
}

func TestProfitablePaperPositionStaysOpenWithoutBetterDestination(t *testing.T) {
	e, _ := operationTestEngine(t, false)
	e.cfg.Mode = "paper"
	e.cfg.Strategy.MinPriceArbNetUSDT = 1
	e.cfg.Strategy.PriceSpreadTakeProfitBPS = 50
	p := &Position{Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 10, SpotCostUSDT: 100, ShortQty: 10, ShortEntryPrice: 10}
	e.state.Positions[p.Symbol] = p
	e.chain = &operationTestChain{sellOut: 110}
	markets := map[string]FundingMarket{p.Symbol: {Symbol: p.Symbol, MarkPrice: 9, UpdatedAt: time.Now()}}
	e.manageSpreadProfitRotations(context.Background(), time.Now(), markets, nil)
	if e.state.Positions[p.Symbol] == nil || len(e.state.PendingOperations) != 0 {
		t.Fatal("profitable position was closed without a better destination")
	}
}

func TestProfitablePaperPositionRotatesOnlyToBetterDestination(t *testing.T) {
	e, _ := operationTestEngine(t, false)
	e.cfg.Mode = "paper"
	e.cfg.Strategy.MinPriceArbNetUSDT = 1
	e.cfg.Strategy.PriceSpreadTakeProfitBPS = 50
	e.cfg.Strategy.EvaluationHoldHours = 30 * 24
	e.cfg.Strategy.MinSwitchAPRAdvantage = 5
	e.cfg.Strategy.MinSwitchProfitUSDT = 1
	e.cfg.Strategy.EntryConfirmationScans = 1
	p := &Position{Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 10, SpotCostUSDT: 100, ShortQty: 10, ShortEntryPrice: 10}
	e.state.Positions[p.Symbol] = p
	e.state.FundingStats = map[string]FundingStats{}
	e.state.EntryConfirmations = map[string]int{}
	e.state.CooldownUntil = map[string]time.Time{}
	e.state.FundingStats[p.Symbol] = FundingStats{ConservativeAPRPercent: 24}
	e.state.EntryConfirmations["NEWUSDT"] = 1
	e.tokens = map[string]TokenConfig{"NEWUSDT": {BinanceSymbol: "NEWUSDT", BSCAddress: "0x2222222222222222222222222222222222222222", Decimals: 18}}
	e.chain = &operationTestChain{sellOut: 110}
	now := time.Now()
	markets := map[string]FundingMarket{p.Symbol: {Symbol: p.Symbol, MarkPrice: 9, UpdatedAt: now}}
	candidate := Opportunity{Symbol: "NEWUSDT", TargetNotionalUSDT: 100, NetAPRPercent: 35, ExpectedHoldProfit: 5, Chain: ChainQuote{OutputTokens: 10}, Market: FundingMarket{MarkPrice: 10}}
	e.manageSpreadProfitRotations(context.Background(), now, markets, []Opportunity{candidate})
	if e.state.Positions[p.Symbol] != nil || e.state.Positions[candidate.Symbol] == nil {
		t.Fatalf("profitable position did not rotate to better candidate: positions=%+v", e.state.Positions)
	}
}

func TestWeakFundingPaperPositionStaysOpenWithoutBetterDestination(t *testing.T) {
	e, _ := operationTestEngine(t, false)
	e.cfg.Mode = "paper"
	e.cfg.Strategy.WeakFundingSettlements = 3
	e.cfg.Strategy.MinPositionAgeHours = 24
	e.cfg.Strategy.TargetPositions = 1
	e.state.WeakFundingSettlements = map[string]int{"CAKEUSDT": 3}
	e.state.FundingStats = map[string]FundingStats{"CAKEUSDT": {ConservativeAPRPercent: 8}}
	e.state.EntryConfirmations = map[string]int{}
	e.state.CooldownUntil = map[string]time.Time{}
	p := &Position{Symbol: "CAKEUSDT", TokenAddress: "0x1111111111111111111111111111111111111111", TokenDecimals: 18, TokenQty: 10, SpotCostUSDT: 100, ShortQty: 10, ShortEntryPrice: 10, OpenedAt: time.Now().Add(-100 * time.Hour)}
	e.state.Positions[p.Symbol] = p
	now := time.Now()
	markets := map[string]FundingMarket{p.Symbol: {Symbol: p.Symbol, MarkPrice: 10, UpdatedAt: now}}
	e.rebalance(context.Background(), now, markets, nil)
	if e.state.Positions[p.Symbol] == nil {
		t.Fatal("weak-funding position was closed without a better confirmed destination")
	}
	if !strings.Contains(e.positionDecisions[p.Symbol], "暂无更高净收益候选") {
		t.Fatalf("UI decision did not explain why weak position was retained: %q", e.positionDecisions[p.Symbol])
	}
}
