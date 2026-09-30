package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func operationClientOrderID(opID, leg string) string {
	sum := sha256.Sum256([]byte(opID + ":" + leg))
	return fmt.Sprintf("fb-%x", sum[:12])
}

func (e *Engine) submitTrackedMarketOrder(ctx context.Context, op *PendingOperation, leg, side string, qty float64, reduceOnly bool) (OrderFill, error) {
	clientID := operationClientOrderID(op.ID, leg)
	if strings.Contains(leg, "restore") {
		op.CompensationClientOrderID = clientID
	} else {
		op.BinanceClientOrderID = clientID
	}
	op.UpdatedAt = time.Now()
	if err := saveState(e.cfg.StateDir, e.state); err != nil {
		return OrderFill{}, fmt.Errorf("persist Binance client order id: %w", err)
	}
	fill, err := e.binance.MarketOrderTracked(ctx, op.Symbol, side, qty, reduceOnly, clientID)
	if err != nil {
		// A timeout may happen after Binance accepted the order. Querying the
		// deterministic ID prevents a blind duplicate submission.
		queried, queryErr := e.binance.OrderByClientID(context.Background(), op.Symbol, clientID)
		if queryErr != nil {
			return OrderFill{}, fmt.Errorf("Binance order status unknown for %s: submit: %v; query by client id: %w", clientID, err, queryErr)
		}
		fill = queried
	}
	op.BinanceOrderID = fill.OrderID
	op.FuturesFilledQty = fill.ExecutedQty
	if err := e.updateOperation(op, op.Stage, nil); err != nil {
		return OrderFill{}, err
	}
	return fill, nil
}

