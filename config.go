package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const liveUnlockPhrase = "I_UNDERSTAND_THIS_CAN_LOSE_MONEY"

var addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

type Config struct {
	Mode         string          `json:"mode"`
	ScanSeconds  int             `json:"scan_seconds"`
	StateDir     string          `json:"state_dir"`
	HealthListen string          `json:"health_listen"`
	Binance      BinanceConfig   `json:"binance"`
	BSC          BSCConfig       `json:"bsc"`
	OKXDEX       OKXDEXConfig    `json:"okx_dex"`
	Discovery    DiscoveryConfig `json:"auto_discovery"`
	Dashboard    DashboardConfig `json:"dashboard"`
	Strategy     StrategyConfig  `json:"strategy"`
	Risk         RiskConfig      `json:"risk"`
	Alerts       AlertConfig     `json:"alerts"`
	Tokens       []TokenConfig   `json:"tokens"`
	LiveUnlock   string          `json:"live_unlock"`
}

type BinanceConfig struct {
	BaseURL               string  `json:"base_url"`
	APIKeyEnv             string  `json:"api_key_env"`
	SecretKeyEnv          string  `json:"secret_key_env"`
	TakerFeeBPS           float64 `json:"taker_fee_bps"`
	Leverage              int     `json:"leverage"`
	MarginType            string  `json:"margin_type"`
	RecvWindowMS          int64   `json:"recv_window_ms"`
	RequestTimeoutSeconds int     `json:"request_timeout_seconds"`
}

type BSCConfig struct {
	RPCURL          string  `json:"rpc_url"`
	ChainID         int64   `json:"chain_id"`
	WalletAddress   string  `json:"wallet_address"`
	PrivateKeyEnv   string  `json:"private_key_env"`
	USDTAddress     string  `json:"usdt_address"`
	USDTDecimals    uint8   `json:"usdt_decimals"`
	SlippageBPS     float64 `json:"slippage_bps"`
	GasReserveUSDT  float64 `json:"gas_reserve_usdt_per_round_trip"`
	MaxGasLimit     uint64  `json:"max_gas_limit"`
	MaxGasPriceGwei float64 `json:"max_gas_price_gwei"`
	Confirmations   uint64  `json:"confirmations"`
}

type OKXDEXConfig struct {
	BaseURL               string `json:"base_url"`
	APIKeyEnv             string `json:"api_key_env"`
	SecretKeyEnv          string `json:"secret_key_env"`
	PassphraseEnv         string `json:"passphrase_env"`
	ProjectIDEnv          string `json:"project_id_env"`
	RequestTimeoutSeconds int    `json:"request_timeout_seconds"`
	DisableRFQ            bool   `json:"disable_rfq"`
}

type DiscoveryConfig struct {
	Enabled        bool     `json:"enabled"`
	RefreshMinutes int      `json:"refresh_minutes"`
	DenySymbols    []string `json:"deny_symbols"`
}

type DashboardConfig struct {
	RefreshSeconds      int    `json:"refresh_seconds"`
	AllowLAN            bool   `json:"allow_lan"`
	Username            string `json:"username"`
	PasswordEnv         string `json:"password_env"`
	ReadOnlyUsername    string `json:"read_only_username,omitempty"`
	ReadOnlyPasswordEnv string `json:"read_only_password_env,omitempty"`
}

