package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "config.json", "configuration file")
	check := flag.Bool("check", false, "validate configuration and exit")
	probe := flag.String("probe", "", "quote one configured Binance symbol and exit")
	flag.Parse()
	cfg, err := loadEffectiveConfig(*configPath)
	if err != nil {
		fatal(err)
	}
	if *check {
		fmt.Println("configuration valid")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	engine, err := newEngineWithRateLimitWait(ctx, cfg)
	if err != nil {
		fatal(err)
	}
	defer engine.Close()
	if *probe != "" {
		t, ok := engine.tokens[*probe]
		if !ok && cfg.Discovery.Enabled {
			rule, supported := engine.symbols[*probe]
			if !supported {
				fatal(fmt.Errorf("probe symbol %s is not a tradable Binance USDT perpetual", *probe))
			}
			matches, searchErr := engine.chain.SearchTokens(ctx, rule.Base)
			if searchErr != nil {
				fatal(fmt.Errorf("search %s on OKX DEX BSC: %w", rule.Base, searchErr))
			}
			markets, marketErr := engine.binance.Markets(ctx)
			if marketErr != nil {
				fatal(fmt.Errorf("load Binance mark price for %s: %w", *probe, marketErr))
			}
			market, marketOK := markets[*probe]
			if !marketOK {
				fatal(fmt.Errorf("Binance mark price for %s is unavailable", *probe))
			}
			minLiquidity := cfg.Risk.TargetNotionalPerCoinUSDT * cfg.Risk.DepthSafetyMultiplier
			if minLiquidity < 100 {
				minLiquidity = 100
			}
			match, reason, matched := selectUniqueSearchToken(matches, market.MarkPrice, minLiquidity)
			if !matched {
				fatal(fmt.Errorf("probe symbol %s has %d exact BSC matches but no unique safe market match (%s)", *probe, len(matches), reason))
			}
			decimals, parseErr := strconv.ParseUint(match.Decimals, 10, 8)
			if parseErr != nil || decimals > 36 {
				fatal(fmt.Errorf("probe symbol %s returned invalid token decimals", *probe))
			}
			t = TokenConfig{Enabled: true, Symbol: rule.Base, BinanceSymbol: *probe, BSCAddress: match.TokenContractAddress, Decimals: uint8(decimals), MaxNotionalUSDT: cfg.Risk.MaxNotionalPerCoinUSDT, VerifiedContract: true}
			ok = true
		}
		if !ok {
			fatal(fmt.Errorf("probe symbol %s is not enabled", *probe))
		}
		capacity, quote, err := engine.chain.DepthCapacity(ctx, t, cfg)
		if err != nil {
			fatal(err)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"symbol": *probe, "depth_capacity_usdt": capacity, "max_safe_notional_usdt": maxSafeNotional(cfg, t, capacity), "quote": quote})
		return
	}
	if _, err := startHTTP(ctx.Done(), cfg.HealthListen, engine); err != nil {
		fatal(err)
	}
	slog.Info("starting", "version", version, "mode", cfg.Mode, "tokens", len(engine.tokens), "state_dir", cfg.StateDir)
	if err := engine.Run(ctx); err != nil && err != context.Canceled {
		fatal(err)
	}
}

func newEngineWithRateLimitWait(ctx context.Context, cfg Config) (*Engine, error) {
	for {
		engine, err := NewEngine(ctx, cfg)
		if err == nil {
			return engine, nil
		}
		var rateLimit *BinanceRateLimitError
		if !errors.As(err, &rateLimit) {
			return nil, err
		}
		wait := time.Until(rateLimit.Until)
		if wait <= 0 {
			wait = time.Minute
		}
		slog.Warn("Binance rate limit active during startup; waiting without exiting", "retry_at", time.Now().Add(wait), "wait", wait)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func fatal(err error) { slog.Error("fatal", "error", err); os.Exit(1) }