func (e *Engine) reconcileLiveState(ctx context.Context) error {
	if e.cfg.Mode != "live" {
		e.accountReconciled = true
		return nil
	}
	if len(e.state.PendingOperations) > 0 {
		if err := e.recoverPendingOperations(ctx); err != nil {
			e.accountReconciled = false
			return err
		}
	}
	risks, err := e.binance.PositionRisks(ctx)
	if err != nil {
		e.accountReconciled = false
		return fmt.Errorf("read Binance positions: %w", err)
	}
	settingsChanged := false
	for symbol := range e.state.Positions {
		risk, ok := risks[symbol]
		if !ok {
			continue
		}
		marginMatches := strings.EqualFold(risk.MarginType, e.cfg.Binance.MarginType) || (strings.EqualFold(risk.MarginType, "CROSS") && e.cfg.Binance.MarginType == "CROSSED")
		if risk.Leverage != e.cfg.Binance.Leverage || !marginMatches {
			if err := e.binance.ConfigureSymbol(ctx, symbol); err != nil {
				e.accountReconciled = false
				return fmt.Errorf("apply leverage/margin mode to %s: %w", symbol, err)
			}
			settingsChanged = true
		}
	}
	if settingsChanged {
		risks, err = e.binance.PositionRisks(ctx)
		if err != nil {
			e.accountReconciled = false
			return fmt.Errorf("verify updated Binance positions: %w", err)
		}
	}
	account, err := e.binance.AccountRisk(ctx)
	if err != nil {
		e.accountReconciled = false
		return fmt.Errorf("read Binance account risk: %w", err)
	}
	chainUSDT, err := e.chain.Balance(ctx, e.cfg.BSC.USDTAddress, e.cfg.BSC.USDTDecimals)
	if err != nil {
		e.accountReconciled = false
		return fmt.Errorf("read BSC USDT balance: %w", err)
	}
	for symbol, p := range e.state.Positions {
		token, err := e.positionToken(p)
		if err != nil {
			e.accountReconciled = false
			return fmt.Errorf("%s token: %w", symbol, err)
		}
		balance, err := e.chain.Balance(ctx, token.BSCAddress, token.Decimals)
		if err != nil {
			e.accountReconciled = false
			return fmt.Errorf("%s BSC balance: %w", symbol, err)
		}
		risk, ok := risks[symbol]
		if !ok || risk.PositionAmount >= 0 {
			e.accountReconciled = false
			return fmt.Errorf("%s has no matching Binance short", symbol)
		}
		if risk.PositionSide != "" && !strings.EqualFold(risk.PositionSide, "BOTH") {
			e.accountReconciled = false
			return fmt.Errorf("%s position side %s is incompatible with one-way strategy", symbol, risk.PositionSide)
		}
		marginMatches := strings.EqualFold(risk.MarginType, e.cfg.Binance.MarginType) || (strings.EqualFold(risk.MarginType, "CROSS") && e.cfg.Binance.MarginType == "CROSSED")
		if risk.Leverage != e.cfg.Binance.Leverage || !marginMatches {
			e.accountReconciled = false
			return fmt.Errorf("%s leverage/margin mode is %dx/%s, expected %dx/%s", symbol, risk.Leverage, risk.MarginType, e.cfg.Binance.Leverage, e.cfg.Binance.MarginType)
		}
		actualShort := math.Abs(risk.PositionAmount)
		denom := math.Max(actualShort, p.ShortQty)
		if denom <= 0 || math.Abs(actualShort-p.ShortQty)/denom*100 > e.cfg.Risk.MaxHedgeDriftPercent {
			e.accountReconciled = false
			return fmt.Errorf("%s Binance short %.8f differs from saved %.8f", symbol, actualShort, p.ShortQty)
		}
		hedgeDenom := math.Max(p.TokenQty, p.ShortQty)
		if hedgeDenom <= 0 || math.Abs(p.TokenQty-p.ShortQty)/hedgeDenom*100 > e.cfg.Risk.MaxHedgeDriftPercent {
			e.accountReconciled = false
			return fmt.Errorf("%s saved spot %.8f and short %.8f exceed hedge drift limit", symbol, p.TokenQty, p.ShortQty)
		}
		if balance+1e-12 < p.TokenQty*(1-e.cfg.Risk.MaxHedgeDriftPercent/100) {
			e.accountReconciled = false
			return fmt.Errorf("%s BSC balance %.8f is below saved spot %.8f", symbol, balance, p.TokenQty)
		}
	}
	for symbol, risk := range risks {
		if risk.PositionAmount != 0 {
			if _, tracked := e.state.Positions[symbol]; !tracked {
				e.accountReconciled = false
				return fmt.Errorf("untracked Binance position %s amount %.8f", symbol, risk.PositionAmount)
			}
		}
	}
	// A configured token balance without a saved position is also an orphaned
	// leg. Live mode requires a dedicated strategy wallet so this is fail-closed.
	for symbol, token := range e.tokens {
		if _, tracked := e.state.Positions[symbol]; tracked {
			continue
		}
		balance, err := e.chain.Balance(ctx, token.BSCAddress, token.Decimals)
		if err != nil {
			e.accountReconciled = false
			return fmt.Errorf("%s BSC balance: %w", symbol, err)
		}
		if balance > 1e-12 {
			e.accountReconciled = false
			return fmt.Errorf("untracked BSC balance %s %.8f", symbol, balance)
		}
	}
	e.positionRisks = risks
	e.accountRisk = account
	e.chainAvailableUSDT = chainUSDT
	e.accountReconciled = true
	e.reconciliationError = ""
	return nil
}