type StrategyConfig struct {
	TargetPositions                    int     `json:"target_positions"`
	MaxQuoteCandidates                 int     `json:"max_quote_candidates"`
	ChainQuoteRefreshMinutes           int     `json:"chain_quote_refresh_minutes"`
	MinCurrentFundingBPS               float64 `json:"min_current_funding_bps_per_interval"`
	MinFundingAPRPercent               float64 `json:"min_funding_apr_percent"`
	FundingIntervalsPerYear            float64 `json:"funding_intervals_per_year"`
	FundingEWMAAlpha                   float64 `json:"funding_ewma_alpha"`
	FundingHistoryDays                 int     `json:"funding_history_days"`
	MinFundingHistorySamples           int     `json:"min_funding_history_samples"`
	MinPositiveFundingRatio            float64 `json:"min_positive_funding_ratio"`
	MaxCurrentToMedianRatio            float64 `json:"max_current_to_median_ratio"`
	EntryConfirmationScans             int     `json:"entry_confirmation_scans"`
	DepthHistoryProbeMinBPS            float64 `json:"depth_history_probe_min_funding_bps"`
	DepthHistoryWindowHours            int     `json:"depth_history_window_hours"`
	DepthHistoryMaxSamples             int     `json:"depth_history_max_samples"`
	DepthHistoryHalfSamples            int     `json:"depth_history_half_samples"`
	DepthHistoryFullSamples            int     `json:"depth_history_full_samples"`
	DepthHistoryMinSpanMinutes         int     `json:"depth_history_min_span_minutes"`
	DepthHistoryMinPassRatio           float64 `json:"depth_history_min_pass_ratio"`
	DepthHistoryColdStartConfirmations int     `json:"depth_history_cold_start_confirmations"`
	WeakFundingSettlements             int     `json:"weak_funding_settlements"`
	MinCostCoverageRatio               float64 `json:"min_cost_coverage_ratio"`
	EvaluationHoldHours                float64 `json:"evaluation_hold_hours"`
	MaxEntryPaybackDays                float64 `json:"max_entry_payback_days"`
	MaxEntryBasisBPS                   float64 `json:"max_entry_basis_bps"`
	MinSwitchAPRAdvantage              float64 `json:"min_switch_apr_advantage_percent"`
	MinSwitchProfitUSDT                float64 `json:"min_switch_profit_usdt"`
	MinPositionAgeHours                float64 `json:"min_position_age_hours_before_switch"`
	PriceSpreadTakeProfitBPS           float64 `json:"price_spread_take_profit_bps"`
	MinPriceArbNetUSDT                 float64 `json:"min_price_arb_net_usdt"`
	PriceArbCooldownHours              float64 `json:"price_arb_cooldown_hours"`
}

type RiskConfig struct {
	TotalCapitalUSDT               float64 `json:"total_capital_usdt"`
	TargetNotionalPerCoinUSDT      float64 `json:"target_notional_per_coin_usdt"`
	MaxNotionalPerCoinUSDT         float64 `json:"max_notional_per_coin_usdt"`
	MaxCapitalPerCoinPercent       float64 `json:"max_capital_per_coin_percent"`
	DepthSafetyMultiplier          float64 `json:"depth_safety_multiplier"`
	DepthReductionTriggerPercent   float64 `json:"depth_reduction_trigger_percent"`
	MinDepthReductionPercent       float64 `json:"min_depth_reduction_percent"`
	DepthEmergencyShortfallPct     float64 `json:"depth_emergency_shortfall_percent"`
	DepthBreachConfirmations       int     `json:"depth_breach_confirmations"`
	DepthCloseCooldownHours        float64 `json:"depth_close_cooldown_hours"`
	MaxEntryChainPriceImpactBPS    float64 `json:"max_entry_chain_price_impact_bps"`
	MaxChainPriceImpactBPS         float64 `json:"max_chain_price_impact_bps"`
	ReferenceQuoteUSDT             float64 `json:"reference_quote_usdt"`
	MinLiquidationDistanceX        float64 `json:"min_liquidation_distance_x"`
	EmergencyLiquidationDistanceX  float64 `json:"emergency_liquidation_distance_x"`
	SlowReducePercent              float64 `json:"slow_reduce_percent"`
	LiquidationBreachConfirmations int     `json:"liquidation_breach_confirmations"`
	RiskReduceCooldownMinutes      int     `json:"risk_reduce_cooldown_minutes"`
	AccountMarginStopEntryPercent  float64 `json:"account_margin_stop_entry_percent"`
	AccountMarginReducePercent     float64 `json:"account_margin_reduce_percent"`
	AccountMarginHighPercent       float64 `json:"account_margin_high_percent"`
	AccountMarginEmergencyPercent  float64 `json:"account_margin_emergency_percent"`
	AccountHighReducePercent       float64 `json:"account_high_reduce_percent"`
	AccountEmergencyReducePercent  float64 `json:"account_emergency_reduce_percent"`
	MaxHedgeDriftPercent           float64 `json:"max_hedge_drift_percent"`
	MaxDataAgeSeconds              int     `json:"max_data_age_seconds"`
	MaxConsecutiveFailures         int     `json:"max_consecutive_failures"`
	MaxDailyLossUSDT               float64 `json:"max_daily_loss_usdt"`
	MaxFuturesMarginUsePercent     float64 `json:"max_futures_margin_use_percent"`
	MinBSCUSDTReserve              float64 `json:"min_bsc_usdt_reserve"`
}

