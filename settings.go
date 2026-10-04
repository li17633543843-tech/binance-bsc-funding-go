package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

const tuningFileVersion = 1

var errInvalidTuning = errors.New("invalid tuning settings")

// TuningSettings contains only non-secret operator controls. Credentials,
// wallet addresses, URLs, token allowlists, mode and live unlock are never
// returned by, or accepted from, the dashboard API.
type TuningSettings struct {
	Version                 int            `json:"version"`
	ScanSeconds             int            `json:"scan_seconds"`
	Leverage                int            `json:"leverage"`
	TakerFeeBPS             float64        `json:"taker_fee_bps"`
	SlippageBPS             float64        `json:"slippage_bps"`
	GasReserveUSDT          float64        `json:"gas_reserve_usdt_per_round_trip"`
	MaxGasPriceGwei         float64        `json:"max_gas_price_gwei"`
	DiscoveryRefreshMinutes int            `json:"discovery_refresh_minutes"`
	DashboardRefreshSeconds int            `json:"dashboard_refresh_seconds"`
	Strategy                StrategyConfig `json:"strategy"`
	Risk                    RiskConfig     `json:"risk"`
}

type SettingsResponse struct {
	Mode                  string         `json:"mode"`
	Settings              TuningSettings `json:"settings"`
	RuntimeSections       []string       `json:"runtime_sections"`
	RestartSections       []string       `json:"restart_sections"`
	AppliedAfterNextCycle bool           `json:"applied_after_next_cycle,omitempty"`
	RestartRequired       bool           `json:"restart_required,omitempty"`
	ChangedSections       []string       `json:"changed_sections,omitempty"`
	SavedAt               *time.Time     `json:"saved_at,omitempty"`
}

func tuningFromConfig(c Config) TuningSettings {
	return TuningSettings{
		Version:                 tuningFileVersion,
		ScanSeconds:             c.ScanSeconds,
		Leverage:                c.Binance.Leverage,
		TakerFeeBPS:             c.Binance.TakerFeeBPS,
		SlippageBPS:             c.BSC.SlippageBPS,
		GasReserveUSDT:          c.BSC.GasReserveUSDT,
		MaxGasPriceGwei:         c.BSC.MaxGasPriceGwei,
		DiscoveryRefreshMinutes: c.Discovery.RefreshMinutes,
		DashboardRefreshSeconds: c.Dashboard.RefreshSeconds,
		Strategy:                c.Strategy,
		Risk:                    c.Risk,
	}
}

func applyTuning(c *Config, t TuningSettings) {
	c.ScanSeconds = t.ScanSeconds
	c.Binance.Leverage = t.Leverage
	c.Binance.TakerFeeBPS = t.TakerFeeBPS
	c.BSC.SlippageBPS = t.SlippageBPS
	c.BSC.GasReserveUSDT = t.GasReserveUSDT
	c.BSC.MaxGasPriceGwei = t.MaxGasPriceGwei
	c.Discovery.RefreshMinutes = t.DiscoveryRefreshMinutes
	c.Dashboard.RefreshSeconds = t.DashboardRefreshSeconds
	c.Strategy = t.Strategy
	c.Risk = t.Risk
	// Older ui-tuning.json files predate the basis guard. Preserve all saved
	// controls while inheriting the safe default instead of failing startup.
	if c.Strategy.MaxEntryBasisBPS == 0 {
		c.Strategy.MaxEntryBasisBPS = 100
	}
	if c.Risk.MaxFuturesMarginUsePercent == 0 {
		c.Risk.MaxFuturesMarginUsePercent = 50
	}
	if c.Risk.MinBSCUSDTReserve == 0 {
		c.Risk.MinBSCUSDTReserve = 20
	}
	if c.Risk.MaxEntryChainPriceImpactBPS == 0 {
		c.Risk.MaxEntryChainPriceImpactBPS = math.Min(20, c.Risk.MaxChainPriceImpactBPS)
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
}

func tuningPath(stateDir string) string { return filepath.Join(stateDir, "ui-tuning.json") }

func loadTuning(stateDir string) (TuningSettings, bool, error) {
	var t TuningSettings
	f, err := os.Open(tuningPath(stateDir))
	if os.IsNotExist(err) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 128*1024))
	d.DisallowUnknownFields()
	if err := d.Decode(&t); err != nil {
		return t, false, err
	}
	if err := ensureJSONEOF(d); err != nil {
		return t, false, err
	}
	if t.Version != tuningFileVersion {
		return t, false, fmt.Errorf("unsupported UI tuning version %d", t.Version)
	}
	return t, true, nil
}