func (e *Engine) recoverPendingOperations(ctx context.Context) error {
	ids := make([]string, 0, len(e.state.PendingOperations))
	for id := range e.state.PendingOperations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		op := e.state.PendingOperations[id]
		if op == nil {
			delete(e.state.PendingOperations, id)
			continue
		}
		token := TokenConfig{Enabled: true, BinanceSymbol: op.Symbol, BSCAddress: op.TokenAddress, Decimals: op.TokenDecimals, VerifiedContract: true}
		if op.Kind == "open" && op.CompensationChainTxHash != "" {
			rollbackHash := common.HexToHash(op.CompensationChainTxHash)
			rollbackStatus, err := e.chain.TransactionStatus(ctx, rollbackHash)
			if err != nil {
				return fmt.Errorf("pending %s spot rollback receipt: %w", op.Symbol, err)
			}
			switch rollbackStatus {
			case "pending":
				return fmt.Errorf("pending %s spot rollback transaction %s is not confirmed", op.Symbol, op.CompensationChainTxHash)
			case "success":
				risks, err := e.binance.PositionRisks(ctx)
				if err != nil {
					return fmt.Errorf("recover open %s after spot rollback: %w", op.Symbol, err)
				}
				if risk, ok := risks[op.Symbol]; ok && math.Abs(risk.PositionAmount) > 1e-12 {
					return fmt.Errorf("recover open %s: spot rollback succeeded but Binance position %.8f still exists", op.Symbol, risk.PositionAmount)
				}
				balance, err := e.chain.Balance(ctx, token.BSCAddress, token.Decimals)
				if err != nil {
					return fmt.Errorf("recover open %s after spot rollback balance: %w", op.Symbol, err)
				}
				residual := balance - op.BalanceBefore
				tolerance := math.Max(1e-12, math.Abs(op.PlannedTokenQty)*1e-6)
				if residual > tolerance {
					return fmt.Errorf("recover open %s: spot rollback succeeded but %.8f untracked token remains", op.Symbol, residual)
				}
				_ = appendLedger(e.cfg.StateDir, map[string]any{"time": time.Now(), "event": "operation_recovered", "kind": "open_rollback", "symbol": op.Symbol, "chain_tx_hash": op.ChainTxHash, "rollback_tx_hash": op.CompensationChainTxHash})
				if err := e.finishOperation(op); err != nil {
					return err
				}
				continue
			case "reverted":
				// The rollback did not change balances. Continue recovering the
				// original spot purchase and establish its futures hedge below.
			default:
				return fmt.Errorf("recover open %s: unsupported spot rollback transaction status %q", op.Symbol, rollbackStatus)
			}
		}
		var txStatus string
		if op.ChainTxHash != "" {
			hash := common.HexToHash(op.ChainTxHash)
			status, err := e.chain.TransactionStatus(ctx, hash)
			if err != nil {
				return fmt.Errorf("pending %s transaction receipt: %w", op.Symbol, err)
			}
			txStatus = status
			if status == "pending" {
				return fmt.Errorf("pending %s transaction %s is not confirmed", op.Symbol, op.ChainTxHash)
			}
		}
		switch op.Kind {
		case "open":
			if txStatus == "reverted" {
				risks, err := e.binance.PositionRisks(ctx)
				if err != nil {
					return fmt.Errorf("recover reverted open %s positions: %w", op.Symbol, err)
				}
				if risk, ok := risks[op.Symbol]; ok && math.Abs(risk.PositionAmount) > 1e-12 {
					return fmt.Errorf("recover reverted open %s: Binance position %.8f unexpectedly exists", op.Symbol, risk.PositionAmount)
				}
				if err := e.finishOperation(op); err != nil {
					return err
				}
				continue
			}
			balance, err := e.chain.Balance(ctx, token.BSCAddress, token.Decimals)
			if err != nil {
				return fmt.Errorf("recover open %s balance: %w", op.Symbol, err)
			}
			qty := balance - op.BalanceBefore
			if qty <= 0 {
				if op.ChainTxHash == "" {
					if err := e.finishOperation(op); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("recover open %s: confirmed chain transaction produced no balance increase", op.Symbol)
			}
			if op.ChainTxHash == "" {
				return fmt.Errorf("recover open %s: token balance increased by %.8f but no BSC transaction hash was durably recorded", op.Symbol, qty)
			}
			risks, err := e.binance.PositionRisks(ctx)
			if err != nil {
				return err
			}
			shortQty := 0.0
			shortPrice := 0.0
			if risk, ok := risks[op.Symbol]; ok && risk.PositionAmount < 0 {
				shortQty = math.Abs(risk.PositionAmount)
				shortPrice = risk.EntryPrice
				if shortPrice <= 0 {
					shortPrice = risk.MarkPrice
				}
			}
			if shortQty <= 0 && op.BinanceClientOrderID != "" {
				fill, queryErr := e.binance.OrderByClientID(ctx, op.Symbol, op.BinanceClientOrderID)
				if queryErr != nil {
					// Never submit another opening short while the deterministic
					// client ID cannot be resolved. The first order may have been
					// accepted even though its HTTP response was lost.
					return fmt.Errorf("recover open %s Binance order %s status is unknown: %w", op.Symbol, op.BinanceClientOrderID, queryErr)
				}
				shortQty, shortPrice = fill.ExecutedQty, fill.AvgPrice
			}
			if shortQty <= 0 {
				if err := e.binance.ConfigureSymbol(ctx, op.Symbol); err != nil {
					return fmt.Errorf("recover open %s configure: %w", op.Symbol, err)
				}
				rule, ok := e.symbols[op.Symbol]
				if !ok {
					return fmt.Errorf("recover open %s symbol rule missing", op.Symbol)
				}
				planned := floorStep(qty, rule.StepSize)
				fill, err := e.submitTrackedMarketOrder(ctx, op, "futures-open-recovery", "SELL", planned, false)
				if err != nil {
					return fmt.Errorf("recover open %s hedge: %w", op.Symbol, err)
				}
				shortQty, shortPrice = fill.ExecutedQty, fill.AvgPrice
			}
			driftDenom := math.Max(qty, shortQty)
			if driftDenom <= 0 || math.Abs(qty-shortQty)/driftDenom*100 > e.cfg.Risk.MaxHedgeDriftPercent {
				return fmt.Errorf("recover open %s hedge drift: spot %.8f short %.8f", op.Symbol, qty, shortQty)
			}
			now := time.Now()
			e.state.Positions[op.Symbol] = &Position{Symbol: op.Symbol, TokenAddress: op.TokenAddress, TokenDecimals: op.TokenDecimals, TokenQty: qty, SpotCostUSDT: op.TargetNotionalUSDT, ShortQty: shortQty, ShortEntryPrice: shortPrice, OpenedAt: op.CreatedAt, LastAdjustedAt: now, LastFundingTime: now}
			_ = appendLedger(e.cfg.StateDir, map[string]any{"time": now, "event": "operation_recovered", "kind": "open", "symbol": op.Symbol, "chain_tx_hash": op.ChainTxHash})
			if err := e.finishOperation(op); err != nil {
				return err
			}
		case "reduce":
			p := e.state.Positions[op.Symbol]
			if p == nil {
				return fmt.Errorf("recover reduction %s: saved position is missing", op.Symbol)
			}
			risks, err := e.binance.PositionRisks(ctx)
			if err != nil {
				return err
			}
			actualShort := 0.0
			if risk, ok := risks[op.Symbol]; ok && risk.PositionAmount < 0 {
				actualShort = math.Abs(risk.PositionAmount)
			}
			if op.BinanceClientOrderID != "" {
				if fill, queryErr := e.binance.OrderByClientID(ctx, op.Symbol, op.BinanceClientOrderID); queryErr == nil && fill.ExecutedQty > 0 && actualShort >= op.OriginalShortQty-1e-12 {
					actualShort = math.Max(0, op.OriginalShortQty-fill.ExecutedQty)
				}
			}
			if txStatus == "success" {
				balance, err := e.chain.Balance(ctx, token.BSCAddress, token.Decimals)
				if err != nil {
					return err
				}
				remainingToken := math.Max(0, op.OriginalTokenQty-op.PlannedTokenQty)
				if balance < remainingToken {
					remainingToken = balance
				}
				reductionFraction := 0.0
				if op.OriginalTokenQty > 0 {
					reductionFraction = math.Max(0, math.Min(1, (op.OriginalTokenQty-remainingToken)/op.OriginalTokenQty))
				}
				if op.QuoteBalanceRecorded && reductionFraction > 0 {
					quoteBalanceAfter, balanceErr := e.chain.Balance(ctx, e.cfg.BSC.USDTAddress, e.cfg.BSC.USDTDecimals)
					if balanceErr != nil {
						return balanceErr
					}
					exitUSDT := math.Max(0, quoteBalanceAfter-op.QuoteBalanceBefore)
					markPrice := p.ShortEntryPrice
					if risk, ok := risks[op.Symbol]; ok && risk.MarkPrice > 0 {
						markPrice = risk.MarkPrice
					}
					pnl, _, _ := partialReductionPnL(*p, reductionFraction, exitUSDT, markPrice, e.cfg.Binance.TakerFeeBPS, e.cfg.BSC.GasReserveUSDT)
					e.state.DailyRealizedPnL += pnl
					p.RealizedPnLUSDT += pnl
					op.QuoteBalanceAfter = quoteBalanceAfter
				}
				tokenFraction := 0.0
				if op.OriginalTokenQty > 0 {
					tokenFraction = remainingToken / op.OriginalTokenQty
				}
				p.TokenQty = remainingToken
				p.ShortQty = actualShort
				p.SpotCostUSDT *= tokenFraction
				p.FundingAccruedUSDT *= tokenFraction
				p.LastAdjustedAt = time.Now()
				if p.TokenQty <= 0 && p.ShortQty <= 0 {
					delete(e.state.Positions, op.Symbol)
				}
				_ = appendLedger(e.cfg.StateDir, map[string]any{"time": time.Now(), "event": "operation_recovered", "kind": "reduce", "symbol": op.Symbol, "chain_tx_hash": op.ChainTxHash})
				if err := e.finishOperation(op); err != nil {
					return err
				}
				continue
			}
			// The chain leg reverted or was never broadcast. Restore any futures
			// quantity that was closed so the original hedge is re-established.
			delta := math.Max(0, op.OriginalShortQty-actualShort)
			if delta > 0 {
				fill, err := e.submitTrackedMarketOrder(ctx, op, "futures-restore-recovery", "SELL", delta, false)
				if err != nil || fill.ExecutedQty+1e-12 < delta {
					return fmt.Errorf("recover reduction %s futures hedge failed: fill %.8f/%.8f err=%v", op.Symbol, fill.ExecutedQty, delta, err)
				}
			}
			if err := e.finishOperation(op); err != nil {
				return err
			}
		case "hedge":
			p := e.state.Positions[op.Symbol]
			if p == nil {
				return fmt.Errorf("recover hedge %s: saved position is missing", op.Symbol)
			}
			if op.BinanceClientOrderID == "" {
				// The intent was persisted, but no deterministic order ID was
				// recorded, so no Binance order could have been submitted.
				if err := e.finishOperation(op); err != nil {
					return err
				}
				continue
			}
			risks, err := e.binance.PositionRisks(ctx)
			if err != nil {
				return fmt.Errorf("recover hedge %s positions: %w", op.Symbol, err)
			}
			risk, ok := risks[op.Symbol]
			if !ok || risk.PositionAmount > 1e-12 {
				return fmt.Errorf("recover hedge %s: expected Binance short is missing or reversed", op.Symbol)
			}
			actualShort := math.Abs(risk.PositionAmount)
			if math.Abs(actualShort-op.OriginalShortQty) <= 1e-12 {
				if _, queryErr := e.binance.OrderByClientID(ctx, op.Symbol, op.BinanceClientOrderID); queryErr != nil {
					return fmt.Errorf("recover hedge %s Binance order %s status is unknown: %w", op.Symbol, op.BinanceClientOrderID, queryErr)
				}
				return fmt.Errorf("recover hedge %s: Binance order is filled but position quantity has not reconciled", op.Symbol)
			}
			p.ShortQty = actualShort
			p.LastAdjustedAt = time.Now()
			driftDenom := math.Max(p.TokenQty, p.ShortQty)
			if driftDenom <= 0 || math.Abs(p.TokenQty-p.ShortQty)/driftDenom*100 > e.cfg.Risk.MaxHedgeDriftPercent {
				if err := e.updateOperation(op, "manual_reconciliation_required", fmt.Errorf("recovered hedge remains outside drift limit: spot %.8f short %.8f", p.TokenQty, p.ShortQty)); err != nil {
					return err
				}
				return fmt.Errorf("recover hedge %s remains outside drift limit: spot %.8f short %.8f", op.Symbol, p.TokenQty, p.ShortQty)
			}
			_ = appendLedger(e.cfg.StateDir, map[string]any{"time": time.Now(), "event": "operation_recovered", "kind": "hedge", "symbol": op.Symbol, "side": op.BinanceSide, "short_qty": actualShort})
			if err := e.finishOperation(op); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported pending operation %s kind %q", op.ID, op.Kind)
		}
	}
	return saveState(e.cfg.StateDir, e.state)
}