type AlertConfig struct {
	WebhookURLEnv     string `json:"webhook_url_env"`
	TelegramTokenEnv  string `json:"telegram_token_env"`
	TelegramChatIDEnv string `json:"telegram_chat_id_env"`
	CooldownSeconds   int    `json:"cooldown_seconds"`
}

type TokenConfig struct {
	Enabled          bool    `json:"enabled"`
	Symbol           string  `json:"symbol"`
	BinanceSymbol    string  `json:"binance_symbol"`
	BSCAddress       string  `json:"bsc_address"`
	Decimals         uint8   `json:"decimals"`
	MaxNotionalUSDT  float64 `json:"max_notional_usdt"`
	VerifiedContract bool    `json:"verified_contract"`
	FeeOnTransfer    bool    `json:"fee_on_transfer"`
}

func loadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	applyConfigDefaults(&c)
	return c, c.Validate()
}

func applyConfigDefaults(c *Config) {
	if c.Strategy.FundingHistoryDays == 0 {
		c.Strategy.FundingHistoryDays = 30
	}
	if c.Strategy.MinFundingHistorySamples == 0 {
		c.Strategy.MinFundingHistorySamples = 12
	}
	if c.Strategy.MinPositiveFundingRatio == 0 {
		c.Strategy.MinPositiveFundingRatio = 0.70
	}
	if c.Strategy.MaxCurrentToMedianRatio == 0 {
		c.Strategy.MaxCurrentToMedianRatio = 3
	}
	if c.Strategy.EntryConfirmationScans == 0 {
		c.Strategy.EntryConfirmationScans = 2
	}
	if c.Strategy.DepthHistoryProbeMinBPS == 0 {
		c.Strategy.DepthHistoryProbeMinBPS = math.Min(1, c.Strategy.MinCurrentFundingBPS)
	}
	if c.Strategy.DepthHistoryWindowHours == 0 {
		c.Strategy.DepthHistoryWindowHours = 24
	}
	if c.Strategy.DepthHistoryMaxSamples == 0 {
		c.Strategy.DepthHistoryMaxSamples = 48
	}
	if c.Strategy.DepthHistoryHalfSamples == 0 {
		c.Strategy.DepthHistoryHalfSamples = 12
	}
	if c.Strategy.DepthHistoryFullSamples == 0 {
		c.Strategy.DepthHistoryFullSamples = 24
	}
	if c.Strategy.DepthHistoryMinSpanMinutes == 0 {
		c.Strategy.DepthHistoryMinSpanMinutes = 180
	}
	if c.Strategy.DepthHistoryMinPassRatio == 0 {
		c.Strategy.DepthHistoryMinPassRatio = .85
	}
	if c.Strategy.DepthHistoryColdStartConfirmations == 0 {
		c.Strategy.DepthHistoryColdStartConfirmations = 3
	}
	if c.Strategy.WeakFundingSettlements == 0 {
		c.Strategy.WeakFundingSettlements = 3
	}
	if c.Strategy.MinCostCoverageRatio == 0 {
		c.Strategy.MinCostCoverageRatio = 2.5
	}
	if c.Strategy.MaxEntryBasisBPS == 0 {
		c.Strategy.MaxEntryBasisBPS = 100
	}
	if c.Risk.DepthReductionTriggerPercent == 0 {
		c.Risk.DepthReductionTriggerPercent = 10
	}
	if c.Risk.MinDepthReductionPercent == 0 {
		c.Risk.MinDepthReductionPercent = 5
	}
	if c.Risk.DepthEmergencyShortfallPct == 0 {
		c.Risk.DepthEmergencyShortfallPct = 50
	}
	if c.Risk.DepthBreachConfirmations == 0 {
		c.Risk.DepthBreachConfirmations = 3
	}
	if c.Risk.DepthCloseCooldownHours == 0 {
		c.Risk.DepthCloseCooldownHours = 24
	}
	if c.Risk.MaxEntryChainPriceImpactBPS == 0 {
		c.Risk.MaxEntryChainPriceImpactBPS = math.Min(20, c.Risk.MaxChainPriceImpactBPS)
	}
	if c.Risk.LiquidationBreachConfirmations == 0 {
		c.Risk.LiquidationBreachConfirmations = 3
	}
	if c.Risk.RiskReduceCooldownMinutes == 0 {
		c.Risk.RiskReduceCooldownMinutes = 15
	}
	if c.Risk.AccountMarginStopEntryPercent == 0 {
		c.Risk.AccountMarginStopEntryPercent = 55
	}
	if c.Risk.AccountMarginReducePercent == 0 {
		c.Risk.AccountMarginReducePercent = 65
	}
	if c.Risk.AccountMarginHighPercent == 0 {
		c.Risk.AccountMarginHighPercent = 75
	}
	if c.Risk.AccountMarginEmergencyPercent == 0 {
		c.Risk.AccountMarginEmergencyPercent = 80
	}
	if c.Risk.AccountHighReducePercent == 0 {
		c.Risk.AccountHighReducePercent = 25
	}
	if c.Risk.AccountEmergencyReducePercent == 0 {
		c.Risk.AccountEmergencyReducePercent = 50
	}
	if c.Risk.MaxFuturesMarginUsePercent == 0 {
		c.Risk.MaxFuturesMarginUsePercent = 50
	}
	if c.Risk.MinBSCUSDTReserve == 0 {
		c.Risk.MinBSCUSDTReserve = 20
	}
}