func loadEffectiveConfig(path string) (Config, error) {
	c, err := loadConfig(path)
	if err != nil {
		return c, err
	}
	t, ok, err := loadTuning(c.StateDir)
	if err != nil {
		return c, fmt.Errorf("load dashboard tuning: %w", err)
	}
	if !ok {
		return c, nil
	}
	applyTuning(&c, t)
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("dashboard tuning is invalid: %w", err)
	}
	return c, nil
}

func saveTuning(stateDir string, t TuningSettings) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	t.Version = tuningFileVersion
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := tuningPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, tuningPath(stateDir))
}

func ensureJSONEOF(d *json.Decoder) error {
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request must contain exactly one JSON object")
		}
		return err
	}
	return nil
}

func changedTuningSections(before, after TuningSettings) (all []string, restart []string) {
	checkRestart := func(changed bool, name string) {
		if changed {
			all = append(all, name)
			restart = append(restart, name)
		}
	}
	checkRestart(before.ScanSeconds != after.ScanSeconds, "扫描间隔")
	checkRestart(before.Leverage != after.Leverage, "合约杠杆")
	checkRestart(before.TakerFeeBPS != after.TakerFeeBPS || before.SlippageBPS != after.SlippageBPS || before.GasReserveUSDT != after.GasReserveUSDT || before.MaxGasPriceGwei != after.MaxGasPriceGwei, "成本参数")
	checkRestart(before.DiscoveryRefreshMinutes != after.DiscoveryRefreshMinutes, "代币发现刷新")
	checkRestart(before.DashboardRefreshSeconds != after.DashboardRefreshSeconds, "页面刷新")
	if !reflect.DeepEqual(before.Strategy, after.Strategy) {
		all = append(all, "策略参数")
	}
	if !reflect.DeepEqual(before.Risk, after.Risk) {
		all = append(all, "风控参数")
	}
	return all, restart
}

func (e *Engine) settingsResponse() SettingsResponse {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	return SettingsResponse{
		Mode:            e.settingsConfig.Mode,
		Settings:        tuningFromConfig(e.settingsConfig),
		RuntimeSections: []string{"策略参数", "风控参数"},
		RestartSections: []string{"扫描间隔", "合约杠杆", "成本参数", "代币发现刷新", "页面刷新"},
	}
}

func (e *Engine) updateSettings(t TuningSettings) (SettingsResponse, error) {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	if t.Version != tuningFileVersion {
		return SettingsResponse{}, fmt.Errorf("%w: unsupported version %d", errInvalidTuning, t.Version)
	}
	before := tuningFromConfig(e.settingsConfig)
	candidate := e.settingsConfig
	applyTuning(&candidate, t)
	if err := candidate.Validate(); err != nil {
		return SettingsResponse{}, fmt.Errorf("%w: %v", errInvalidTuning, err)
	}
	changed, restart := changedTuningSections(before, t)
	if err := saveTuning(candidate.StateDir, t); err != nil {
		return SettingsResponse{}, fmt.Errorf("save tuning: %w", err)
	}
	e.settingsConfig = candidate
	strategy, risk := candidate.Strategy, candidate.Risk
	e.pendingStrategy = &strategy
	e.pendingRisk = &risk
	now := time.Now()
	if len(changed) > 0 {
		_ = appendLedger(candidate.StateDir, map[string]any{"time": now, "event": "config_update", "mode": candidate.Mode, "changed_sections": changed, "restart_required": len(restart) > 0})
		slog.Info("dashboard tuning saved", "changed_sections", changed, "restart_required", restart)
	}
	return SettingsResponse{
		Mode:                  candidate.Mode,
		Settings:              t,
		RuntimeSections:       []string{"策略参数", "风控参数"},
		RestartSections:       []string{"扫描间隔", "合约杠杆", "成本参数", "代币发现刷新", "页面刷新"},
		AppliedAfterNextCycle: !reflect.DeepEqual(before.Strategy, t.Strategy) || !reflect.DeepEqual(before.Risk, t.Risk),
		RestartRequired:       len(restart) > 0,
		ChangedSections:       changed,
		SavedAt:               &now,
	}, nil
}

func (e *Engine) applyPendingSettings() {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	if e.pendingStrategy != nil {
		e.cfg.Strategy = *e.pendingStrategy
		e.pendingStrategy = nil
	}
	if e.pendingRisk != nil {
		e.cfg.Risk = *e.pendingRisk
		e.pendingRisk = nil
	}
}
