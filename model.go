package main

import (
	"fmt"
	"math"
	"sort"
	"time"
)

type FundingMarket struct {
	Symbol          string    `json:"symbol"`
	MarkPrice       float64   `json:"mark_price"`
	IndexPrice      float64   `json:"index_price"`
	FundingRate     float64   `json:"funding_rate"`
	NextFundingTime time.Time `json:"next_funding_time"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type FundingRecord struct {
	Time      time.Time `json:"time"`
	Rate      float64   `json:"rate"`
	MarkPrice float64   `json:"mark_price,omitempty"`
}

type FundingStats struct {
	Samples                  int       `json:"samples"`
	Median7DAPRPercent       float64   `json:"median_7d_apr_percent"`
	Median30DAPRPercent      float64   `json:"median_30d_apr_percent"`
	LowerQuartileAPRPercent  float64   `json:"lower_quartile_apr_percent"`
	PositiveRatio            float64   `json:"positive_ratio"`
	MADAPRPercent            float64   `json:"mad_apr_percent"`
	SettlementEWMAAPRPercent float64   `json:"settlement_ewma_apr_percent"`
	ConservativeAPRPercent   float64   `json:"conservative_apr_percent"`
	LastSettlementTime       time.Time `json:"last_settlement_time"`
	ObservedNextFundingTime  time.Time `json:"observed_next_funding_time"`
}

type FundingWatch struct {
	Symbol            string    `json:"symbol"`
	BaseAsset         string    `json:"base_asset"`
	FundingBPS        float64   `json:"funding_bps_per_interval"`
	SimpleAPRPercent  float64   `json:"simple_apr_percent"`
	MarkPrice         float64   `json:"mark_price"`
	NextFundingTime   time.Time `json:"next_funding_time"`
	BSCSpotMatched    bool      `json:"bsc_spot_matched"`
	BSCAddress        string    `json:"bsc_address,omitempty"`
	BSCMatchStatus    string    `json:"bsc_match_status,omitempty"`
	EligibleForQuotes bool      `json:"eligible_for_okx_quotes"`
}

type ChainQuote struct {
	Symbol                string   `json:"symbol"`
	Route                 string   `json:"route"`
	DEXes                 []string `json:"dexes"`
	InputUSDT             float64  `json:"input_usdt"`
	OutputTokens          float64  `json:"output_tokens"`
	BuyPrice              float64  `json:"buy_price"`
	SellPrice             float64  `json:"sell_price"`
	RoundTripLossBPS      float64  `json:"round_trip_loss_bps"`
	DepthImpactBPS        float64  `json:"depth_impact_bps"`
	GasEstimate           uint64   `json:"gas_estimate"`
	OKXPriceImpactPercent float64  `json:"okx_price_impact_percent"`
}

type Opportunity struct {
	Symbol              string        `json:"symbol"`
	FundingBPS          float64       `json:"funding_bps_per_interval"`
	FundingAPRPercent   float64       `json:"funding_apr_percent"`
	Median7DAPRPercent  float64       `json:"median_7d_apr_percent"`
	Median30DAPRPercent float64       `json:"median_30d_apr_percent"`
	PositiveFundingRate float64       `json:"positive_funding_ratio"`
	FundingSamples      int           `json:"funding_history_samples"`
	NetAPRPercent       float64       `json:"net_apr_percent"`
	EntryCostUSDT       float64       `json:"entry_cost_usdt"`
	EntryBasisBPS       float64       `json:"entry_basis_bps"`
	BasisCostUSDT       float64       `json:"basis_cost_usdt"`
	RoundTripCostUSDT   float64       `json:"round_trip_cost_usdt"`
	ExpectedHoldProfit  float64       `json:"expected_hold_profit_usdt"`
	PaybackDays         float64       `json:"payback_days"`
	TargetNotionalUSDT  float64       `json:"target_notional_usdt"`
	MaxSafeNotionalUSDT float64       `json:"max_safe_notional_usdt"`
	Chain               ChainQuote    `json:"chain"`
	Market              FundingMarket `json:"market"`
}

type Position struct {
	Symbol                    string    `json:"symbol"`
	TokenAddress              string    `json:"token_address"`
	TokenDecimals             uint8     `json:"token_decimals"`
	TokenQty                  float64   `json:"token_qty"`
	SpotCostUSDT              float64   `json:"spot_cost_usdt"`
	ShortQty                  float64   `json:"short_qty"`
	ShortEntryPrice           float64   `json:"short_entry_price"`
	OpenedAt                  time.Time `json:"opened_at"`
	LastAdjustedAt            time.Time `json:"last_adjusted_at"`
	FundingAccruedUSDT        float64   `json:"funding_accrued_usdt"`
	LastFundingTime           time.Time `json:"last_funding_time"`
	LastFundingAssessmentTime time.Time `json:"last_funding_assessment_time,omitempty"`
	RealizedPnLUSDT           float64   `json:"realized_pnl_usdt"`
}

// PendingOperation is the durable journal for an exchange/chain action that may
// span more than one process lifetime. New risk is blocked while any operation
// is unfinished; recovery code can then reconcile the recorded stages against
// the real Binance position and BSC balances before retrying a leg.
type PendingOperation struct {
	ID                        string    `json:"id"`
	Kind                      string    `json:"kind"`
	Stage                     string    `json:"stage"`
	Symbol                    string    `json:"symbol"`
	Reason                    string    `json:"reason,omitempty"`
	TokenAddress              string    `json:"token_address"`
	TokenDecimals             uint8     `json:"token_decimals"`
	PlannedTokenQty           float64   `json:"planned_token_qty,omitempty"`
	PlannedFuturesQty         float64   `json:"planned_futures_qty,omitempty"`
	FuturesFilledQty          float64   `json:"futures_filled_qty,omitempty"`
	BinanceSide               string    `json:"binance_side,omitempty"`
	BinanceClientOrderID      string    `json:"binance_client_order_id,omitempty"`
	CompensationClientOrderID string    `json:"compensation_client_order_id,omitempty"`
	BinanceOrderID            int64     `json:"binance_order_id,omitempty"`
	TargetNotionalUSDT        float64   `json:"target_notional_usdt,omitempty"`
	OriginalTokenQty          float64   `json:"original_token_qty,omitempty"`
	OriginalShortQty          float64   `json:"original_short_qty,omitempty"`
	ChainTxHash               string    `json:"chain_tx_hash,omitempty"`
	CompensationChainTxHash   string    `json:"compensation_chain_tx_hash,omitempty"`
	BalanceBefore             float64   `json:"balance_before,omitempty"`
	BalanceAfter              float64   `json:"balance_after,omitempty"`
	QuoteBalanceBefore        float64   `json:"quote_balance_before,omitempty"`
	QuoteBalanceAfter         float64   `json:"quote_balance_after,omitempty"`
	QuoteBalanceRecorded      bool      `json:"quote_balance_recorded,omitempty"`
	LastError                 string    `json:"last_error,omitempty"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

type PositionRisk struct {
	Symbol           string  `json:"symbol"`
	PositionSide     string  `json:"position_side"`
	PositionAmount   float64 `json:"position_amount"`
	MarkPrice        float64 `json:"mark_price"`
	EntryPrice       float64 `json:"entry_price"`
	LiquidationPrice float64 `json:"liquidation_price"`
	UnrealizedProfit float64 `json:"unrealized_profit"`
	Leverage         int     `json:"leverage"`
	MarginType       string  `json:"margin_type"`
}

type FuturesAccountRisk struct {
	TotalWalletBalance float64 `json:"total_wallet_balance"`
	TotalUnrealizedPnL float64 `json:"total_unrealized_pnl"`
	TotalMarginBalance float64 `json:"total_margin_balance"`
	TotalMaintMargin   float64 `json:"total_maint_margin"`
	AvailableBalance   float64 `json:"available_balance"`
	MarginRatioPercent float64 `json:"margin_ratio_percent"`
}

type BotState struct {
	Version                  int                          `json:"version"`
	StartedAt                time.Time                    `json:"started_at"`
	UpdatedAt                time.Time                    `json:"updated_at"`
	Positions                map[string]*Position         `json:"positions"`
	FundingEWMA              map[string]float64           `json:"funding_ewma"`
	FundingSamples           map[string]int               `json:"funding_samples"`
	WeakFundingScans         map[string]int               `json:"weak_funding_scans"`
	FundingHistory           map[string][]FundingRecord   `json:"funding_history,omitempty"`
	FundingStats             map[string]FundingStats      `json:"funding_stats,omitempty"`
	WeakFundingSettlements   map[string]int               `json:"weak_funding_settlements,omitempty"`
	EntryConfirmations       map[string]int               `json:"entry_confirmations,omitempty"`
	CooldownUntil            map[string]time.Time         `json:"cooldown_until"`
	DepthBreachScans         map[string]int               `json:"depth_breach_scans"`
	LiquidationBreachScans   map[string]int               `json:"liquidation_breach_scans,omitempty"`
	RiskReduceCooldownUntil  time.Time                    `json:"risk_reduce_cooldown_until,omitempty"`
	AccountMarginBreachScans int                          `json:"account_margin_breach_scans,omitempty"`
	DailyRealizedPnL         float64                      `json:"daily_realized_pnl_usdt"`
	DailyDate                string                       `json:"daily_date"`
	ConsecutiveFailures      int                          `json:"consecutive_failures"`
	PendingOperations        map[string]*PendingOperation `json:"pending_operations,omitempty"`
}

type EntryEconomics struct {
	BasisCostUSDT          float64
	RoundTripCostUSDT      float64
	ExpectedHoldProfitUSDT float64
	PaybackDays            float64
	NetAPRPercent          float64
}

// immutablePositionToken permits discovery metadata to disappear or change,
// but never permits it to change the contract owned by an existing position.
func immutablePositionToken(p *Position, discovered TokenConfig) (TokenConfig, error) {
	if p == nil || !addressPattern.MatchString(p.TokenAddress) {
		return TokenConfig{}, fmt.Errorf("position token address is invalid")
	}
	discovered.Enabled = true
	discovered.BinanceSymbol = p.Symbol
	discovered.BSCAddress = p.TokenAddress
	discovered.Decimals = p.TokenDecimals
	return discovered, nil
}

// calculateEntryEconomics conservatively treats a positive on-chain premium
// over the futures mark as a convergence cost. A negative basis is not booked
// as guaranteed profit before it is realized.
func calculateEntryEconomics(target, aprPercent, holdHours, chainLoss, binanceFees, gasReserve, basisBPS float64) EntryEconomics {
	basisCost := target * math.Max(0, basisBPS) / 10000
	roundTrip := chainLoss + binanceFees + gasReserve + basisCost
	grossAnnual := target * aprPercent / 100
	payback := math.Inf(1)
	if grossAnnual > 0 {
		payback = roundTrip / (grossAnnual / 365)
	}
	holdGross := grossAnnual * holdHours / (365 * 24)
	expected := holdGross - roundTrip
	netAPR := aprPercent
	if target > 0 && holdHours > 0 {
		netAPR -= roundTrip / target * (365 * 24 / holdHours) * 100
	}
	return EntryEconomics{BasisCostUSDT: basisCost, RoundTripCostUSDT: roundTrip, ExpectedHoldProfitUSDT: expected, PaybackDays: payback, NetAPRPercent: netAPR}
}

type DepthPolicy struct {
	TriggerPercent            float64
	MinReductionPercent       float64
	EmergencyShortfallPercent float64
}

// positionDepthReductionFraction limits each reduction to a sell amount that
// was itself quoted inside the configured impact limit. A zero measured sell
// capacity is not permission to blindly liquidate the whole position.
func positionDepthReductionFraction(current, sellCapacity float64, fundingSettled bool, policy DepthPolicy) float64 {
	if current <= 0 || sellCapacity >= current || sellCapacity <= 0 {
		return 0
	}
	overagePercent := (current/sellCapacity - 1) * 100
	if overagePercent < policy.TriggerPercent {
		return 0
	}
	// Reduce toward the measured capacity, but never submit a reduction larger
	// than the largest sell chunk that passed the real token-to-USDT quote.
	reductionNotional := math.Min(current-sellCapacity, sellCapacity)
	fraction := math.Min(1, reductionNotional/current)
	shortfallPercent := (current - sellCapacity) / current * 100
	if !fundingSettled && shortfallPercent < policy.EmergencyShortfallPercent {
		return 0
	}
	if shortfallPercent < policy.MinReductionPercent {
		return 0
	}
	return fraction
}

func confirmDepthBreach(previous int, breached bool, required int) (int, bool) {
	if !breached {
		return 0, false
	}
	next := previous + 1
	return next, next >= required
}

func partialReductionPnL(p Position, fraction, exitUSDT, markPrice, takerFeeBPS, gasReserveUSDT float64) (pnl, funding, fees float64) {
	shortQty := p.ShortQty * fraction
	spotCost := p.SpotCostUSDT * fraction
	funding = p.FundingAccruedUSDT * fraction
	shortPnL := shortQty * (p.ShortEntryPrice - markPrice)
	fees = shortQty*(p.ShortEntryPrice+markPrice)*takerFeeBPS/10000 + gasReserveUSDT
	pnl = exitUSDT - spotCost + shortPnL + funding - fees
	return pnl, funding, fees
}

func annualizedFundingPercent(ratePerInterval, intervalsPerYear float64) float64 {
	return ratePerInterval * intervalsPerYear * 100
}

func mergeFundingHistory(existing, incoming []FundingRecord, cutoff time.Time) []FundingRecord {
	byTime := make(map[int64]FundingRecord, len(existing)+len(incoming))
	for _, record := range append(append([]FundingRecord(nil), existing...), incoming...) {
		if record.Time.IsZero() || record.Time.Before(cutoff) {
			continue
		}
		byTime[record.Time.UnixMilli()] = record
	}
	out := make([]FundingRecord, 0, len(byTime))
	for _, record := range byTime {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func quantileSorted(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if q <= 0 {
		return values[0]
	}
	if q >= 1 {
		return values[len(values)-1]
	}
	position := q * float64(len(values)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return values[lower]
	}
	weight := position - float64(lower)
	return values[lower]*(1-weight) + values[upper]*weight
}

func fundingAPRValues(records []FundingRecord, since time.Time, fallbackIntervalHours float64) []float64 {
	if fallbackIntervalHours <= 0 {
		fallbackIntervalHours = 8
	}
	values := make([]float64, 0, len(records))
	for i, record := range records {
		if record.Time.Before(since) {
			continue
		}
		intervalHours := fallbackIntervalHours
		if i > 0 {
			observed := record.Time.Sub(records[i-1].Time).Hours()
			if observed >= 1 && observed <= 24 {
				intervalHours = observed
			}
		} else if len(records) > 1 {
			observed := records[1].Time.Sub(record.Time).Hours()
			if observed >= 1 && observed <= 24 {
				intervalHours = observed
			}
		}
		values = append(values, record.Rate*(365*24/intervalHours)*100)
	}
	return values
}

func winsorizedDistribution(values []float64) (median, lowerQuartile, mad float64) {
	if len(values) == 0 {
		return 0, 0, 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	low, high := quantileSorted(sorted, .05), quantileSorted(sorted, .95)
	for i, value := range sorted {
		sorted[i] = math.Max(low, math.Min(high, value))
	}
	sort.Float64s(sorted)
	median = quantileSorted(sorted, .5)
	lowerQuartile = quantileSorted(sorted, .25)
	deviations := make([]float64, len(sorted))
	for i, value := range sorted {
		deviations[i] = math.Abs(value - median)
	}
	sort.Float64s(deviations)
	mad = quantileSorted(deviations, .5)
	return median, lowerQuartile, mad
}

func buildFundingStats(records []FundingRecord, currentRate, intervalHours float64, now, observedNext time.Time, ewmaAlpha float64) FundingStats {
	thirtyDaysAgo := now.Add(-30 * 24 * time.Hour)
	sevenDaysAgo := now.Add(-7 * 24 * time.Hour)
	thirty := fundingAPRValues(records, thirtyDaysAgo, intervalHours)
	seven := fundingAPRValues(records, sevenDaysAgo, intervalHours)
	median30, lowerQuartile, mad := winsorizedDistribution(thirty)
	median7, _, _ := winsorizedDistribution(seven)
	if len(seven) == 0 {
		median7 = median30
	}
	positive := 0
	for _, record := range records {
		if !record.Time.Before(thirtyDaysAgo) && record.Rate > 0 {
			positive++
		}
	}
	positiveRatio := 0.0
	if len(thirty) > 0 {
		positiveRatio = float64(positive) / float64(len(thirty))
	}
	ewma := 0.0
	for i, value := range thirty {
		if i == 0 {
			ewma = value
		} else {
			ewma = ewmaAlpha*value + (1-ewmaAlpha)*ewma
		}
	}
	currentAPR := currentRate * (365 * 24 / intervalHours) * 100
	blended := .35*median30 + .25*median7 + .25*lowerQuartile + .15*currentAPR
	confidence := .5 + .5*positiveRatio
	stability := 1 / (1 + .5*mad/math.Max(math.Abs(median30), 1))
	last := time.Time{}
	if len(records) > 0 {
		last = records[len(records)-1].Time
	}
	return FundingStats{Samples: len(thirty), Median7DAPRPercent: median7, Median30DAPRPercent: median30, LowerQuartileAPRPercent: lowerQuartile, PositiveRatio: positiveRatio, MADAPRPercent: mad, SettlementEWMAAPRPercent: ewma, ConservativeAPRPercent: blended * confidence * stability, LastSettlementTime: last, ObservedNextFundingTime: observedNext}
}

func quoteImpactBPS(referencePrice, executionPrice float64) float64 {
	if referencePrice <= 0 || executionPrice <= 0 {
		return math.Inf(1)
	}
	return math.Abs(executionPrice/referencePrice-1) * 10000
}

func liquidationDistanceX(r PositionRisk) float64 {
	if r.MarkPrice <= 0 || r.LiquidationPrice <= 0 {
		return math.Inf(1)
	}
	if r.PositionAmount < 0 {
		return r.LiquidationPrice / r.MarkPrice
	}
	return r.MarkPrice / r.LiquidationPrice
}

func reductionFraction(distance, warning, emergency, slowPercent float64) float64 {
	if distance <= emergency {
		return 1
	}
	if distance >= warning {
		return 0
	}
	return math.Min(1, slowPercent/100)
}

type PortfolioRiskCandidate struct {
	Symbol            string
	Distance          float64
	UnrealizedLoss    float64
	Notional          float64
	ConfirmedBreach   bool
	EmergencyBreach   bool
	AccountLevelEntry bool
}

// choosePortfolioRiskCandidate guarantees at most one cross-margin reduction
// per cycle. Emergency single-symbol risks come first, then confirmed symbol
// breaches, then account-level pressure ranked by loss contribution and size.
func choosePortfolioRiskCandidate(candidates []PortfolioRiskCandidate) (PortfolioRiskCandidate, bool) {
	eligible := make([]PortfolioRiskCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.EmergencyBreach || c.ConfirmedBreach || c.AccountLevelEntry {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return PortfolioRiskCandidate{}, false
	}
	tier := func(c PortfolioRiskCandidate) int {
		if c.EmergencyBreach {
			return 3
		}
		if c.ConfirmedBreach {
			return 2
		}
		return 1
	}
	contribution := func(c PortfolioRiskCandidate) float64 {
		return math.Max(0, c.UnrealizedLoss) + c.Notional*.05
	}
	sort.Slice(eligible, func(i, j int) bool {
		left, right := eligible[i], eligible[j]
		if tier(left) != tier(right) {
			return tier(left) > tier(right)
		}
		if tier(left) >= 2 && left.Distance != right.Distance {
			return left.Distance < right.Distance
		}
		if contribution(left) != contribution(right) {
			return contribution(left) > contribution(right)
		}
		return left.Symbol < right.Symbol
	})
	return eligible[0], true
}

func accountReductionFraction(ratio, reduceAt, highAt, emergencyAt, slowPercent, highPercent, emergencyPercent float64) float64 {
	switch {
	case ratio >= emergencyAt:
		return math.Min(1, emergencyPercent/100)
	case ratio >= highAt:
		return math.Min(1, highPercent/100)
	case ratio >= reduceAt:
		return math.Min(1, slowPercent/100)
	default:
		return 0
	}
}

func maxSafeNotional(c Config, t TokenConfig, depthCapacity float64) float64 {
	if depthCapacity <= 0 {
		return 0
	}
	limit := math.Min(c.Risk.MaxNotionalPerCoinUSDT, c.Risk.TotalCapitalUSDT*c.Risk.MaxCapitalPerCoinPercent/100)
	if t.MaxNotionalUSDT > 0 {
		limit = math.Min(limit, t.MaxNotionalUSDT)
	}
	limit = math.Min(limit, depthCapacity/c.Risk.DepthSafetyMultiplier)
	return math.Max(0, limit)
}

func shouldSwitch(current, replacement Opportunity, p Position, c Config, now time.Time) bool {
	if now.Sub(p.OpenedAt).Hours() < c.Strategy.MinPositionAgeHours {
		return false
	}
	if !replacementFitsReleasedCapital(p, replacement) {
		return false
	}
	if replacement.NetAPRPercent-current.NetAPRPercent < c.Strategy.MinSwitchAPRAdvantage {
		return false
	}
	oldNotional := p.ShortQty * p.ShortEntryPrice
	oldHoldGross := oldNotional * math.Max(0, current.FundingAPRPercent) / 100 * c.Strategy.EvaluationHoldHours / (365 * 24)
	oldExitCost := current.EntryCostUSDT
	if oldExitCost <= 0 {
		oldExitCost = oldNotional*c.Binance.TakerFeeBPS/10000 + c.BSC.GasReserveUSDT/2
	}
	switchBenefit := replacement.ExpectedHoldProfit - oldHoldGross - oldExitCost
	return switchBenefit >= c.Strategy.MinSwitchProfitUSDT
}

func replacementFitsReleasedCapital(p Position, replacement Opportunity) bool {
	if replacement.Symbol == "" || replacement.Symbol == p.Symbol {
		return false
	}
	// Older paper states and unit inputs may not contain notionals. When both
	// sides are known, the replacement must fit the spot capital this position
	// releases; unrelated free cash is handled by normal portfolio filling.
	if p.SpotCostUSDT > 0 && replacement.TargetNotionalUSDT > 0 && replacement.TargetNotionalUSDT > p.SpotCostUSDT*1.01 {
		return false
	}
	return true
}

// A profitable exit only releases capital when a confirmed replacement is
// expected to earn more over the same evaluation horizon. The exit PnL is
// already net of selling costs, so compare future returns from this point.
func betterDestinationForProfitExit(p Position, currentNotional, currentAPR float64, replacement Opportunity, c Config) bool {
	if replacement.TargetNotionalUSDT <= 0 || p.SpotCostUSDT <= 0 || !replacementFitsReleasedCapital(p, replacement) {
		return false
	}
	if replacement.NetAPRPercent-currentAPR < c.Strategy.MinSwitchAPRAdvantage {
		return false
	}
	currentHoldProfit := currentNotional * math.Max(0, currentAPR) / 100 * c.Strategy.EvaluationHoldHours / (365 * 24)
	return replacement.ExpectedHoldProfit-currentHoldProfit >= c.Strategy.MinSwitchProfitUSDT
}

func rankOpportunities(in []Opportunity) []Opportunity {
	out := append([]Opportunity(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].NetAPRPercent == out[j].NetAPRPercent {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].NetAPRPercent > out[j].NetAPRPercent
	})
	return out
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