func (c Config) Validate() error {
	if c.Mode != "monitor" && c.Mode != "paper" && c.Mode != "live" {
		return errors.New("mode must be monitor, paper or live")
	}
	if c.ScanSeconds < 5 || c.StateDir == "" {
		return errors.New("scan_seconds must be >= 5 and state_dir is required")
	}
	if err := validateHTTPS(c.Binance.BaseURL, "binance.base_url"); err != nil {
		return err
	}
	if err := validateHTTPS(c.BSC.RPCURL, "bsc.rpc_url"); err != nil {
		return err
	}
	if err := validateHTTPS(c.OKXDEX.BaseURL, "okx_dex.base_url"); err != nil {
		return err
	}
	for n, a := range map[string]string{"wallet": c.BSC.WalletAddress, "usdt": c.BSC.USDTAddress} {
		if !addressPattern.MatchString(a) {
			return fmt.Errorf("invalid BSC %s address", n)
		}
	}
	if c.BSC.ChainID != 56 {
		return errors.New("this build only supports BSC mainnet chain_id 56")
	}
	if c.BSC.USDTDecimals > 36 || c.BSC.Confirmations < 1 || c.BSC.MaxGasLimit < 21000 || c.BSC.MaxGasPriceGwei <= 0 {
		return errors.New("invalid BSC decimals, confirmations or gas safety limits")
	}
	if c.Binance.Leverage < 1 || c.Binance.Leverage > 20 || c.Binance.RecvWindowMS < 1000 || c.Binance.RecvWindowMS > 60000 || c.Binance.RequestTimeoutSeconds < 2 {
		return errors.New("invalid Binance settings")
	}
	if c.Binance.MarginType != "CROSSED" && c.Binance.MarginType != "ISOLATED" {
		return errors.New("binance.margin_type must be CROSSED or ISOLATED")
	}
	if c.Binance.APIKeyEnv == "" || c.Binance.SecretKeyEnv == "" || c.BSC.PrivateKeyEnv == "" {
		return errors.New("secret environment variable names are required")
	}
	if c.OKXDEX.APIKeyEnv == "" || c.OKXDEX.SecretKeyEnv == "" || c.OKXDEX.PassphraseEnv == "" || c.OKXDEX.ProjectIDEnv == "" || c.OKXDEX.RequestTimeoutSeconds < 2 {
		return errors.New("OKX DEX credential environment names and timeout are required")
	}
	if c.Discovery.Enabled && c.Discovery.RefreshMinutes < 5 {
		return errors.New("auto_discovery.refresh_minutes must be at least 5")
	}
	if c.Discovery.Enabled && c.Mode == "live" {
		return errors.New("auto_discovery is monitor/paper only; live mode requires an explicit reviewed token allowlist")
	}
	if c.Dashboard.RefreshSeconds < 2 || c.Dashboard.RefreshSeconds > 300 {
		return errors.New("dashboard.refresh_seconds must be between 2 and 300")
	}
	if (c.Dashboard.ReadOnlyUsername == "") != (c.Dashboard.ReadOnlyPasswordEnv == "") {
		return errors.New("dashboard read-only username and password_env must be configured together")
	}
	if c.Dashboard.ReadOnlyUsername != "" && c.Dashboard.ReadOnlyUsername == c.Dashboard.Username {
		return errors.New("dashboard admin and read-only usernames must differ")
	}
	for i, symbol := range c.Discovery.DenySymbols {
		if symbol == "" || symbol != strings.ToUpper(symbol) {
			return fmt.Errorf("auto_discovery.deny_symbols[%d] must be uppercase", i)
		}
	}
	nums := []float64{c.Binance.TakerFeeBPS, c.BSC.SlippageBPS, c.BSC.GasReserveUSDT, c.BSC.MaxGasPriceGwei, c.Strategy.MinCurrentFundingBPS, c.Strategy.MinFundingAPRPercent, c.Strategy.FundingIntervalsPerYear, c.Strategy.FundingEWMAAlpha, c.Strategy.MinPositiveFundingRatio, c.Strategy.MaxCurrentToMedianRatio, c.Strategy.DepthHistoryProbeMinBPS, c.Strategy.DepthHistoryMinPassRatio, c.Strategy.MinCostCoverageRatio, c.Strategy.EvaluationHoldHours, c.Strategy.MaxEntryPaybackDays, c.Strategy.MaxEntryBasisBPS, c.Strategy.MinSwitchAPRAdvantage, c.Strategy.MinSwitchProfitUSDT, c.Strategy.MinPositionAgeHours, c.Strategy.PriceSpreadTakeProfitBPS, c.Strategy.MinPriceArbNetUSDT, c.Strategy.PriceArbCooldownHours, c.Risk.TotalCapitalUSDT, c.Risk.TargetNotionalPerCoinUSDT, c.Risk.MaxNotionalPerCoinUSDT, c.Risk.MaxCapitalPerCoinPercent, c.Risk.DepthSafetyMultiplier, c.Risk.DepthReductionTriggerPercent, c.Risk.MinDepthReductionPercent, c.Risk.DepthEmergencyShortfallPct, c.Risk.DepthCloseCooldownHours, c.Risk.MaxEntryChainPriceImpactBPS, c.Risk.MaxChainPriceImpactBPS, c.Risk.ReferenceQuoteUSDT, c.Risk.MinLiquidationDistanceX, c.Risk.EmergencyLiquidationDistanceX, c.Risk.SlowReducePercent, c.Risk.AccountMarginStopEntryPercent, c.Risk.AccountMarginReducePercent, c.Risk.AccountMarginHighPercent, c.Risk.AccountMarginEmergencyPercent, c.Risk.AccountHighReducePercent, c.Risk.AccountEmergencyReducePercent, c.Risk.MaxHedgeDriftPercent, c.Risk.MaxDailyLossUSDT, c.Risk.MaxFuturesMarginUsePercent, c.Risk.MinBSCUSDTReserve}
	for _, v := range nums {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("numeric settings must be finite and nonnegative")
		}
	}
	if c.Strategy.TargetPositions < 1 || c.Strategy.TargetPositions > 50 || c.Strategy.MaxQuoteCandidates < c.Strategy.TargetPositions || c.Strategy.MaxQuoteCandidates > 100 || c.Strategy.ChainQuoteRefreshMinutes < 5 || c.Strategy.ChainQuoteRefreshMinutes > 1440 || c.Strategy.FundingIntervalsPerYear <= 0 || c.Strategy.FundingEWMAAlpha <= 0 || c.Strategy.FundingEWMAAlpha > 1 {
		return errors.New("invalid strategy portfolio/funding settings")
	}
	if c.Strategy.FundingHistoryDays < 30 || c.Strategy.FundingHistoryDays > 90 || c.Strategy.MinFundingHistorySamples < 3 || c.Strategy.MinFundingHistorySamples > 1000 || c.Strategy.MinPositiveFundingRatio <= 0 || c.Strategy.MinPositiveFundingRatio > 1 || c.Strategy.MaxCurrentToMedianRatio < 1 || c.Strategy.EntryConfirmationScans < 1 || c.Strategy.EntryConfirmationScans > 10 || c.Strategy.WeakFundingSettlements < 1 || c.Strategy.WeakFundingSettlements > 10 || c.Strategy.MinCostCoverageRatio < 1 {
		return errors.New("invalid historical funding, entry confirmation or cost coverage settings")
	}
	if c.Strategy.DepthHistoryProbeMinBPS > c.Strategy.MinCurrentFundingBPS || c.Strategy.DepthHistoryWindowHours < 1 || c.Strategy.DepthHistoryWindowHours > 168 || c.Strategy.DepthHistoryMaxSamples < 3 || c.Strategy.DepthHistoryMaxSamples > 500 || c.Strategy.DepthHistoryHalfSamples < 3 || c.Strategy.DepthHistoryFullSamples < c.Strategy.DepthHistoryHalfSamples || c.Strategy.DepthHistoryFullSamples > c.Strategy.DepthHistoryMaxSamples || c.Strategy.DepthHistoryMinSpanMinutes < 10 || c.Strategy.DepthHistoryMinSpanMinutes > c.Strategy.DepthHistoryWindowHours*60 || c.Strategy.DepthHistoryMinPassRatio <= 0 || c.Strategy.DepthHistoryMinPassRatio > 1 || c.Strategy.DepthHistoryColdStartConfirmations < 2 || c.Strategy.DepthHistoryColdStartConfirmations > c.Strategy.DepthHistoryHalfSamples {
		return errors.New("invalid historical two-sided depth settings")
	}
	if c.Strategy.MaxEntryBasisBPS <= 0 || c.Strategy.MaxEntryBasisBPS > 1000 {
		return errors.New("max_entry_basis_bps must be between 0 and 1000")
	}
	if c.Risk.TotalCapitalUSDT <= 0 || c.Risk.TargetNotionalPerCoinUSDT <= 0 || c.Risk.MaxNotionalPerCoinUSDT < c.Risk.TargetNotionalPerCoinUSDT || c.Risk.MaxCapitalPerCoinPercent <= 0 || c.Risk.MaxCapitalPerCoinPercent > 100 {
		return errors.New("invalid capital limits")
	}
	if float64(c.Strategy.TargetPositions)*c.Risk.TargetNotionalPerCoinUSDT > c.Risk.TotalCapitalUSDT {
		return errors.New("target portfolio notional exceeds total_capital_usdt")
	}
	if c.Risk.DepthSafetyMultiplier < 1 || c.Risk.SlowReducePercent <= 0 || c.Risk.SlowReducePercent > 100 || c.Risk.EmergencyLiquidationDistanceX >= c.Risk.MinLiquidationDistanceX {
		return errors.New("invalid depth or liquidation reduction limits")
	}
	if c.Risk.LiquidationBreachConfirmations < 1 || c.Risk.LiquidationBreachConfirmations > 10 || c.Risk.RiskReduceCooldownMinutes < 1 || c.Risk.RiskReduceCooldownMinutes > 1440 {
		return errors.New("invalid liquidation confirmation or cooldown settings")
	}
	if !(c.Risk.AccountMarginStopEntryPercent < c.Risk.AccountMarginReducePercent && c.Risk.AccountMarginReducePercent < c.Risk.AccountMarginHighPercent && c.Risk.AccountMarginHighPercent < c.Risk.AccountMarginEmergencyPercent && c.Risk.AccountMarginEmergencyPercent < 100) || c.Risk.AccountHighReducePercent < c.Risk.SlowReducePercent || c.Risk.AccountHighReducePercent > 100 || c.Risk.AccountEmergencyReducePercent < c.Risk.AccountHighReducePercent || c.Risk.AccountEmergencyReducePercent > 100 {
		return errors.New("invalid cross-margin portfolio risk thresholds")
	}
	if c.Risk.DepthReductionTriggerPercent <= 0 || c.Risk.DepthReductionTriggerPercent > 100 || c.Risk.MinDepthReductionPercent <= 0 || c.Risk.MinDepthReductionPercent > 100 || c.Risk.DepthEmergencyShortfallPct <= c.Risk.DepthReductionTriggerPercent || c.Risk.DepthEmergencyShortfallPct > 100 || c.Risk.DepthBreachConfirmations < 1 || c.Risk.DepthBreachConfirmations > 10 || c.Risk.DepthCloseCooldownHours <= 0 {
		return errors.New("invalid depth hysteresis, confirmation or cooldown settings")
	}
	if c.Risk.MaxHedgeDriftPercent <= 0 || c.BSC.SlippageBPS > 500 || c.Risk.MaxChainPriceImpactBPS > 1000 || (c.Strategy.PriceSpreadTakeProfitBPS > 0 && c.Strategy.PriceArbCooldownHours <= 0) {
		return errors.New("invalid slippage, impact, hedge drift or spread cooldown")
	}
	if c.Risk.MaxEntryChainPriceImpactBPS <= 0 || c.Risk.MaxEntryChainPriceImpactBPS > c.Risk.MaxChainPriceImpactBPS {
		return errors.New("max_entry_chain_price_impact_bps must be positive and no greater than max_chain_price_impact_bps")
	}
	if c.Risk.MaxFuturesMarginUsePercent <= 0 || c.Risk.MaxFuturesMarginUsePercent > 100 {
		return errors.New("max_futures_margin_use_percent must be between 0 and 100")
	}
	if c.Risk.MaxDataAgeSeconds < c.ScanSeconds || c.Risk.MaxConsecutiveFailures < 1 {
		return errors.New("invalid stale/failure limits")
	}
	if c.HealthListen != "" {
		host, _, err := net.SplitHostPort(c.HealthListen)
		if err != nil {
			return fmt.Errorf("health_listen: %w", err)
		}
		loopback := host == "localhost"
		if !loopback {
			ip := net.ParseIP(host)
			loopback = ip != nil && ip.IsLoopback()
		}
		if !loopback && !c.Dashboard.AllowLAN {
			return errors.New("non-loopback health_listen requires dashboard.allow_lan=true")
		}
		if !loopback && (c.Dashboard.Username == "" || c.Dashboard.PasswordEnv == "") {
			return errors.New("LAN dashboard requires username and password_env")
		}
	}
	seenSymbol, seenAddress := map[string]bool{}, map[string]bool{}
	enabled := 0
	for i, t := range c.Tokens {
		if !t.Enabled {
			continue
		}
		enabled++
		if t.Symbol == "" || t.Symbol != strings.ToUpper(t.Symbol) || t.BinanceSymbol == "" || t.BinanceSymbol != strings.ToUpper(t.BinanceSymbol) {
			return fmt.Errorf("tokens[%d]: symbols must be uppercase", i)
		}
		addr := strings.ToLower(t.BSCAddress)
		if !addressPattern.MatchString(t.BSCAddress) || seenSymbol[t.BinanceSymbol] || seenAddress[addr] {
			return fmt.Errorf("tokens[%d]: invalid or duplicate symbol/address", i)
		}
		seenSymbol[t.BinanceSymbol], seenAddress[addr] = true, true
		if !t.VerifiedContract || t.FeeOnTransfer {
			return fmt.Errorf("tokens[%d]: live-safe tokens must be verified and not fee-on-transfer", i)
		}
		if t.Decimals > 36 {
			return fmt.Errorf("tokens[%d]: valid decimals required", i)
		}
	}
	if enabled == 0 && !c.Discovery.Enabled {
		return errors.New("at least one enabled, verified token is required")
	}
	if c.Mode == "live" && c.LiveUnlock != liveUnlockPhrase {
		return errors.New("live mode requires the exact live_unlock phrase")
	}
	if c.Mode == "live" && (c.Dashboard.Username == "" || c.Dashboard.PasswordEnv == "") {
		return errors.New("live mode requires dashboard administrator authentication")
	}
	if c.Mode == "live" && strings.EqualFold(c.BSC.WalletAddress, "0x0000000000000000000000000000000000000000") {
		return errors.New("live mode requires a nonzero wallet address")
	}
	return nil
}

func validateHTTPS(raw, name string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%s must be an HTTPS URL", name)
	}
	return nil
}
