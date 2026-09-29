package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Health struct {
	StartedAt               time.Time               `json:"started_at"`
	LastCycle               time.Time               `json:"last_cycle"`
	LastSuccess             time.Time               `json:"last_success"`
	Mode                    string                  `json:"mode"`
	Status                  string                  `json:"status"`
	LastError               string                  `json:"last_error,omitempty"`
	Positions               int                     `json:"positions"`
	Opportunities           int                     `json:"opportunities"`
	DailyRealizedPnL        float64                 `json:"daily_realized_pnl_usdt"`
	ConsecutiveFailures     int                     `json:"consecutive_failures"`
	MonitoredTokens         int                     `json:"monitored_bsc_tokens"`
	AutoDiscovered          int                     `json:"auto_discovered_tokens"`
	AmbiguousSymbols        int                     `json:"ambiguous_symbols_skipped"`
	PositiveFunding         int                     `json:"positive_funding_markets"`
	AccountMarginRatio      float64                 `json:"account_margin_ratio_percent,omitempty"`
	AccountMarginBalance    float64                 `json:"account_margin_balance_usdt,omitempty"`
	AccountAvailableBalance float64                 `json:"account_available_balance_usdt,omitempty"`
	EntryPausedByMargin     bool                    `json:"entry_paused_by_margin"`
	RiskReduceCooldownUntil time.Time               `json:"risk_reduce_cooldown_until,omitempty"`
	BinanceRateLimitUntil   time.Time               `json:"binance_rate_limit_until,omitempty"`
	Portfolio               map[string]PositionView `json:"portfolio"`
}

type PositionView struct {
	Symbol               string  `json:"symbol"`
	NotionalUSDT         float64 `json:"notional_usdt"`
	FundingAccruedUSDT   float64 `json:"funding_accrued_usdt"`
	FundingAPRPercent    float64 `json:"funding_apr_percent"`
	Median30DAPRPercent  float64 `json:"median_30d_apr_percent"`
	PositiveFundingRatio float64 `json:"positive_funding_ratio"`
	WeakSettlements      int     `json:"weak_funding_settlements"`
	HedgeDriftPercent    float64 `json:"hedge_drift_percent"`
	LiquidationDistanceX float64 `json:"liquidation_distance_x,omitempty"`
}

type Engine struct {
	cfg              Config
	binance          *BinanceClient
	chain            *OKXDEXClient
	alert            *Alerter
	state            *BotState
	symbols          map[string]BinanceSymbol
	tokens           map[string]TokenConfig
	fundingHours     map[string]float64
	manualTokens     map[string]TokenConfig
	fundingWatch     []FundingWatch
	nextDiscovery    time.Time
	nextChainScan    time.Time
	tokenSearchAt    map[string]time.Time
	tokenMatchStatus map[string]string
	autoDiscovered   int
	ambiguousSymbols int
	accountRisk      FuturesAccountRisk
	positionRisks    map[string]PositionRisk
	entryRiskPaused  bool
	settingsMu       sync.Mutex
	settingsConfig   Config
	pendingStrategy  *StrategyConfig
	pendingRisk      *RiskConfig
	mu               sync.RWMutex
	health           Health
	opportunities    []Opportunity
}

func NewEngine(ctx context.Context, c Config) (*Engine, error) {
	s, err := loadState(c.StateDir)
	if err != nil {
		return nil, err
	}
	b := NewBinanceClient(c.Binance)
	privateKey := ""
	if c.Mode == "live" {
		privateKey = os.Getenv(c.BSC.PrivateKeyEnv)
		if privateKey == "" {
			return nil, fmt.Errorf("live mode requires %s", c.BSC.PrivateKeyEnv)
		}
		if b.apiKey == "" || b.secret == "" {
			return nil, fmt.Errorf("live mode requires Binance credentials")
		}
	}
	chain, err := NewOKXDEXClient(ctx, c.BSC, c.OKXDEX, privateKey)
	if err != nil {
		return nil, err
	}
	symbols, err := b.Symbols(ctx)
	if err != nil {
		chain.Close()
		return nil, err
	}
	manualTokens := map[string]TokenConfig{}
	for _, t := range c.Tokens {
		if t.Enabled {
			if _, ok := symbols[t.BinanceSymbol]; !ok {
				chain.Close()
				return nil, fmt.Errorf("configured Binance symbol %s is not a tradable USDT perpetual", t.BinanceSymbol)
			}
			manualTokens[t.BinanceSymbol] = t
		}
	}
	fundingHours, intervalErr := b.FundingIntervals(ctx)
	if intervalErr != nil {
		slog.Warn("funding interval lookup failed; using configured fallback", "error", intervalErr)
		fundingHours = map[string]float64{}
	}
	tokens := make(map[string]TokenConfig, len(manualTokens))
	for symbol, token := range manualTokens {
		tokens[symbol] = token
	}
	e := &Engine{cfg: c, settingsConfig: c, binance: b, chain: chain, alert: NewAlerter(c.Alerts), state: s, symbols: symbols, tokens: tokens, manualTokens: manualTokens, fundingHours: fundingHours, tokenSearchAt: map[string]time.Time{}, tokenMatchStatus: map[string]string{}, positionRisks: map[string]PositionRisk{}}
	if c.Discovery.Enabled {
		if err := e.refreshDiscovery(ctx, true); err != nil {
			if len(manualTokens) == 0 {
				chain.Close()
				return nil, fmt.Errorf("initial token auto-discovery failed: %w", err)
			}
			slog.Warn("initial token auto-discovery failed; using manual fallback", "error", err)
		}
	}
	e.restorePositionTokens(ctx)
	e.health = Health{StartedAt: s.StartedAt, Mode: c.Mode, Status: "starting", Portfolio: map[string]PositionView{}}
	return e, nil
}

func (e *Engine) Close() { e.chain.Close() }

func (e *Engine) restorePositionTokens(ctx context.Context) {
	if len(e.state.Positions) == 0 {
		return
	}
	restored, failed := 0, 0
	for symbol, position := range e.state.Positions {
		rule, supported := e.symbols[symbol]
		if !supported || !addressPattern.MatchString(position.TokenAddress) {
			slog.Warn("cannot restore position token", "symbol", symbol, "address", position.TokenAddress, "reason", "invalid symbol or address")
			failed++
			continue
		}
		if token, ok := e.tokens[symbol]; ok && strings.EqualFold(token.BSCAddress, position.TokenAddress) {
			position.TokenDecimals = token.Decimals
			continue
		}
		matches, err := e.chain.SearchTokenAddress(ctx, position.TokenAddress)
		if err != nil || len(matches) != 1 || !strings.EqualFold(strings.TrimSpace(matches[0].TokenSymbol), rule.Base) {
			slog.Warn("cannot restore position token", "symbol", symbol, "address", position.TokenAddress, "matches", len(matches), "error", err)
			failed++
			continue
		}
		decimals, err := strconv.ParseUint(matches[0].Decimals, 10, 8)
		if err != nil || decimals > 36 {
			slog.Warn("cannot restore position token decimals", "symbol", symbol, "address", position.TokenAddress)
			failed++
			continue
		}
		e.tokens[symbol] = TokenConfig{Enabled: true, Symbol: rule.Base, BinanceSymbol: symbol, BSCAddress: matches[0].TokenContractAddress, Decimals: uint8(decimals), MaxNotionalUSDT: e.cfg.Risk.MaxNotionalPerCoinUSDT, VerifiedContract: true}
		position.TokenDecimals = uint8(decimals)
		restored++
	}
	if restored > 0 {
		e.autoDiscovered += restored
		e.persist()
	}
	slog.Info("position token restoration completed", "positions", len(e.state.Positions), "restored", restored, "failed", failed)
}

