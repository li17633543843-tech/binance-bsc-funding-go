package main

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// binanceGateway is the exchange boundary used by the strategy engine. Keeping
// this boundary explicit lets integration tests inject partial fills, missing
// positions and transport failures without talking to a real account.
type binanceGateway interface {
	RateLimitUntil() time.Time
	Markets(context.Context) (map[string]FundingMarket, error)
	Symbols(context.Context) (map[string]BinanceSymbol, error)
	FundingIntervals(context.Context) (map[string]float64, error)
	FundingHistory(context.Context, string, time.Time, int) ([]FundingRecord, error)
	AccountRisk(context.Context) (FuturesAccountRisk, error)
	PositionRisks(context.Context) (map[string]PositionRisk, error)
	PositionMode(context.Context) (bool, error)
	ConfigureSymbol(context.Context, string) error
	MarketOrder(context.Context, string, string, float64, bool) (OrderFill, error)
	MarketOrderTracked(context.Context, string, string, float64, bool, string) (OrderFill, error)
	OrderByClientID(context.Context, string, string) (OrderFill, error)
}

// chainGateway is the BSC/DEX boundary used by the strategy engine.
type chainGateway interface {
	Close()
	TokenCatalog(context.Context) ([]OKXCatalogToken, error)
	SearchTokens(context.Context, string) ([]OKXCatalogToken, error)
	SearchTokenAddress(context.Context, string) ([]OKXCatalogToken, error)
	DepthCapacity(context.Context, TokenConfig, Config) (float64, ChainQuote, error)
	PositionExitCapacity(context.Context, TokenConfig, float64, float64, float64) (float64, PositionExitQuote, error)
	BestQuote(context.Context, TokenConfig, float64, float64) (ChainQuote, error)
	SellQuote(context.Context, TokenConfig, float64) (float64, error)
	Balance(context.Context, string, uint8) (float64, error)
	SwapExactInput(context.Context, string, string, uint8, uint8, float64, float64) (float64, common.Hash, error)
	SwapExactInputTracked(context.Context, string, string, uint8, uint8, float64, float64, func(common.Hash) error) (float64, common.Hash, error)
	TransactionStatus(context.Context, common.Hash) (string, error)
}
