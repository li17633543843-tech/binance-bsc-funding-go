package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestBinance418StopsHTTPUntilBanExpires(t *testing.T) {
	requests := 0
	banUntil := time.Now().Add(time.Hour).UnixMilli()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"code":-1003,"msg":"Way too many requests; IP banned until %d."}`, banUntil)))
	}))
	defer server.Close()

	client := &BinanceClient{
		cfg:  BinanceConfig{BaseURL: server.URL},
		http: server.Client(),
	}
	if _, err := client.Markets(context.Background()); err == nil {
		t.Fatal("first request should report the Binance IP ban")
	}
	if _, err := client.Markets(context.Background()); err == nil {
		t.Fatal("request attempted during the local ban window should fail locally")
	}
	if requests != 1 {
		t.Fatalf("HTTP requests during active Binance ban = %d, want 1", requests)
	}
}

func TestAnnualizedFunding(t *testing.T) {
	if got := annualizedFundingPercent(0.0003, 1095); math.Abs(got-32.85) > 1e-9 {
		t.Fatalf("got %f", got)
	}
}

func TestFundingStatsUseSettlementsAndResistOneSpike(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	records := make([]FundingRecord, 0, 31)
	for i := 30; i >= 1; i-- {
		records = append(records, FundingRecord{Time: now.Add(-time.Duration(i) * 8 * time.Hour), Rate: .0002, MarkPrice: 1})
	}
	records = append(records, FundingRecord{Time: now, Rate: .01, MarkPrice: 1})
	stats := buildFundingStats(records, .0002, 8, now, now.Add(8*time.Hour), .15)
	if stats.Samples != 31 || stats.PositiveRatio != 1 {
		t.Fatalf("unexpected history sample statistics: %+v", stats)
	}
	if math.Abs(stats.Median30DAPRPercent-21.9) > .01 {
		t.Fatalf("single spike distorted median: %+v", stats)
	}
	if stats.ConservativeAPRPercent > 30 {
		t.Fatalf("single spike distorted conservative APR: %+v", stats)
	}
}

func TestFundingHistoryMergeDeduplicatesAndTrims(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	old := FundingRecord{Time: now.Add(-40 * 24 * time.Hour), Rate: .1}
	kept := FundingRecord{Time: now.Add(-8 * time.Hour), Rate: .0001}
	replacement := FundingRecord{Time: kept.Time, Rate: .0002}
	got := mergeFundingHistory([]FundingRecord{old, kept}, []FundingRecord{replacement}, now.Add(-30*24*time.Hour))
	if len(got) != 1 || got[0].Rate != replacement.Rate {
		t.Fatalf("unexpected merged funding history: %+v", got)
	}
}

func TestPaperFundingAccruesOnlyExactSettlementsOnce(t *testing.T) {
	opened := time.Date(2026, 9, 27, 1, 0, 0, 0, time.UTC)
	settlement := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	e := &Engine{state: &BotState{FundingHistory: map[string][]FundingRecord{
		"CAKEUSDT": {{Time: settlement, Rate: .0002, MarkPrice: 2}},
	}}}
	p := &Position{Symbol: "CAKEUSDT", ShortQty: 100, OpenedAt: opened, LastFundingTime: opened}
	e.accrueFunding(p, FundingMarket{MarkPrice: 3}, settlement.Add(time.Minute))
	if math.Abs(p.FundingAccruedUSDT-.04) > 1e-12 || !p.LastFundingTime.Equal(settlement) {
		t.Fatalf("exact settlement not accrued correctly: %+v", p)
	}
	e.accrueFunding(p, FundingMarket{MarkPrice: 3}, settlement.Add(time.Hour))
	if math.Abs(p.FundingAccruedUSDT-.04) > 1e-12 {
		t.Fatalf("settlement was accrued twice: %+v", p)
	}
}

func TestLiquidationReduction(t *testing.T) {
	if got := reductionFraction(3.1, 3, 1.5, 10); got != 0 {
		t.Fatalf("safe distance reduced: %f", got)
	}
	if got := reductionFraction(1.4, 3, 1.5, 10); got != 1 {
		t.Fatalf("emergency did not close: %f", got)
	}
	if got := reductionFraction(2.9, 3, 1.5, 10); got < .099 || got > .101 {
		t.Fatalf("slow reduction floor wrong: %f", got)
	}
	if got := reductionFraction(2.0, 3, 1.5, 10); got < .099 || got > .101 {
		t.Fatalf("non-emergency reduction must stay gradual: %f", got)
	}
}

func TestPortfolioRiskSelectsOnlyOneConfirmedSymbol(t *testing.T) {
	candidates := []PortfolioRiskCandidate{
		{Symbol: "AAAUSDT", Distance: 2.8, UnrealizedLoss: 20, Notional: 100, ConfirmedBreach: true},
		{Symbol: "BBBUSDT", Distance: 2.6, UnrealizedLoss: 10, Notional: 100, ConfirmedBreach: true},
		{Symbol: "CCCUSDT", Distance: 3.5, UnrealizedLoss: 80, Notional: 300, AccountLevelEntry: true},
	}
	got, ok := choosePortfolioRiskCandidate(candidates)
	if !ok || got.Symbol != "BBBUSDT" {
		t.Fatalf("expected closest confirmed liquidation risk, got=%+v ok=%v", got, ok)
	}
}

func TestPortfolioRiskUsesLargestContributorForAccountStress(t *testing.T) {
	candidates := []PortfolioRiskCandidate{
		{Symbol: "AAAUSDT", Distance: 4, UnrealizedLoss: 20, Notional: 100, AccountLevelEntry: true},
		{Symbol: "BBBUSDT", Distance: 4, UnrealizedLoss: 80, Notional: 200, AccountLevelEntry: true},
	}
	got, ok := choosePortfolioRiskCandidate(candidates)
	if !ok || got.Symbol != "BBBUSDT" {
		t.Fatalf("expected largest account risk contributor, got=%+v ok=%v", got, ok)
	}
}

func TestAccountReductionIsTiered(t *testing.T) {
	cases := []struct {
		ratio float64
		want  float64
	}{{64.9, 0}, {65, .1}, {75, .25}, {80, .5}}
	for _, tc := range cases {
		if got := accountReductionFraction(tc.ratio, 65, 75, 80, 10, 25, 50); math.Abs(got-tc.want) > 1e-12 {
			t.Fatalf("ratio %.1f: got %.3f want %.3f", tc.ratio, got, tc.want)
		}
	}
}

func TestBinanceCrossAccountRiskParsesMarginRatio(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v3/account" || r.Header.Get("X-MBX-APIKEY") != "key" {
			t.Fatalf("unexpected account request: %s key=%q", r.URL.Path, r.Header.Get("X-MBX-APIKEY"))
		}
		_, _ = w.Write([]byte(`{"totalWalletBalance":"4000","totalUnrealizedProfit":"-500","totalMarginBalance":"3500","totalMaintMargin":"2275","availableBalance":"900"}`))
	}))
	defer server.Close()
	client := &BinanceClient{cfg: BinanceConfig{BaseURL: server.URL, RecvWindowMS: 5000}, http: server.Client(), apiKey: "key", secret: "secret"}
	risk, err := client.AccountRisk(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(risk.MarginRatioPercent-65) > 1e-12 || risk.TotalMarginBalance != 3500 || risk.AvailableBalance != 900 {
		t.Fatalf("unexpected account risk: %+v", risk)
	}
}

func TestBinancePositionRisksLoadsPortfolioOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v3/positionRisk" || r.URL.Query().Get("symbol") != "" {
			t.Fatalf("expected one portfolio position request, got %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`[{"symbol":"AAAUSDT","positionAmt":"-100","markPrice":"1","liquidationPrice":"3","unRealizedProfit":"-20"},{"symbol":"EMPTYUSDT","positionAmt":"0","markPrice":"1","liquidationPrice":"0","unRealizedProfit":"0"}]`))
	}))
	defer server.Close()
	client := &BinanceClient{cfg: BinanceConfig{BaseURL: server.URL, RecvWindowMS: 5000}, http: server.Client(), apiKey: "key", secret: "secret"}
	risks, err := client.PositionRisks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(risks) != 1 || risks["AAAUSDT"].PositionAmount != -100 {
		t.Fatalf("unexpected portfolio risks: %+v", risks)
	}
}

func TestMaxSafeNotionalUsesAllCaps(t *testing.T) {
	c := Config{Risk: RiskConfig{TotalCapitalUSDT: 10000, MaxCapitalPerCoinPercent: 5, MaxNotionalPerCoinUSDT: 700, DepthSafetyMultiplier: 2}}
	tok := TokenConfig{MaxNotionalUSDT: 600}
	if got := maxSafeNotional(c, tok, 800); got != 400 {
		t.Fatalf("got %f", got)
	}
}

func TestDepthReductionPolicyHasHysteresisAndFundingGuard(t *testing.T) {
	policy := DepthPolicy{TriggerPercent: 10, MinReductionPercent: 5, EmergencyShortfallPercent: 50}
	if got := depthReductionFraction(105, 100, true, policy); got != 0 {
		t.Fatalf("small depth fluctuation reduced position: %f", got)
	}
	if got := depthReductionFraction(120, 100, true, policy); math.Abs(got-1.0/6.0) > 1e-9 {
		t.Fatalf("confirmed depth reduction fraction wrong: %f", got)
	}
	if got := depthReductionFraction(120, 100, false, policy); got != 0 {
		t.Fatalf("ordinary depth reduction happened before first funding settlement: %f", got)
	}
	if got := depthReductionFraction(250, 100, false, policy); math.Abs(got-.6) > 1e-9 {
		t.Fatalf("emergency depth shortfall was blocked: %f", got)
	}
	if got := depthReductionFraction(100, 0, false, policy); got != 1 {
		t.Fatalf("zero safe capacity did not close position: %f", got)
	}
}

func TestDepthBreachNeedsConsecutiveConfirmations(t *testing.T) {
	count, confirmed := confirmDepthBreach(0, true, 3)
	if count != 1 || confirmed {
		t.Fatalf("first breach unexpectedly confirmed: count=%d confirmed=%v", count, confirmed)
	}
	count, confirmed = confirmDepthBreach(count, true, 3)
	if count != 2 || confirmed {
		t.Fatalf("second breach unexpectedly confirmed: count=%d confirmed=%v", count, confirmed)
	}
	count, confirmed = confirmDepthBreach(count, true, 3)
	if count != 3 || !confirmed {
		t.Fatalf("third breach was not confirmed: count=%d confirmed=%v", count, confirmed)
	}
	count, confirmed = confirmDepthBreach(count, false, 3)
	if count != 0 || confirmed {
		t.Fatalf("healthy scan did not reset breach count: count=%d confirmed=%v", count, confirmed)
	}
}

func TestPartialReductionPnLIncludesProportionalFundingAndCosts(t *testing.T) {
	p := Position{TokenQty: 100, SpotCostUSDT: 200, ShortQty: 100, ShortEntryPrice: 2, FundingAccruedUSDT: 4}
	pnl, funding, fees := partialReductionPnL(p, .25, 49, 1.9, 5, .2)
	if math.Abs(funding-1) > 1e-9 || math.Abs(fees-.24875) > 1e-9 || math.Abs(pnl-2.25125) > 1e-9 {
		t.Fatalf("unexpected partial PnL: pnl=%f funding=%f fees=%f", pnl, funding, fees)
	}
}

func TestLoadStateBackfillsDepthBreachCounters(t *testing.T) {
	dir := t.TempDir()
	oldState := `{"version":1,"positions":{"CAKEUSDT":{"symbol":"CAKEUSDT"}},"daily_date":"2026-09-27"}`
	if err := os.WriteFile(dir+"/state.json", []byte(oldState), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := loadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if state.DepthBreachScans == nil || state.LiquidationBreachScans == nil || state.FundingHistory == nil || state.FundingStats == nil || state.WeakFundingSettlements == nil || state.EntryConfirmations == nil || len(state.Positions) != 1 {
		t.Fatalf("old state was not migrated safely: %+v", state)
	}
}

func TestSwitchNeedsAgeYieldAndProfit(t *testing.T) {
	c := Config{Strategy: StrategyConfig{MinPositionAgeHours: 72, MinSwitchAPRAdvantage: 10, MinSwitchProfitUSDT: 2}}
	current := Opportunity{NetAPRPercent: 20}
	replacement := Opportunity{NetAPRPercent: 35, ExpectedHoldProfit: 5, RoundTripCostUSDT: 1}
	p := Position{OpenedAt: time.Now().Add(-100 * time.Hour)}
	if !shouldSwitch(current, replacement, p, c, time.Now()) {
		t.Fatal("expected switch")
	}
	p.OpenedAt = time.Now().Add(-2 * time.Hour)
	if shouldSwitch(current, replacement, p, c, time.Now()) {
		t.Fatal("switched too early")
	}
}

func TestRankDeterministic(t *testing.T) {
	in := []Opportunity{{Symbol: "B", NetAPRPercent: 10}, {Symbol: "A", NetAPRPercent: 10}, {Symbol: "C", NetAPRPercent: 20}}
	out := rankOpportunities(in)
	if out[0].Symbol != "C" || out[1].Symbol != "A" {
		t.Fatalf("bad order: %+v", out)
	}
}

func TestFloorStep(t *testing.T) {
	if got := floorStep(1.239, 0.01); math.Abs(got-1.23) > 1e-9 {
		t.Fatalf("got %f", got)
	}
}

func TestOKXSignature(t *testing.T) {
	got := okxSignature(
		"test-secret",
		"2026-09-24T00:00:00.000Z",
		"GET",
		"/api/v6/dex/aggregator/quote?amount=100&chainIndex=56",
	)
	const want = "lwZA6XUAaEIBtKjkIT8MSS033nd29Cmcz2EX4NnVB8Y="
	if got != want {
		t.Fatalf("signature mismatch: got %q want %q", got, want)
	}
}

func TestValidateApprovalData(t *testing.T) {
	spender := common.HexToAddress("0x1111111111111111111111111111111111111111")
	approved := big.NewInt(1000)
	data := append([]byte{0x09, 0x5e, 0xa7, 0xb3}, common.LeftPadBytes(spender.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(approved.Bytes(), 32)...)
	encoded := "0x" + hex.EncodeToString(data)

	if _, err := validateApprovalData(encoded, spender, big.NewInt(999)); err != nil {
		t.Fatalf("valid approval rejected: %v", err)
	}
	if _, err := validateApprovalData(encoded, common.HexToAddress("0x2222222222222222222222222222222222222222"), big.NewInt(999)); err == nil {
		t.Fatal("spender mismatch accepted")
	}
	if _, err := validateApprovalData(encoded, spender, big.NewInt(1001)); err == nil {
		t.Fatal("insufficient approval amount accepted")
	}
}

func TestAutoDiscoveryRequiresUniqueExactSymbol(t *testing.T) {
	symbols := map[string]BinanceSymbol{
		"CAKEUSDT": {Symbol: "CAKEUSDT", Base: "CAKE", Quote: "USDT"},
		"DUPUSDT":  {Symbol: "DUPUSDT", Base: "DUP", Quote: "USDT"},
		"BNBUSDT":  {Symbol: "BNBUSDT", Base: "BNB", Quote: "USDT"},
	}
	manual := map[string]TokenConfig{
		"BNBUSDT": {Enabled: true, Symbol: "BNB", BinanceSymbol: "BNBUSDT", BSCAddress: "0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c", Decimals: 18},
	}
	catalog := []OKXCatalogToken{
		{TokenSymbol: "CAKE", TokenContractAddress: "0x0E09FaBB73Bd3Ade0a17ECC321fD13a19e81cE82", Decimals: "18"},
		{TokenSymbol: "DUP", TokenContractAddress: "0x1111111111111111111111111111111111111111", Decimals: "18"},
		{TokenSymbol: "DUP", TokenContractAddress: "0x2222222222222222222222222222222222222222", Decimals: "18"},
	}

	got, autoCount, ambiguous := buildDiscoveredTokens(catalog, symbols, manual, nil, 500)
	if _, ok := got["CAKEUSDT"]; !ok {
		t.Fatal("unique exact symbol was not discovered")
	}
	if _, ok := got["DUPUSDT"]; ok {
		t.Fatal("ambiguous symbol was accepted")
	}
	if _, ok := got["BNBUSDT"]; !ok {
		t.Fatal("manual allowlist token was not preserved")
	}
	if autoCount != 1 || ambiguous != 1 {
		t.Fatalf("unexpected discovery counts: auto=%d ambiguous=%d", autoCount, ambiguous)
	}
}

func TestTokenSearchRequiresOneExactBSCSymbol(t *testing.T) {
	rows := []okxTokenSearchResult{
		{ChainIndex: "56", TokenSymbol: "LYN", TokenName: "Everlyn Token", TokenContractAddress: "0x302DFaF2CDbE51a18d97186A7384e87CF599877D", Decimal: "18"},
		{ChainIndex: "1", TokenSymbol: "LYN", TokenName: "Other chain", TokenContractAddress: "0x1111111111111111111111111111111111111111", Decimal: "18"},
		{ChainIndex: "56", TokenSymbol: "LYNX", TokenName: "Partial symbol", TokenContractAddress: "0x2222222222222222222222222222222222222222", Decimal: "18"},
	}
	got := exactSearchTokens(rows, 56, "lyn")
	if len(got) != 1 || !strings.EqualFold(got[0].TokenContractAddress, "0x302DFaF2CDbE51a18d97186A7384e87CF599877D") {
		t.Fatalf("expected the one exact BSC LYN match, got %#v", got)
	}

	rows = append(rows, okxTokenSearchResult{ChainIndex: "56", TokenSymbol: "LYN", TokenName: "Duplicate symbol", TokenContractAddress: "0x3333333333333333333333333333333333333333", Decimal: "18"})
	if got := exactSearchTokens(rows, 56, "LYN"); len(got) != 2 {
		t.Fatalf("expected two exact BSC LYN addresses to remain ambiguous, got %#v", got)
	}
}

func TestTokenSearchSelectsUniqueMarketMatchedToken(t *testing.T) {
	tokens := []OKXCatalogToken{
		{
			TokenSymbol:          "Q",
			TokenName:            "Q",
			TokenContractAddress: "0x1111111111111111111111111111111111111111",
			Decimals:             "18",
			Price:                "0.04920",
			Liquidity:            "250000",
			CommunityRecognized:  true,
		},
		{
			TokenSymbol:          "Q",
			TokenName:            "Q Copy",
			TokenContractAddress: "0x2222222222222222222222222222222222222222",
			Decimals:             "18",
			Price:                "0.00001",
			Liquidity:            "12",
		},
	}

	got, reason, ok := selectUniqueSearchToken(tokens, 0.049508, 900)
	if !ok || !strings.EqualFold(got.TokenContractAddress, "0x1111111111111111111111111111111111111111") {
		t.Fatalf("expected the unique price/liquidity matched Q token, got=%#v reason=%q ok=%v", got, reason, ok)
	}
}

func TestTokenSearchMetadataIsDecoded(t *testing.T) {
	var env okxEnvelope[okxTokenSearchResult]
	raw := `{"code":"0","data":[{"chainIndex":"56","decimal":"18","tokenContractAddress":"0x1111111111111111111111111111111111111111","tokenName":"Q","tokenSymbol":"Q","price":"0.04920","liquidity":"250000","holders":"12000","tagList":{"communityRecognized":true}}]}`
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	got := exactSearchTokens(env.Data, 56, "Q")
	if len(got) != 1 || !got[0].CommunityRecognized || got[0].Price != "0.04920" || got[0].Liquidity != "250000" {
		t.Fatalf("search metadata was lost: %#v", got)
	}
}

func TestTokenSearchKeepsTrueAmbiguityUnmatched(t *testing.T) {
	tokens := []OKXCatalogToken{
		{TokenSymbol: "LIGHT", TokenContractAddress: "0x1111111111111111111111111111111111111111", Decimals: "18", Price: "0.18", Liquidity: "50000", CommunityRecognized: true},
		{TokenSymbol: "LIGHT", TokenContractAddress: "0x2222222222222222222222222222222222222222", Decimals: "18", Price: "0.181", Liquidity: "48000", CommunityRecognized: true},
	}

	if got, reason, ok := selectUniqueSearchToken(tokens, 0.1802, 900); ok {
		t.Fatalf("two credible LIGHT contracts must remain ambiguous, got=%#v reason=%q", got, reason)
	}
}

func TestFundingWatchIncludesRateEqualToThreshold(t *testing.T) {
	e := &Engine{
		cfg: Config{Strategy: StrategyConfig{MinCurrentFundingBPS: 1, FundingIntervalsPerYear: 1095}},
		symbols: map[string]BinanceSymbol{
			"EQUALUSDT": {Symbol: "EQUALUSDT", Base: "EQUAL", Quote: "USDT"},
			"ABOVEUSDT": {Symbol: "ABOVEUSDT", Base: "ABOVE", Quote: "USDT"},
		},
		tokens: map[string]TokenConfig{
			"EQUALUSDT": {Symbol: "EQUAL", BinanceSymbol: "EQUALUSDT", BSCAddress: "0x1111111111111111111111111111111111111111"},
			"ABOVEUSDT": {Symbol: "ABOVE", BinanceSymbol: "ABOVEUSDT", BSCAddress: "0x2222222222222222222222222222222222222222"},
		},
		fundingHours: map[string]float64{},
	}
	now := time.Now()
	e.updateFundingWatch(map[string]FundingMarket{
		"EQUALUSDT": {Symbol: "EQUALUSDT", FundingRate: 0.0001, MarkPrice: 1, NextFundingTime: now},
		"ABOVEUSDT": {Symbol: "ABOVEUSDT", FundingRate: 0.00010001, MarkPrice: 1, NextFundingTime: now},
	})
	if len(e.fundingWatch) != 2 {
		t.Fatalf("expected rates equal to or above 1 bps, got %#v", e.fundingWatch)
	}
}

func TestDashboardAndLANAuthentication(t *testing.T) {
	e := &Engine{cfg: Config{Dashboard: DashboardConfig{RefreshSeconds: 10, Username: "monitor"}}, health: Health{Status: "ok", Portfolio: map[string]PositionView{}}}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	dashboardHandler(e, "").ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "资金费套利监控台") || !strings.Contains(response.Body.String(), "参数设置") {
		t.Fatalf("dashboard failed: status=%d", response.Code)
	}

	protected := dashboardHandler(e, "secret")
	response = httptest.NewRecorder()
	protected.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated LAN request returned %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.SetBasicAuth("monitor", "secret")
	response = httptest.NewRecorder()
	protected.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated LAN request returned %d", response.Code)
	}
}

func TestDashboardSettingsSaveValidateAndApplyAtCycleBoundary(t *testing.T) {
	cfg, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = t.TempDir()
	e := &Engine{
		cfg:            cfg,
		settingsConfig: cfg,
		state:          &BotState{Positions: map[string]*Position{}},
		health:         Health{Status: "ok", Portfolio: map[string]PositionView{}},
	}
	handler := dashboardHandler(e, "")

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/settings", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("settings GET returned %d: %s", get.Code, get.Body.String())
	}
	if strings.Contains(get.Body.String(), cfg.BSC.WalletAddress) || strings.Contains(get.Body.String(), cfg.OKXDEX.APIKeyEnv) {
		t.Fatalf("settings API exposed protected configuration: %s", get.Body.String())
	}
	var envelope SettingsResponse
	if err := json.Unmarshal(get.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Settings.Strategy.MinCurrentFundingBPS = 6
	envelope.Settings.Risk.MaxDailyLossUSDT = 75
	envelope.Settings.Leverage = 5
	body, _ := json.Marshal(envelope.Settings)
	request := httptest.NewRequest(http.MethodPost, "/v1/settings", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Config-Intent", "save")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("settings POST returned %d: %s", response.Code, response.Body.String())
	}
	var saved SettingsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.AppliedAfterNextCycle || !saved.RestartRequired {
		t.Fatalf("unexpected apply flags: %+v", saved)
	}
	disk, ok, err := loadTuning(cfg.StateDir)
	if err != nil || !ok || disk.Strategy.MinCurrentFundingBPS != 6 || disk.Leverage != 5 {
		t.Fatalf("unexpected persisted tuning ok=%v err=%v data=%+v", ok, err, disk)
	}
	if e.cfg.Strategy.MinCurrentFundingBPS == 6 {
		t.Fatal("runtime tuning applied in the middle of a cycle")
	}
	e.applyPendingSettings()
	if e.cfg.Strategy.MinCurrentFundingBPS != 6 || e.cfg.Risk.MaxDailyLossUSDT != 75 {
		t.Fatalf("runtime tuning was not applied: %+v %+v", e.cfg.Strategy, e.cfg.Risk)
	}
	if e.cfg.Binance.Leverage == 5 {
		t.Fatal("restart-only leverage changed at runtime")
	}
	basePath := cfg.StateDir + string(os.PathSeparator) + "base.json"
	baseJSON, _ := json.Marshal(cfg)
	if err := os.WriteFile(basePath, baseJSON, 0600); err != nil {
		t.Fatal(err)
	}
	effective, err := loadEffectiveConfig(basePath)
	if err != nil || effective.Strategy.MinCurrentFundingBPS != 6 || effective.Binance.Leverage != 5 {
		t.Fatalf("saved tuning did not survive restart: err=%v config=%+v", err, effective)
	}
	disk.Strategy.MinCurrentFundingBPS = 7
	if err := saveTuning(cfg.StateDir, disk); err != nil {
		t.Fatalf("overwrite tuning: %v", err)
	}
	disk, ok, err = loadTuning(cfg.StateDir)
	if err != nil || !ok || disk.Strategy.MinCurrentFundingBPS != 7 {
		t.Fatalf("overwritten tuning was not readable: ok=%v err=%v data=%+v", ok, err, disk)
	}
}

func TestDashboardSettingsRejectInvalidOrUnintentionalWrite(t *testing.T) {
	cfg, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = t.TempDir()
	e := &Engine{cfg: cfg, settingsConfig: cfg, state: &BotState{Positions: map[string]*Position{}}, health: Health{Status: "ok", Portfolio: map[string]PositionView{}}}
	handler := dashboardHandler(e, "")
	tuning := tuningFromConfig(cfg)
	tuning.Strategy.TargetPositions = 50
	body, _ := json.Marshal(tuning)

	request := httptest.NewRequest(http.MethodPost, "/v1/settings", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing intent header returned %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/settings", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Config-Intent", "save")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid capital settings returned %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(tuningPath(cfg.StateDir)); !os.IsNotExist(err) {
		t.Fatalf("invalid settings must not be persisted, stat err=%v", err)
	}
}

func TestDashboardBackgroundRefreshPreservesSettingsForm(t *testing.T) {
	if !strings.Contains(dashboardHTML, "function render(preserveSettingsForm=false)") {
		t.Fatal("dashboard render function has no settings-form preservation mode")
	}
	if !strings.Contains(dashboardHTML, "if(preserveSettingsForm&&$('settingsForm'))return") {
		t.Fatal("background refresh can still rebuild an active settings form")
	}
	if !strings.Contains(dashboardHTML, "render(active==='settings')") {
		t.Fatal("monitor refresh does not request settings-form preservation")
	}
}

func TestLedgerNewestFirstAndLimited(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 3; i++ {
		if err := appendLedger(dir, map[string]any{"event": "open", "sequence": i}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := readLedger(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["sequence"] != float64(3) || rows[1]["sequence"] != float64(2) {
		t.Fatalf("unexpected ledger order: %+v", rows)
	}
}