func buildDiscoveredTokens(catalog []OKXCatalogToken, symbols map[string]BinanceSymbol, manual map[string]TokenConfig, deny []string, maxNotional float64) (map[string]TokenConfig, int, int) {
	bySymbol := map[string]map[string]OKXCatalogToken{}
	for _, token := range catalog {
		symbol := strings.ToUpper(strings.TrimSpace(token.TokenSymbol))
		address := strings.ToLower(strings.TrimSpace(token.TokenContractAddress))
		decimals, err := strconv.ParseUint(token.Decimals, 10, 8)
		if symbol == "" || !addressPattern.MatchString(address) || strings.EqualFold(address, "0x0000000000000000000000000000000000000000") || err != nil || decimals > 36 {
			continue
		}
		if bySymbol[symbol] == nil {
			bySymbol[symbol] = map[string]OKXCatalogToken{}
		}
		bySymbol[symbol][address] = token
	}
	denied := map[string]bool{}
	for _, symbol := range deny {
		denied[symbol] = true
	}
	out := make(map[string]TokenConfig, len(manual)+len(symbols))
	usedAddress := map[string]bool{}
	for symbol, token := range manual {
		out[symbol] = token
		usedAddress[strings.ToLower(token.BSCAddress)] = true
	}
	autoCount, ambiguous := 0, 0
	for binanceSymbol, binanceToken := range symbols {
		if _, exists := out[binanceSymbol]; exists || denied[binanceToken.Base] {
			continue
		}
		matches := bySymbol[binanceToken.Base]
		if len(matches) != 1 {
			if len(matches) > 1 {
				ambiguous++
			}
			continue
		}
		for address, token := range matches {
			if usedAddress[address] {
				continue
			}
			decimals, _ := strconv.ParseUint(token.Decimals, 10, 8)
			out[binanceSymbol] = TokenConfig{Enabled: true, Symbol: binanceToken.Base, BinanceSymbol: binanceSymbol, BSCAddress: token.TokenContractAddress, Decimals: uint8(decimals), MaxNotionalUSDT: maxNotional, VerifiedContract: true}
			usedAddress[address] = true
			autoCount++
		}
	}
	return out, autoCount, ambiguous
}

func (e *Engine) refreshDiscovery(ctx context.Context, force bool) error {
	if !e.cfg.Discovery.Enabled {
		return nil
	}
	now := time.Now()
	if !force && now.Before(e.nextDiscovery) {
		return nil
	}
	catalog, err := e.chain.TokenCatalog(ctx)
	if err != nil {
		return err
	}
	tokens, autoCount, ambiguous := buildDiscoveredTokens(catalog, e.symbols, e.manualTokens, e.cfg.Discovery.DenySymbols, e.cfg.Risk.MaxNotionalPerCoinUSDT)
	if len(tokens) == 0 {
		return fmt.Errorf("OKX DEX catalog produced no unambiguous BSC/Binance matches")
	}
	e.tokens = tokens
	e.autoDiscovered = autoCount
	e.ambiguousSymbols = ambiguous
	e.nextDiscovery = now.Add(time.Duration(e.cfg.Discovery.RefreshMinutes) * time.Minute)
	slog.Info("token auto-discovery refreshed", "catalog_tokens", len(catalog), "matched_tokens", len(tokens), "auto_discovered", autoCount, "ambiguous_skipped", ambiguous)
	return nil
}

