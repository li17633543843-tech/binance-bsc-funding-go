package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func loadState(dir string) (*BotState, error) {
	path := filepath.Join(dir, "state.json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		now := time.Now()
		return &BotState{Version: 1, StartedAt: now, UpdatedAt: now, Positions: map[string]*Position{}, FundingEWMA: map[string]float64{}, FundingSamples: map[string]int{}, WeakFundingScans: map[string]int{}, FundingHistory: map[string][]FundingRecord{}, FundingStats: map[string]FundingStats{}, WeakFundingSettlements: map[string]int{}, EntryConfirmations: map[string]int{}, CooldownUntil: map[string]time.Time{}, DepthBreachScans: map[string]int{}, LiquidationBreachScans: map[string]int{}, DailyDate: now.Format("2006-01-02")}, nil
	}
	if err != nil {
		return nil, err
	}
	var s BotState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Version != 1 {
		return nil, fmt.Errorf("unsupported state version %d", s.Version)
	}
	if s.Positions == nil {
		s.Positions = map[string]*Position{}
	}
	if s.FundingEWMA == nil {
		s.FundingEWMA = map[string]float64{}
	}
	if s.FundingSamples == nil {
		s.FundingSamples = map[string]int{}
	}
	if s.WeakFundingScans == nil {
		s.WeakFundingScans = map[string]int{}
	}
	if s.FundingHistory == nil {
		s.FundingHistory = map[string][]FundingRecord{}
	}
	if s.FundingStats == nil {
		s.FundingStats = map[string]FundingStats{}
	}
	if s.WeakFundingSettlements == nil {
		s.WeakFundingSettlements = map[string]int{}
	}
	if s.EntryConfirmations == nil {
		s.EntryConfirmations = map[string]int{}
	}
	if s.CooldownUntil == nil {
		s.CooldownUntil = map[string]time.Time{}
	}
	if s.DepthBreachScans == nil {
		s.DepthBreachScans = map[string]int{}
	}
	if s.LiquidationBreachScans == nil {
		s.LiquidationBreachScans = map[string]int{}
	}
	return &s, nil
}

func saveState(dir string, s *BotState) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	s.UpdatedAt = time.Now()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "state.json.tmp")
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func appendLedger(dir string, event any) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "ledger.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(event)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = f.Write(b)
	return err
}

func readLedger(dir string, limit int) ([]map[string]any, error) {
	if limit < 1 {
		return []map[string]any{}, nil
	}
	f, err := os.Open(filepath.Join(dir, "ledger.jsonl"))
	if os.IsNotExist(err) {
		return []map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows := make([]map[string]any, 0, limit)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if len(rows) == limit {
			copy(rows, rows[1:])
			rows[len(rows)-1] = event
		} else {
			rows = append(rows, event)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	return rows, nil
}