func (e *Engine) beginOperation(kind, symbol, reason string, token TokenConfig) (*PendingOperation, error) {
	now := time.Now()
	op := &PendingOperation{
		ID:            fmt.Sprintf("%s-%s-%d", symbol, kind, now.UnixNano()),
		Kind:          kind,
		Stage:         "prepared",
		Symbol:        symbol,
		Reason:        reason,
		TokenAddress:  token.BSCAddress,
		TokenDecimals: token.Decimals,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if e.state.PendingOperations == nil {
		e.state.PendingOperations = map[string]*PendingOperation{}
	}
	e.state.PendingOperations[op.ID] = op
	if err := saveState(e.cfg.StateDir, e.state); err != nil {
		delete(e.state.PendingOperations, op.ID)
		return nil, fmt.Errorf("persist operation intent: %w", err)
	}
	return op, nil
}

func (e *Engine) updateOperation(op *PendingOperation, stage string, opErr error) error {
	if op == nil {
		return nil
	}
	op.Stage = stage
	op.UpdatedAt = time.Now()
	if opErr != nil {
		op.LastError = opErr.Error()
	} else {
		op.LastError = ""
	}
	return saveState(e.cfg.StateDir, e.state)
}

func (e *Engine) finishOperation(op *PendingOperation) error {
	if op == nil {
		return nil
	}
	delete(e.state.PendingOperations, op.ID)
	if err := saveState(e.cfg.StateDir, e.state); err != nil {
		e.state.PendingOperations[op.ID] = op
		return err
	}
	return nil
}

// executeLiveReduction runs both legs and restores the futures short if the
// chain leg fails. If even that compensation fails, the durable journal is
// deliberately retained and the global risk gate prevents any new exposure.
func (e *Engine) executeLiveReduction(ctx context.Context, p *Position, token TokenConfig, requestedFraction float64, reason string) (float64, float64, error) {
	if requestedFraction <= 0 || p.ShortQty <= 0 || p.TokenQty <= 0 {
		return 0, 0, fmt.Errorf("invalid reduction request")
	}
	rule, ok := e.symbols[p.Symbol]
	if !ok {
		return 0, 0, fmt.Errorf("Binance symbol rule %s is unavailable", p.Symbol)
	}
	closeQty := floorStep(p.ShortQty*math.Min(1, requestedFraction), rule.StepSize)
	if closeQty <= 0 || closeQty < rule.MinQty {
		return 0, 0, fmt.Errorf("reduction quantity %.8f is below Binance minimum", closeQty)
	}
	balanceBefore, err := e.chain.Balance(ctx, token.BSCAddress, token.Decimals)
	if err != nil {
		return 0, 0, fmt.Errorf("read spot balance before reduction: %w", err)
	}
	quoteBalanceBefore, err := e.chain.Balance(ctx, e.cfg.BSC.USDTAddress, e.cfg.BSC.USDTDecimals)
	if err != nil {
		return 0, 0, fmt.Errorf("read USDT balance before reduction: %w", err)
	}
	op, err := e.beginOperation("reduce", p.Symbol, reason, token)
	if err != nil {
		return 0, 0, err
	}
	op.OriginalTokenQty = p.TokenQty
	op.OriginalShortQty = p.ShortQty
	op.PlannedFuturesQty = closeQty
	op.BalanceBefore = balanceBefore
	op.QuoteBalanceBefore = quoteBalanceBefore
	op.QuoteBalanceRecorded = true
	if err := e.updateOperation(op, "futures_submitting", nil); err != nil {
		return 0, 0, err
	}
	fill, err := e.submitTrackedMarketOrder(ctx, op, "futures-reduce", "BUY", closeQty, true)
	if err != nil {
		_ = e.updateOperation(op, "futures_status_unknown", err)
		return 0, 0, err
	}
	op.FuturesFilledQty = math.Min(fill.ExecutedQty, p.ShortQty)
	actualFraction := math.Min(1, op.FuturesFilledQty/p.ShortQty)
	op.PlannedTokenQty = p.TokenQty * actualFraction
	if err := e.updateOperation(op, "futures_reduced", nil); err != nil {
		return 0, 0, err
	}

	compensate := func(cause error) (float64, float64, error) {
		compensation, compensateErr := e.submitTrackedMarketOrder(ctx, op, "futures-restore", "SELL", op.FuturesFilledQty, false)
		if compensateErr == nil && compensation.ExecutedQty+rule.StepSize/2 >= op.FuturesFilledQty {
			if finishErr := e.finishOperation(op); finishErr != nil {
				return 0, 0, fmt.Errorf("spot reduction failed and futures hedge was restored, but operation journal cleanup failed: %w", finishErr)
			}
			return 0, 0, fmt.Errorf("spot reduction failed; futures hedge restored: %w", cause)
		}
		if compensateErr == nil {
			compensateErr = fmt.Errorf("compensation partially filled %.8f of %.8f", compensation.ExecutedQty, op.FuturesFilledQty)
		}
		combined := fmt.Errorf("spot reduction failed: %v; futures compensation failed: %w", cause, compensateErr)
		_ = e.updateOperation(op, "manual_reconciliation_required", combined)
		_ = e.alert.Send(ctx, "unhedged-"+p.Symbol, "CRITICAL", combined.Error())
		return 0, 0, combined
	}

	quoted, err := e.chain.SellQuote(ctx, token, op.PlannedTokenQty)
	if err != nil {
		return compensate(err)
	}
	if err := e.updateOperation(op, "spot_submitting", nil); err != nil {
		return compensate(err)
	}
	recordBroadcast := func(hash common.Hash) error {
		op.ChainTxHash = hash.Hex()
		op.Stage = "spot_broadcast"
		op.UpdatedAt = time.Now()
		return saveState(e.cfg.StateDir, e.state)
	}
	_, tx, err := e.chain.SwapExactInputTracked(ctx, token.BSCAddress, e.cfg.BSC.USDTAddress, token.Decimals, e.cfg.BSC.USDTDecimals, op.PlannedTokenQty, quoted, recordBroadcast)
	if err != nil {
		if tx != (common.Hash{}) {
			op.ChainTxHash = tx.Hex()
			_ = e.updateOperation(op, "chain_status_unknown", err)
			_ = e.alert.Send(ctx, "chain-status-"+p.Symbol, "CRITICAL", fmt.Sprintf("%s BSC transaction %s was broadcast but its final status is unknown; all new risk is locked", p.Symbol, tx.Hex()))
			return 0, 0, fmt.Errorf("BSC transaction %s was broadcast; compensation is locked until receipt reconciliation: %w", tx.Hex(), err)
		}
		return compensate(err)
	}
	op.ChainTxHash = tx.Hex()
	op.BalanceAfter = balanceBefore - op.PlannedTokenQty
	if quoteBalanceAfter, balanceErr := e.chain.Balance(ctx, e.cfg.BSC.USDTAddress, e.cfg.BSC.USDTDecimals); balanceErr == nil {
		op.QuoteBalanceAfter = quoteBalanceAfter
		if actualExit := quoteBalanceAfter - quoteBalanceBefore; actualExit > 0 {
			quoted = actualExit
		}
	}
	if err := e.updateOperation(op, "completed", nil); err != nil {
		return 0, 0, err
	}
	if err := e.finishOperation(op); err != nil {
		return 0, 0, err
	}
	return actualFraction, quoted, nil
}

func (e *Engine) emergencySellSpotTracked(ctx context.Context, op *PendingOperation, token TokenConfig, qty float64) error {
	quoted, err := e.chain.SellQuote(ctx, token, qty)
	if err != nil {
		return err
	}
	recordBroadcast := func(hash common.Hash) error {
		op.CompensationChainTxHash = hash.Hex()
		op.Stage = "spot_rollback_broadcast"
		op.UpdatedAt = time.Now()
		return saveState(e.cfg.StateDir, e.state)
	}
	_, hash, err := e.chain.SwapExactInputTracked(ctx, token.BSCAddress, e.cfg.BSC.USDTAddress, token.Decimals, e.cfg.BSC.USDTDecimals, qty, quoted, recordBroadcast)
	if hash != (common.Hash{}) {
		op.CompensationChainTxHash = hash.Hex()
	}
	if err != nil {
		_ = e.updateOperation(op, "spot_rollback_status_unknown", err)
		return err
	}
	return e.updateOperation(op, "spot_rollback_confirmed", nil)
}