func (e *Engine) enrichDiscoveryFromSearch(ctx context.Context, markets map[string]FundingMarket) {
	if !e.cfg.Discovery.Enabled {
		return
	}
	now := time.Now()
	refreshAfter := time.Duration(e.cfg.Discovery.RefreshMinutes) * time.Minute
	denied := map[string]bool{}
	for _, symbol := range e.cfg.Discovery.DenySymbols {
		denied[symbol] = true
	}
	type candidate struct {
		binanceSymbol string
		base          string
		markPrice     float64
	}
	var candidates []candidate
	for symbol, market := range markets {
		rule, ok := e.symbols[symbol]
		if !ok || market.FundingRate*10000 < e.cfg.Strategy.MinCurrentFundingBPS || denied[rule.Base] {
			continue
		}
		if _, matched := e.tokens[symbol]; matched {
			continue
		}
		if last := e.tokenSearchAt[symbol]; !last.IsZero() && now.Sub(last) < refreshAfter {
			continue
		}
		e.tokenSearchAt[symbol] = now
		candidates = append(candidates, candidate{binanceSymbol: symbol, base: rule.Base, markPrice: market.MarkPrice})
	}
	if len(candidates) == 0 {
		return
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].binanceSymbol < candidates[j].binanceSymbol })

	type result struct {
		candidate candidate
		tokens    []OKXCatalogToken
		err       error
	}
	jobs := make(chan candidate)
	results := make(chan result, len(candidates))
	workers := 4
	if len(candidates) < workers {
		workers = len(candidates)
	}
	pace := time.NewTicker(150 * time.Millisecond)
	defer pace.Stop()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				select {
				case <-ctx.Done():
					results <- result{candidate: item, err: ctx.Err()}
					continue
				case <-pace.C:
				}
				tokens, err := e.chain.SearchTokens(ctx, item.base)
				results <- result{candidate: item, tokens: tokens, err: err}
			}
		}()
	}
	go func() {
		for _, item := range candidates {
			jobs <- item
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	usedAddress := map[string]bool{}
	for _, token := range e.tokens {
		usedAddress[strings.ToLower(token.BSCAddress)] = true
	}
	added, metadataResolved, ambiguous, failed := 0, 0, 0, 0
	minLiquidity := math.Max(100, e.cfg.Risk.TargetNotionalPerCoinUSDT*e.cfg.Risk.DepthSafetyMultiplier)
	for found := range results {
		if found.err != nil {
			failed++
			e.tokenMatchStatus[found.candidate.binanceSymbol] = "search_error"
			continue
		}
		token, reason, matched := selectUniqueSearchToken(found.tokens, found.candidate.markPrice, minLiquidity)
		if !matched {
			e.tokenMatchStatus[found.candidate.binanceSymbol] = reason
			if len(found.tokens) > 1 {
				ambiguous++
			}
			continue
		}
		if reason != "unique_exact_symbol" {
			metadataResolved++
		}
		address := strings.ToLower(token.TokenContractAddress)
		if usedAddress[address] {
			e.tokenMatchStatus[found.candidate.binanceSymbol] = "address_already_used"
			continue
		}
		decimals, decimalsErr := strconv.ParseUint(token.Decimals, 10, 8)
		if decimalsErr != nil || decimals > 36 {
			e.tokenMatchStatus[found.candidate.binanceSymbol] = "invalid_decimals"
			continue
		}
		e.tokens[found.candidate.binanceSymbol] = TokenConfig{Enabled: true, Symbol: found.candidate.base, BinanceSymbol: found.candidate.binanceSymbol, BSCAddress: token.TokenContractAddress, Decimals: uint8(decimals), MaxNotionalUSDT: e.cfg.Risk.MaxNotionalPerCoinUSDT, VerifiedContract: true}
		e.tokenMatchStatus[found.candidate.binanceSymbol] = reason
		usedAddress[address] = true
		added++
	}
	e.autoDiscovered += added
	e.ambiguousSymbols += ambiguous
	slog.Info("OKX token-search discovery completed", "searched_symbols", len(candidates), "unique_matches_added", added, "metadata_resolved", metadataResolved, "ambiguous_skipped", ambiguous, "failed_searches", failed)
}

func (e *Engine) updateFundingWatch(markets map[string]FundingMarket) {
	watch := make([]FundingWatch, 0, len(markets))
	for symbol, market := range markets {
		binanceToken, supported := e.symbols[symbol]
		fundingBPS := market.FundingRate * 10000
		if !supported || fundingBPS < e.cfg.Strategy.MinCurrentFundingBPS {
			continue
		}
		token, matched := e.tokens[symbol]
		status := e.tokenMatchStatus[symbol]
		if matched && status == "" {
			status = "catalog_or_manual_match"
		}
		watch = append(watch, FundingWatch{Symbol: symbol, BaseAsset: binanceToken.Base, FundingBPS: fundingBPS, SimpleAPRPercent: annualizedFundingPercent(market.FundingRate, e.intervalsPerYear(symbol)), MarkPrice: market.MarkPrice, NextFundingTime: market.NextFundingTime, BSCSpotMatched: matched, BSCAddress: token.BSCAddress, BSCMatchStatus: status, EligibleForQuotes: matched})
	}
	sort.Slice(watch, func(i, j int) bool {
		if watch[i].FundingBPS == watch[j].FundingBPS {
			return watch[i].Symbol < watch[j].Symbol
		}
		return watch[i].FundingBPS > watch[j].FundingBPS
	})
	e.mu.Lock()
	e.fundingWatch = watch
	e.mu.Unlock()
}

func (e *Engine) Run(ctx context.Context) error {
	e.runCycle(ctx)
	ticker := time.NewTicker(time.Duration(e.cfg.ScanSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			e.runCycle(ctx)
		}
	}
}

func (e *Engine) runCycle(ctx context.Context) {
	e.applyPendingSettings()
	now := time.Now()
	if e.pauseForBinanceRateLimit(now) {
		return
	}
	failuresAtStart := e.state.ConsecutiveFailures
	e.resetDaily(now)
	// Existing live positions get liquidation checks before candidate ranking or chain depth scans.
	e.manageLiquidationRisk(ctx, now)
	if e.pauseForBinanceRateLimit(time.Now()) {
		e.persist()
		return
	}
	markets, err := e.binance.Markets(ctx)
	if err != nil {
		e.fail(ctx, "markets", err)
		e.persist()
		return
	}
	if err := e.refreshDiscovery(ctx, false); err != nil {
		slog.Warn("token auto-discovery refresh failed; keeping previous catalog", "error", err)
	}
	e.enrichDiscoveryFromSearch(ctx, markets)
	e.updateFundingWatch(markets)
	if err := e.updateFunding(ctx, markets); err != nil {
		e.fail(ctx, "funding-history", err)
		e.persist()
		return
	}
	e.manageExisting(ctx, now, markets)
	if !now.Before(e.nextChainScan) {
		e.manageDepthSpreadAndHedge(ctx, now, markets)
		opps, scanErr := e.scanOpportunities(ctx, markets)
		if scanErr != nil {
			slog.Warn("candidate scan partially failed", "error", scanErr)
			e.nextChainScan = time.Now().Add(5 * time.Minute)
		} else {
			e.nextChainScan = time.Now().Add(time.Duration(e.cfg.Strategy.ChainQuoteRefreshMinutes) * time.Minute)
		}
		ranked := rankOpportunities(opps)
		e.rebalance(ctx, now, markets, ranked)
		e.mu.Lock()
		e.opportunities = ranked
		e.mu.Unlock()
	}
	cleanCycle := e.state.ConsecutiveFailures == failuresAtStart
	if cleanCycle {
		e.state.ConsecutiveFailures = 0
	}
	e.persist()
	e.mu.Lock()
	e.health.LastCycle = now
	e.health.LastSuccess = time.Now()
	if cleanCycle {
		e.health.Status = "ok"
		e.health.LastError = ""
		e.health.BinanceRateLimitUntil = time.Time{}
	} else {
		e.health.Status = "degraded"
	}
	e.refreshHealthLocked(markets)
	e.mu.Unlock()
}

func (e *Engine) pauseForBinanceRateLimit(now time.Time) bool {
	until := e.binance.RateLimitUntil()
	if !until.After(now) {
		return false
	}
	e.mu.Lock()
	e.health.LastCycle = now
	e.health.Status = "degraded"
	e.health.BinanceRateLimitUntil = until
	e.health.LastError = fmt.Sprintf("Binance IP rate-limit cooldown until %s; no request sent", until.Format(time.RFC3339))
	e.mu.Unlock()
	return true
}

func (e *Engine) resetDaily(now time.Time) {
	d := now.Format("2006-01-02")
	if e.state.DailyDate != d {
		e.state.DailyDate = d
		e.state.DailyRealizedPnL = 0
	}
}

func (e *Engine) updateFunding(ctx context.Context, markets map[string]FundingMarket) error {
	now := time.Now()
	cutoff := now.Add(-time.Duration(e.cfg.Strategy.FundingHistoryDays) * 24 * time.Hour)
	for symbol := range e.tokens {
		m, ok := markets[symbol]
		if !ok {
			continue
		}
		history := e.state.FundingHistory[symbol]
		stats := e.state.FundingStats[symbol]
		refresh := len(history) == 0 || stats.ObservedNextFundingTime.IsZero() || !m.NextFundingTime.Equal(stats.ObservedNextFundingTime)
		observedNext := m.NextFundingTime
		if refresh {
			previousLatest := time.Time{}
			if len(history) > 0 {
				previousLatest = history[len(history)-1].Time
			}
			start := cutoff
			if len(history) > 0 {
				start = history[len(history)-1].Time.Add(time.Millisecond)
			}
			records, err := e.binance.FundingHistory(ctx, symbol, start, 1000)
			if err != nil {
				var rateLimit *BinanceRateLimitError
				if errors.As(err, &rateLimit) {
					return err
				}
				slog.Warn("funding history refresh failed; retaining cached history", "symbol", symbol, "error", err)
				// Keep the prior marker so the next scan retries this settlement instead of
				// silently treating the failed refresh as complete.
				observedNext = stats.ObservedNextFundingTime
			} else {
				history = mergeFundingHistory(history, records, cutoff)
				e.state.FundingHistory[symbol] = history
				// Binance can advance nextFundingTime a little before the completed
				// funding row becomes queryable. Retry until a genuinely newer row
				// arrives so paper funding cannot silently miss a settlement.
				if len(history) == 0 || (!previousLatest.IsZero() && !history[len(history)-1].Time.After(previousLatest)) {
					observedNext = stats.ObservedNextFundingTime
				}
			}
		}
		intervalHours := 365 * 24 / e.intervalsPerYear(symbol)
		stats = buildFundingStats(history, m.FundingRate, intervalHours, now, observedNext, e.cfg.Strategy.FundingEWMAAlpha)
		e.state.FundingStats[symbol] = stats
		e.state.FundingSamples[symbol] = stats.Samples
		e.state.FundingEWMA[symbol] = stats.ConservativeAPRPercent / e.intervalsPerYear(symbol) / 100
	}
	return nil
}

func (e *Engine) manageLiquidationRisk(ctx context.Context, now time.Time) {
	if e.cfg.Mode != "live" {
		return
	}
	account, err := e.binance.AccountRisk(ctx)
	if err != nil {
		// Fail closed: without current cross-margin account data, opening or
		// rotating positions could increase exposure while risk is unknown.
		e.entryRiskPaused = true
		e.fail(ctx, "account-risk", err)
		return
	}
	risks, err := e.binance.PositionRisks(ctx)
	if err != nil {
		e.entryRiskPaused = true
		e.fail(ctx, "position-risks", err)
		return
	}
	e.accountRisk = account
	e.positionRisks = risks
	e.entryRiskPaused = account.MarginRatioPercent >= e.cfg.Risk.AccountMarginStopEntryPercent

	accountBreached := account.MarginRatioPercent >= e.cfg.Risk.AccountMarginReducePercent
	var accountConfirmed bool
	e.state.AccountMarginBreachScans, accountConfirmed = confirmDepthBreach(e.state.AccountMarginBreachScans, accountBreached, e.cfg.Risk.LiquidationBreachConfirmations)
	accountEmergency := account.MarginRatioPercent >= e.cfg.Risk.AccountMarginEmergencyPercent
	if accountEmergency {
		accountConfirmed = true
	}

	candidates := make([]PortfolioRiskCandidate, 0, len(e.state.Positions))
	hasEmergency := accountEmergency
	for symbol := range e.state.Positions {
		risk, ok := risks[symbol]
		if !ok {
			e.fail(ctx, "position-risk-"+symbol, fmt.Errorf("live Binance position is missing"))
			continue
		}
		d := liquidationDistanceX(risk)
		breached := d < e.cfg.Risk.MinLiquidationDistanceX
		count, confirmed := confirmDepthBreach(e.state.LiquidationBreachScans[symbol], breached, e.cfg.Risk.LiquidationBreachConfirmations)
		e.state.LiquidationBreachScans[symbol] = count
		emergency := d <= e.cfg.Risk.EmergencyLiquidationDistanceX
		if emergency {
			confirmed = true
			hasEmergency = true
		}
		candidates = append(candidates, PortfolioRiskCandidate{
			Symbol:            symbol,
			Distance:          d,
			UnrealizedLoss:    math.Max(0, -risk.UnrealizedProfit),
			Notional:          math.Abs(risk.PositionAmount * risk.MarkPrice),
			ConfirmedBreach:   confirmed,
			EmergencyBreach:   emergency,
			AccountLevelEntry: accountConfirmed,
		})
	}
	if now.Before(e.state.RiskReduceCooldownUntil) && !hasEmergency {
		return
	}
	selected, ok := choosePortfolioRiskCandidate(candidates)
	if !ok {
		return
	}
	risk := risks[selected.Symbol]
	fraction := reductionFraction(selected.Distance, e.cfg.Risk.MinLiquidationDistanceX, e.cfg.Risk.EmergencyLiquidationDistanceX, e.cfg.Risk.SlowReducePercent)
	accountFraction := accountReductionFraction(account.MarginRatioPercent, e.cfg.Risk.AccountMarginReducePercent, e.cfg.Risk.AccountMarginHighPercent, e.cfg.Risk.AccountMarginEmergencyPercent, e.cfg.Risk.SlowReducePercent, e.cfg.Risk.AccountHighReducePercent, e.cfg.Risk.AccountEmergencyReducePercent)
	if accountConfirmed && accountFraction > fraction {
		fraction = accountFraction
	}
	if fraction <= 0 {
		return
	}
	reason := "account_margin_ratio"
	if selected.EmergencyBreach {
		reason = "liquidation_emergency"
	} else if selected.ConfirmedBreach {
		reason = "liquidation_distance"
	}
	_ = e.alert.Send(ctx, "cross-margin-risk-"+selected.Symbol, "CRITICAL", fmt.Sprintf("%s cross risk: liquidation %.2fx, account margin %.2f%%, reducing %.0f%%", selected.Symbol, selected.Distance, account.MarginRatioPercent, fraction*100))
	riskMarket := FundingMarket{Symbol: selected.Symbol, MarkPrice: risk.MarkPrice, IndexPrice: risk.MarkPrice, UpdatedAt: now}
	if err := e.reducePosition(ctx, e.state.Positions[selected.Symbol], fraction, reason, &riskMarket); err != nil {
		e.fail(ctx, "reduce-"+selected.Symbol, err)
		return
	}
	if _, stillOpen := e.state.Positions[selected.Symbol]; stillOpen {
		e.state.LiquidationBreachScans[selected.Symbol] = 0
	} else {
		delete(e.state.LiquidationBreachScans, selected.Symbol)
	}
	e.state.RiskReduceCooldownUntil = now.Add(time.Duration(e.cfg.Risk.RiskReduceCooldownMinutes) * time.Minute)
}

func (e *Engine) manageExisting(ctx context.Context, now time.Time, markets map[string]FundingMarket) {
	for symbol, p := range e.state.Positions {
		m, ok := markets[symbol]
		if !ok || time.Since(m.UpdatedAt) > time.Duration(e.cfg.Risk.MaxDataAgeSeconds)*time.Second {
			e.fail(ctx, "stale-"+symbol, fmt.Errorf("missing or stale market"))
			continue
		}
		e.accrueFunding(p, m, now)
		stats := e.state.FundingStats[symbol]
		if stats.LastSettlementTime.After(p.OpenedAt) && stats.LastSettlementTime.After(p.LastFundingAssessmentTime) {
			latestRate := 0.0
			history := e.state.FundingHistory[symbol]
			if len(history) > 0 {
				latestRate = history[len(history)-1].Rate
			}
			if latestRate*10000 < e.cfg.Strategy.MinCurrentFundingBPS || stats.ConservativeAPRPercent < e.cfg.Strategy.MinFundingAPRPercent {
				e.state.WeakFundingSettlements[symbol]++
			} else {
				e.state.WeakFundingSettlements[symbol] = 0
			}
			p.LastFundingAssessmentTime = stats.LastSettlementTime
		}
		if e.state.WeakFundingSettlements[symbol] >= e.cfg.Strategy.WeakFundingSettlements && now.Sub(p.OpenedAt).Hours() >= e.cfg.Strategy.MinPositionAgeHours {
			slog.Info("funding persistently weak; position eligible for replacement", "symbol", symbol, "weak_settlements", e.state.WeakFundingSettlements[symbol])
		}
	}
}

func (e *Engine) manageDepthSpreadAndHedge(ctx context.Context, now time.Time, markets map[string]FundingMarket) {
	policy := DepthPolicy{
		TriggerPercent:            e.cfg.Risk.DepthReductionTriggerPercent,
		MinReductionPercent:       e.cfg.Risk.MinDepthReductionPercent,
		EmergencyShortfallPercent: e.cfg.Risk.DepthEmergencyShortfallPct,
	}
	for symbol, p := range e.state.Positions {
		m, ok := markets[symbol]
		if !ok {
			continue
		}
		t, tokenAvailable := e.tokens[symbol]
		if !tokenAvailable || !addressPattern.MatchString(t.BSCAddress) {
			slog.Warn("position chain checks skipped; token configuration unavailable", "symbol", symbol, "address", p.TokenAddress)
			continue
		}
		current := p.TokenQty * m.IndexPrice
		capacity, exitQuote, err := e.chain.PositionExitCapacity(ctx, t, p.TokenQty, m.IndexPrice, e.cfg.Risk.MaxChainPriceImpactBPS)
		if err != nil {
			e.fail(ctx, "position-depth-"+symbol, err)
			continue
		}
		fundingSettled := p.LastFundingTime.After(p.OpenedAt)
		fraction := positionDepthReductionFraction(current, capacity, fundingSettled, policy)
		breached := capacity+1e-9 < current
		breachCount, confirmed := confirmDepthBreach(e.state.DepthBreachScans[symbol], breached, e.cfg.Risk.DepthBreachConfirmations)
		e.state.DepthBreachScans[symbol] = breachCount
		if breached && !confirmed {
			slog.Warn("position sell depth breach awaiting confirmation", "symbol", symbol, "scan", breachCount, "required", e.cfg.Risk.DepthBreachConfirmations, "current_usdt", current, "safe_sell_chunk_usdt", capacity, "sell_impact_bps", exitQuote.PriceImpactBPS, "planned_reduction_percent", fraction*100)
		}
		if fraction > 0 && confirmed {
			_ = e.alert.Send(ctx, "depth-reduce-"+symbol, "WARNING", fmt.Sprintf("%s position %.2f exceeds executable sell chunk %.2f USDT; reducing %.1f%%", symbol, current, capacity, fraction*100))
			if err := e.reducePosition(ctx, p, fraction, "chain_depth_limit", &m); err != nil {
				e.fail(ctx, "depth-reduce-"+symbol, err)
				continue
			}
			e.state.DepthBreachScans[symbol] = 0
		}
		if breached && confirmed && capacity <= 0 && breachCount == e.cfg.Risk.DepthBreachConfirmations {
			slog.Error("no sell chunk meets chain impact limit; holding position instead of blind close", "symbol", symbol, "current_usdt", current, "smallest_probe_impact_bps", exitQuote.PriceImpactBPS)
			_ = e.alert.Send(ctx, "depth-hold-"+symbol, "CRITICAL", fmt.Sprintf("%s has no sell chunk inside the %.2f bps impact limit; position is held and new entry remains blocked", symbol, e.cfg.Risk.MaxChainPriceImpactBPS))
		}
		if err := e.correctHedgeDrift(ctx, p); err != nil {
			e.fail(ctx, "hedge-drift-"+symbol, err)
		}
		exitUSDT, err := e.chain.SellQuote(ctx, t, p.TokenQty)
		if err != nil {
			e.fail(ctx, "spread-quote-"+symbol, err)
			continue
		}
		shortPnL := p.ShortQty * (p.ShortEntryPrice - m.MarkPrice)
		futuresFees := p.ShortQty * (p.ShortEntryPrice + m.MarkPrice) * e.cfg.Binance.TakerFeeBPS / 10000
		net := exitUSDT - p.SpotCostUSDT + shortPnL + p.FundingAccruedUSDT - futuresFees - e.cfg.BSC.GasReserveUSDT
		gross := p.SpotCostUSDT + p.ShortQty*p.ShortEntryPrice
		returnBPS := 0.0
		if gross > 0 {
			returnBPS = net / gross * 10000
		}
		if net >= e.cfg.Strategy.MinPriceArbNetUSDT && returnBPS >= e.cfg.Strategy.PriceSpreadTakeProfitBPS {
			if err := e.closePosition(ctx, p, m, "price_spread_take_profit"); err != nil {
				e.fail(ctx, "spread-close-"+symbol, err)
			} else {
				e.state.CooldownUntil[symbol] = now.Add(time.Duration(e.cfg.Strategy.PriceArbCooldownHours * float64(time.Hour)))
			}
		}
	}
}

func (e *Engine) correctHedgeDrift(ctx context.Context, p *Position) error {
	denom := math.Max(p.TokenQty, p.ShortQty)
	if denom <= 0 {
		return nil
	}
	drift := abs(p.TokenQty-p.ShortQty) / denom * 100
	if drift <= e.cfg.Risk.MaxHedgeDriftPercent {
		return nil
	}
	_ = e.alert.Send(ctx, "hedge-drift-"+p.Symbol, "WARNING", fmt.Sprintf("%s hedge drift %.2f%%; correcting", p.Symbol, drift))
	if e.cfg.Mode != "live" {
		p.ShortQty = p.TokenQty
		p.LastAdjustedAt = time.Now()
		return nil
	}
	rule := e.symbols[p.Symbol]
	delta := floorStep(abs(p.TokenQty-p.ShortQty), rule.StepSize)
	if delta < rule.MinQty || delta <= 0 {
		return fmt.Errorf("%s hedge drift is above limit but %.8f is below minimum order", p.Symbol, delta)
	}
	if p.TokenQty > p.ShortQty {
		fill, err := e.binance.MarketOrder(ctx, p.Symbol, "SELL", delta, false)
		if err != nil {
			return err
		}
		p.ShortQty += fill.ExecutedQty
	} else {
		fill, err := e.binance.MarketOrder(ctx, p.Symbol, "BUY", delta, true)
		if err != nil {
			return err
		}
		p.ShortQty -= fill.ExecutedQty
	}
	p.LastAdjustedAt = time.Now()
	return appendLedger(e.cfg.StateDir, map[string]any{"time": time.Now(), "event": "hedge_correction", "symbol": p.Symbol, "token_qty": p.TokenQty, "short_qty": p.ShortQty})
}

func (e *Engine) accrueFunding(p *Position, m FundingMarket, now time.Time) {
	if p.LastFundingTime.IsZero() {
		p.LastFundingTime = p.OpenedAt
	}
	for _, record := range e.state.FundingHistory[p.Symbol] {
		if !record.Time.After(p.LastFundingTime) || !record.Time.After(p.OpenedAt) || record.Time.After(now) {
			continue
		}
		markPrice := record.MarkPrice
		if markPrice <= 0 {
			markPrice = m.MarkPrice
		}
		p.FundingAccruedUSDT += p.ShortQty * markPrice * record.Rate
		p.LastFundingTime = record.Time
	}
}

func (e *Engine) scanOpportunities(ctx context.Context, markets map[string]FundingMarket) ([]Opportunity, error) {
	type candidate struct {
		token  TokenConfig
		market FundingMarket
		apr    float64
	}
	type result struct {
		o   Opportunity
		err error
	}
	var candidates []candidate
	for symbol, token := range e.tokens {
		m, ok := markets[symbol]
		if !ok || m.FundingRate*10000 < e.cfg.Strategy.MinCurrentFundingBPS {
			continue
		}
		stats := e.state.FundingStats[token.BinanceSymbol]
		apr := stats.ConservativeAPRPercent
		currentAPR := annualizedFundingPercent(m.FundingRate, e.intervalsPerYear(token.BinanceSymbol))
		if stats.Samples < e.cfg.Strategy.MinFundingHistorySamples || stats.PositiveRatio < e.cfg.Strategy.MinPositiveFundingRatio || stats.Median30DAPRPercent <= 0 || currentAPR > stats.Median30DAPRPercent*e.cfg.Strategy.MaxCurrentToMedianRatio || apr < e.cfg.Strategy.MinFundingAPRPercent {
			continue
		}
		candidates = append(candidates, candidate{token: token, market: m, apr: apr})
	}
	sort.Slice(candidates, func(i, j int) bool {
		_, iHeld := e.state.Positions[candidates[i].token.BinanceSymbol]
		_, jHeld := e.state.Positions[candidates[j].token.BinanceSymbol]
		if iHeld != jHeld {
			return iHeld
		}
		if candidates[i].apr == candidates[j].apr {
			return candidates[i].token.BinanceSymbol < candidates[j].token.BinanceSymbol
		}
		return candidates[i].apr > candidates[j].apr
	})
	if len(candidates) > e.cfg.Strategy.MaxQuoteCandidates {
		candidates = candidates[:e.cfg.Strategy.MaxQuoteCandidates]
	}
	sem := make(chan struct{}, 2)
	results := make(chan result, len(candidates))
	var wg sync.WaitGroup
	for _, item := range candidates {
		wg.Add(1)
		go func(t TokenConfig, market FundingMarket, apr float64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cap, q, err := e.chain.DepthCapacity(ctx, t, e.cfg)
			if err != nil {
				results <- result{err: fmt.Errorf("%s: %w", t.Symbol, err)}
				return
			}
			safe := maxSafeNotional(e.cfg, t, cap)
			target := math.Min(e.cfg.Risk.TargetNotionalPerCoinUSDT, safe)
			if target <= 0 {
				return
			}
			if math.Abs(q.InputUSDT-target) > 1e-8 {
				q, err = e.chain.BestQuote(ctx, t, target, e.cfg.Risk.ReferenceQuoteUSDT)
				if err != nil {
					results <- result{err: err}
					return
				}
			}
			if market.MarkPrice <= 0 || q.BuyPrice <= 0 || math.Abs(q.BuyPrice/market.MarkPrice-1)*100 > maxTokenMatchPriceDeviationPercent {
				results <- result{err: fmt.Errorf("%s BSC spot price %.8f does not match Binance mark price %.8f", t.Symbol, q.BuyPrice, market.MarkPrice)}
				return
			}
			chainLoss := target * q.RoundTripLossBPS / 10000
			binanceFees := target * 2 * e.cfg.Binance.TakerFeeBPS / 10000
			roundTrip := chainLoss + binanceFees + e.cfg.BSC.GasReserveUSDT
			grossAnnual := target * apr / 100
			payback := math.Inf(1)
			if grossAnnual > 0 {
				payback = roundTrip / (grossAnnual / 365)
			}
			holdGross := grossAnnual * e.cfg.Strategy.EvaluationHoldHours / (365 * 24)
			expected := holdGross - roundTrip
			netAPR := apr - roundTrip/target*(365*24/e.cfg.Strategy.EvaluationHoldHours)*100
			costCoverage := 0.0
			if roundTrip > 0 {
				costCoverage = holdGross / roundTrip
			}
			if apr < e.cfg.Strategy.MinFundingAPRPercent || payback > e.cfg.Strategy.MaxEntryPaybackDays || expected <= 0 || costCoverage < e.cfg.Strategy.MinCostCoverageRatio {
				return
			}
			stats := e.state.FundingStats[t.BinanceSymbol]
			results <- result{o: Opportunity{Symbol: t.BinanceSymbol, FundingBPS: market.FundingRate * 10000, FundingAPRPercent: apr, Median7DAPRPercent: stats.Median7DAPRPercent, Median30DAPRPercent: stats.Median30DAPRPercent, PositiveFundingRate: stats.PositiveRatio, FundingSamples: stats.Samples, NetAPRPercent: netAPR, EntryCostUSDT: chainLoss/2 + target*e.cfg.Binance.TakerFeeBPS/10000 + e.cfg.BSC.GasReserveUSDT/2, RoundTripCostUSDT: roundTrip, ExpectedHoldProfit: expected, PaybackDays: payback, TargetNotionalUSDT: target, MaxSafeNotionalUSDT: safe, Chain: q, Market: market}}
		}(item.token, item.market, item.apr)
	}
	wg.Wait()
	close(results)
	var out []Opportunity
	var first error
	for r := range results {
		if r.err != nil {
			if first == nil {
				first = r.err
			}
			continue
		}
		if r.o.Symbol != "" {
			out = append(out, r.o)
		}
	}
	return out, first
}

func (e *Engine) intervalsPerYear(symbol string) float64 {
	if hours := e.fundingHours[symbol]; hours > 0 {
		return 365 * 24 / hours
	}
	return e.cfg.Strategy.FundingIntervalsPerYear
}

func (e *Engine) rebalance(ctx context.Context, now time.Time, markets map[string]FundingMarket, opps []Opportunity) {
	if e.cfg.Mode == "monitor" || (e.cfg.Mode == "live" && e.entryRiskPaused) || (e.cfg.Risk.MaxDailyLossUSDT > 0 && e.state.DailyRealizedPnL <= -e.cfg.Risk.MaxDailyLossUSDT) {
		return
	}
	bySymbol := map[string]Opportunity{}
	for _, o := range opps {
		bySymbol[o.Symbol] = o
		if _, held := e.state.Positions[o.Symbol]; !held {
			if e.state.EntryConfirmations[o.Symbol] < e.cfg.Strategy.EntryConfirmationScans {
				e.state.EntryConfirmations[o.Symbol]++
			}
		}
	}
	for symbol := range e.state.EntryConfirmations {
		if _, eligible := bySymbol[symbol]; !eligible {
			delete(e.state.EntryConfirmations, symbol)
		}
	}
	// Conservative replacement: weak settled funding, minimum age, APR advantage and full switching-cost benefit are all required.
	for symbol, p := range e.state.Positions {
		if e.state.WeakFundingSettlements[symbol] < e.cfg.Strategy.WeakFundingSettlements {
			continue
		}
		current := bySymbol[symbol]
		if current.Symbol == "" {
			stats := e.state.FundingStats[symbol]
			current = Opportunity{Symbol: symbol, FundingAPRPercent: stats.ConservativeAPRPercent, NetAPRPercent: stats.ConservativeAPRPercent}
		}
		for _, candidate := range opps {
			if _, held := e.state.Positions[candidate.Symbol]; held {
				continue
			}
			if until := e.state.CooldownUntil[candidate.Symbol]; now.Before(until) {
				continue
			}
			if e.state.EntryConfirmations[candidate.Symbol] < e.cfg.Strategy.EntryConfirmationScans {
				continue
			}
			if shouldSwitch(current, candidate, *p, e.cfg, now) {
				if err := e.closePosition(ctx, p, markets[symbol], "funding_rebalance"); err != nil {
					e.fail(ctx, "rebalance-close-"+symbol, err)
				} else if err := e.openPosition(ctx, candidate, "funding_rebalance"); err != nil {
					e.fail(ctx, "rebalance-open-"+candidate.Symbol, err)
				}
				break
			}
		}
	}
	if len(e.state.Positions) >= e.cfg.Strategy.TargetPositions {
		return
	}
	for _, o := range opps {
		if len(e.state.Positions) >= e.cfg.Strategy.TargetPositions {
			break
		}
		if _, held := e.state.Positions[o.Symbol]; held {
			continue
		}
		if until := e.state.CooldownUntil[o.Symbol]; now.Before(until) {
			continue
		}
		if e.state.EntryConfirmations[o.Symbol] < e.cfg.Strategy.EntryConfirmationScans {
			continue
		}
		if err := e.openPosition(ctx, o, "portfolio_fill"); err != nil {
			e.fail(ctx, "portfolio-open-"+o.Symbol, err)
		}
	}
}

func (e *Engine) openPosition(ctx context.Context, o Opportunity, reason string) error {
	t := e.tokens[o.Symbol]
	qty := o.Chain.OutputTokens
	shortQtyState := qty
	shortPrice := o.Market.MarkPrice
	if e.cfg.Mode == "live" {
		before, err := e.chain.Balance(ctx, t.BSCAddress, t.Decimals)
		if err != nil {
			return err
		}
		_, tx, err := e.chain.SwapExactInput(ctx, e.cfg.BSC.USDTAddress, t.BSCAddress, e.cfg.BSC.USDTDecimals, t.Decimals, o.TargetNotionalUSDT, o.Chain.OutputTokens)
		if err != nil {
			return err
		}
		after, err := e.chain.Balance(ctx, t.BSCAddress, t.Decimals)
		if err != nil {
			return fmt.Errorf("spot bought in %s but balance reconciliation failed: %w", tx, err)
		}
		qty = after - before
		if qty <= 0 {
			return fmt.Errorf("spot buy %s produced no token balance increase", tx)
		}
		if err := e.binance.ConfigureSymbol(ctx, o.Symbol); err != nil {
			_ = e.emergencySellSpot(ctx, t, qty)
			return err
		}
		rule := e.symbols[o.Symbol]
		shortQty := floorStep(qty, rule.StepSize)
		fill, err := e.binance.MarketOrder(ctx, o.Symbol, "SELL", shortQty, false)
		if err != nil {
			rollback := e.emergencySellSpot(ctx, t, qty)
			return fmt.Errorf("second leg failed: %w; spot rollback: %v", err, rollback)
		}
		shortQtyState = fill.ExecutedQty
		shortPrice = fill.AvgPrice
	}
	now := time.Now()
	p := &Position{Symbol: o.Symbol, TokenAddress: t.BSCAddress, TokenDecimals: t.Decimals, TokenQty: qty, SpotCostUSDT: o.TargetNotionalUSDT, ShortQty: shortQtyState, ShortEntryPrice: shortPrice, OpenedAt: now, LastAdjustedAt: now, LastFundingTime: now}
	e.state.Positions[o.Symbol] = p
	delete(e.state.EntryConfirmations, o.Symbol)
	event := map[string]any{"time": now, "event": "open", "mode": e.cfg.Mode, "symbol": o.Symbol, "reason": reason, "notional_usdt": o.TargetNotionalUSDT, "token_qty": qty, "short_price": shortPrice, "funding_apr_percent": o.FundingAPRPercent, "estimated_round_trip_cost_usdt": o.RoundTripCostUSDT}
	_ = appendLedger(e.cfg.StateDir, event)
	_ = e.alert.Send(ctx, "open-"+o.Symbol, "INFO", fmt.Sprintf("opened %s %.2f USDT hedge, funding APR %.1f%%", o.Symbol, o.TargetNotionalUSDT, o.FundingAPRPercent))
	return nil
}

func (e *Engine) closePosition(ctx context.Context, p *Position, market FundingMarket, reason string) error {
	t, ok := e.tokens[p.Symbol]
	if !ok || !addressPattern.MatchString(t.BSCAddress) {
		return fmt.Errorf("position %s token configuration is unavailable", p.Symbol)
	}
	exitUSDT := p.TokenQty * market.IndexPrice
	if quoted, err := e.chain.SellQuote(ctx, t, p.TokenQty); err == nil {
		exitUSDT = quoted
	}
	if e.cfg.Mode == "live" {
		rule := e.symbols[p.Symbol]
		closeQty := floorStep(p.ShortQty, rule.StepSize)
		if _, err := e.binance.MarketOrder(ctx, p.Symbol, "BUY", closeQty, true); err != nil {
			return err
		}
		quoted, err := e.chain.SellQuote(ctx, t, p.TokenQty)
		if err != nil {
			return fmt.Errorf("futures closed but spot exit quote failed: %w", err)
		}
		if _, _, err := e.chain.SwapExactInput(ctx, t.BSCAddress, e.cfg.BSC.USDTAddress, t.Decimals, e.cfg.BSC.USDTDecimals, p.TokenQty, quoted); err != nil {
			return fmt.Errorf("futures closed but spot exit failed: %w", err)
		}
		exitUSDT = quoted
	}
	shortPnL := p.ShortQty * (p.ShortEntryPrice - market.MarkPrice)
	fees := (p.ShortQty*p.ShortEntryPrice+p.ShortQty*market.MarkPrice)*e.cfg.Binance.TakerFeeBPS/10000 + e.cfg.BSC.GasReserveUSDT
	pnl := exitUSDT - p.SpotCostUSDT + shortPnL + p.FundingAccruedUSDT - fees
	e.state.DailyRealizedPnL += pnl
	delete(e.state.Positions, p.Symbol)
	delete(e.state.WeakFundingScans, p.Symbol)
	delete(e.state.WeakFundingSettlements, p.Symbol)
	delete(e.state.LiquidationBreachScans, p.Symbol)
	now := time.Now()
	delete(e.state.DepthBreachScans, p.Symbol)
	if reason == "chain_depth_limit" {
		e.state.CooldownUntil[p.Symbol] = now.Add(time.Duration(e.cfg.Risk.DepthCloseCooldownHours * float64(time.Hour)))
	}
	_ = appendLedger(e.cfg.StateDir, map[string]any{"time": now, "event": "close", "mode": e.cfg.Mode, "symbol": p.Symbol, "reason": reason, "pnl_usdt": pnl, "funding_usdt": p.FundingAccruedUSDT, "fees_and_gas_estimate_usdt": fees})
	_ = e.alert.Send(ctx, "close-"+p.Symbol, "INFO", fmt.Sprintf("closed %s, estimated PnL %.3f USDT (%s)", p.Symbol, pnl, reason))
	return nil
}

func (e *Engine) reducePosition(ctx context.Context, p *Position, fraction float64, reason string, market *FundingMarket) error {
	if fraction >= .999 {
		m := FundingMarket{Symbol: p.Symbol, MarkPrice: p.ShortEntryPrice, IndexPrice: p.ShortEntryPrice}
		if market != nil {
			m = *market
		}
		return e.closePosition(ctx, p, m, reason)
	}
	// Live partial reduction closes the liquid futures leg first, then removes the matching spot quantity.
	// Paper mode uses the same executable sell quote and cost model so its PnL is not understated.
	t, ok := e.tokens[p.Symbol]
	if !ok || !addressPattern.MatchString(t.BSCAddress) {
		return fmt.Errorf("position %s token configuration is unavailable", p.Symbol)
	}
	reduceQty := p.TokenQty * fraction
	exitUSDT := 0.0
	if e.cfg.Mode == "live" {
		rule := e.symbols[p.Symbol]
		q := floorStep(p.ShortQty*fraction, rule.StepSize)
		if _, err := e.binance.MarketOrder(ctx, p.Symbol, "BUY", q, true); err != nil {
			return err
		}
		quoted, err := e.chain.SellQuote(ctx, t, reduceQty)
		if err != nil {
			return err
		}
		if _, _, err := e.chain.SwapExactInput(ctx, t.BSCAddress, e.cfg.BSC.USDTAddress, t.Decimals, e.cfg.BSC.USDTDecimals, reduceQty, quoted); err != nil {
			return err
		}
		exitUSDT = quoted
	} else {
		quoted, err := e.chain.SellQuote(ctx, t, reduceQty)
		if err != nil {
			return err
		}
		exitUSDT = quoted
	}
	markPrice := p.ShortEntryPrice
	if market != nil && market.MarkPrice > 0 {
		markPrice = market.MarkPrice
	}
	pnl, funding, fees := partialReductionPnL(*p, fraction, exitUSDT, markPrice, e.cfg.Binance.TakerFeeBPS, e.cfg.BSC.GasReserveUSDT)
	e.state.DailyRealizedPnL += pnl
	p.RealizedPnLUSDT += pnl
	p.TokenQty *= 1 - fraction
	p.ShortQty *= 1 - fraction
	p.SpotCostUSDT *= 1 - fraction
	p.FundingAccruedUSDT *= 1 - fraction
	p.LastAdjustedAt = time.Now()
	_ = appendLedger(e.cfg.StateDir, map[string]any{"time": time.Now(), "event": "reduce", "mode": e.cfg.Mode, "symbol": p.Symbol, "fraction": fraction, "reason": reason, "pnl_usdt": pnl, "funding_usdt": funding, "fees_and_gas_estimate_usdt": fees})
	return nil
}

func (e *Engine) emergencySellSpot(ctx context.Context, t TokenConfig, qty float64) error {
	quoted, err := e.chain.SellQuote(ctx, t, qty)
	if err != nil {
		return err
	}
	_, _, err = e.chain.SwapExactInput(ctx, t.BSCAddress, e.cfg.BSC.USDTAddress, t.Decimals, e.cfg.BSC.USDTDecimals, qty, quoted)
	return err
}

func (e *Engine) fail(ctx context.Context, key string, err error) {
	slog.Error("cycle component failed", "component", key, "error", err)
	e.state.ConsecutiveFailures++
	e.mu.Lock()
	e.health.LastCycle = time.Now()
	e.health.Status = "degraded"
	e.health.LastError = err.Error()
	e.health.ConsecutiveFailures = e.state.ConsecutiveFailures
	e.mu.Unlock()
	if e.state.ConsecutiveFailures >= e.cfg.Risk.MaxConsecutiveFailures {
		_ = e.alert.Send(ctx, "failure-"+key, "ERROR", fmt.Sprintf("%s failed %d times: %v", key, e.state.ConsecutiveFailures, err))
	}
}

func (e *Engine) persist() {
	if err := saveState(e.cfg.StateDir, e.state); err != nil {
		slog.Error("save state", "error", err)
	}
}

func (e *Engine) refreshHealthLocked(markets map[string]FundingMarket) {
	e.health.Positions = len(e.state.Positions)
	e.health.Opportunities = len(e.opportunities)
	e.health.MonitoredTokens = len(e.tokens)
	e.health.AutoDiscovered = e.autoDiscovered
	e.health.AmbiguousSymbols = e.ambiguousSymbols
	e.health.PositiveFunding = len(e.fundingWatch)
	e.health.DailyRealizedPnL = e.state.DailyRealizedPnL
	e.health.ConsecutiveFailures = e.state.ConsecutiveFailures
	e.health.AccountMarginRatio = e.accountRisk.MarginRatioPercent
	e.health.AccountMarginBalance = e.accountRisk.TotalMarginBalance
	e.health.AccountAvailableBalance = e.accountRisk.AvailableBalance
	e.health.EntryPausedByMargin = e.entryRiskPaused
	e.health.RiskReduceCooldownUntil = e.state.RiskReduceCooldownUntil
	e.health.Portfolio = map[string]PositionView{}
	for symbol, p := range e.state.Positions {
		m := markets[symbol]
		spot := p.TokenQty * m.IndexPrice
		short := p.ShortQty * m.MarkPrice
		drift := 0.0
		if math.Max(spot, short) > 0 {
			drift = abs(spot-short) / math.Max(spot, short) * 100
		}
		stats := e.state.FundingStats[symbol]
		liquidationDistance := 0.0
		if risk, ok := e.positionRisks[symbol]; ok {
			liquidationDistance = liquidationDistanceX(risk)
			if math.IsInf(liquidationDistance, 0) || math.IsNaN(liquidationDistance) {
				liquidationDistance = 0
			}
		}
		e.health.Portfolio[symbol] = PositionView{Symbol: symbol, NotionalUSDT: (spot + short) / 2, FundingAccruedUSDT: p.FundingAccruedUSDT, FundingAPRPercent: stats.ConservativeAPRPercent, Median30DAPRPercent: stats.Median30DAPRPercent, PositiveFundingRatio: stats.PositiveRatio, WeakSettlements: e.state.WeakFundingSettlements[symbol], HedgeDriftPercent: drift, LiquidationDistanceX: liquidationDistance}
	}
}

func (e *Engine) Health() Health {
	e.mu.RLock()
	defer e.mu.RUnlock()
	h := e.health
	h.Portfolio = map[string]PositionView{}
	for k, v := range e.health.Portfolio {
		h.Portfolio[k] = v
	}
	return h
}
func (e *Engine) Opportunities() []Opportunity {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]Opportunity(nil), e.opportunities...)
}

func (e *Engine) FundingWatch() []FundingWatch {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]FundingWatch(nil), e.fundingWatch...)
}

func (e *Engine) Ledger(limit int) ([]map[string]any, error) {
	return readLedger(e.cfg.StateDir, limit)
}

func (e *Engine) PositionSymbols() []string {
	out := make([]string, 0, len(e.state.Positions))
	for s := range e.state.Positions {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
