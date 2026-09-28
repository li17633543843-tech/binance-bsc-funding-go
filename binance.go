package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type BinanceClient struct {
	cfg             BinanceConfig
	http            *http.Client
	apiKey          string
	secret          string
	mu              sync.Mutex
	rateLimitUntil  time.Time
	rateLimitStatus int
	historyNext     time.Time
}

type BinanceRateLimitError struct {
	StatusCode int
	Until      time.Time
	Method     string
	Path       string
	Message    string
	Local      bool
}

func (e *BinanceRateLimitError) Error() string {
	prefix := "Binance rate limited"
	if e.Local {
		prefix = "Binance request paused locally"
	}
	return fmt.Sprintf("%s (HTTP %d) until %s for %s %s: %s", prefix, e.StatusCode, e.Until.Format(time.RFC3339), e.Method, e.Path, e.Message)
}

type BinanceSymbol struct {
	Symbol      string
	Status      string
	Base        string
	Quote       string
	StepSize    float64
	MinQty      float64
	MinNotional float64
}

type OrderFill struct {
	OrderID     int64
	Symbol      string
	Side        string
	ExecutedQty float64
	AvgPrice    float64
	QuoteQty    float64
}

func NewBinanceClient(c BinanceConfig) *BinanceClient {
	return &BinanceClient{cfg: c, http: &http.Client{Timeout: time.Duration(c.RequestTimeoutSeconds) * time.Second}, apiKey: os.Getenv(c.APIKeyEnv), secret: os.Getenv(c.SecretKeyEnv)}
}

func (b *BinanceClient) public(ctx context.Context, path string, q url.Values, out any) error {
	return b.do(ctx, http.MethodGet, path, q, false, out)
}

func (b *BinanceClient) signed(ctx context.Context, method, path string, q url.Values, out any) error {
	if b.apiKey == "" || b.secret == "" {
		return fmt.Errorf("Binance credentials are missing")
	}
	q.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	q.Set("recvWindow", strconv.FormatInt(b.cfg.RecvWindowMS, 10))
	mac := hmac.New(sha256.New, []byte(b.secret))
	_, _ = mac.Write([]byte(q.Encode()))
	q.Set("signature", hex.EncodeToString(mac.Sum(nil)))
	return b.do(ctx, method, path, q, true, out)
}

func (b *BinanceClient) do(ctx context.Context, method, path string, q url.Values, signed bool, out any) error {
	if err := b.rateLimitError(method, path); err != nil {
		return err
	}
	endpoint := strings.TrimRight(b.cfg.BaseURL, "/") + path
	var body io.Reader
	if method == http.MethodGet {
		endpoint += "?" + q.Encode()
	} else {
		body = strings.NewReader(q.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if signed {
		req.Header.Set("X-MBX-APIKEY", b.apiKey)
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusTeapot {
		until := binanceRetryAt(resp, raw, time.Now())
		b.setRateLimit(resp.StatusCode, until)
		return &BinanceRateLimitError{StatusCode: resp.StatusCode, Until: until, Method: method, Path: path, Message: strings.TrimSpace(string(raw))}
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("Binance %s %s HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("Binance %s decode: %w", path, err)
	}
	return nil
}

func (b *BinanceClient) RateLimitUntil() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rateLimitUntil
}

func (b *BinanceClient) rateLimitError(method, path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.rateLimitUntil.After(time.Now()) {
		return nil
	}
	return &BinanceRateLimitError{
		StatusCode: b.rateLimitStatus,
		Until:      b.rateLimitUntil,
		Method:     method,
		Path:       path,
		Message:    "cooldown is active; no HTTP request was sent",
		Local:      true,
	}
}

func (b *BinanceClient) setRateLimit(status int, until time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if until.After(b.rateLimitUntil) {
		b.rateLimitUntil = until
		b.rateLimitStatus = status
	}
}

func binanceRetryAt(resp *http.Response, raw []byte, now time.Time) time.Time {
	until := time.Time{}
	var payload struct {
		Message string `json:"msg"`
	}
	if json.Unmarshal(raw, &payload) == nil {
		const marker = "banned until "
		if start := strings.Index(payload.Message, marker); start >= 0 {
			digits := payload.Message[start+len(marker):]
			if end := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
				digits = digits[:end]
			}
			if millis, err := strconv.ParseInt(digits, 10, 64); err == nil && millis > 0 {
				until = time.UnixMilli(millis)
			}
		}
	}
	if value := strings.TrimSpace(resp.Header.Get("Retry-After")); value != "" {
		var retryAt time.Time
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			retryAt = now.Add(time.Duration(seconds) * time.Second)
		} else if parsed, err := http.ParseTime(value); err == nil {
			retryAt = parsed
		}
		if retryAt.After(until) {
			until = retryAt
		}
	}
	if until.IsZero() {
		if resp.StatusCode == http.StatusTeapot {
			until = now.Add(5 * time.Minute)
		} else {
			until = now.Add(time.Minute)
		}
	}
	// Avoid resuming on the exact boundary while Binance edge nodes converge.
	return until.Add(5 * time.Second)
}

func (b *BinanceClient) waitFundingHistorySlot(ctx context.Context) error {
	if err := b.rateLimitError(http.MethodGet, "/fapi/v1/fundingRate"); err != nil {
		return err
	}
	b.mu.Lock()
	now := time.Now()
	slot := now
	if b.historyNext.After(slot) {
		slot = b.historyNext
	}
	b.historyNext = slot.Add(750 * time.Millisecond)
	b.mu.Unlock()
	if wait := time.Until(slot); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return b.rateLimitError(http.MethodGet, "/fapi/v1/fundingRate")
}

func (b *BinanceClient) Markets(ctx context.Context) (map[string]FundingMarket, error) {
	var rows []struct {
		Symbol          string `json:"symbol"`
		MarkPrice       string `json:"markPrice"`
		IndexPrice      string `json:"indexPrice"`
		LastFundingRate string `json:"lastFundingRate"`
		NextFundingTime int64  `json:"nextFundingTime"`
		Time            int64  `json:"time"`
	}
	if err := b.public(ctx, "/fapi/v1/premiumIndex", url.Values{}, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]FundingMarket, len(rows))
	for _, r := range rows {
		mark, e1 := strconv.ParseFloat(r.MarkPrice, 64)
		index, e2 := strconv.ParseFloat(r.IndexPrice, 64)
		rate, e3 := strconv.ParseFloat(r.LastFundingRate, 64)
		if e1 != nil || e2 != nil || e3 != nil || mark <= 0 {
			continue
		}
		out[r.Symbol] = FundingMarket{Symbol: r.Symbol, MarkPrice: mark, IndexPrice: index, FundingRate: rate, NextFundingTime: time.UnixMilli(r.NextFundingTime), UpdatedAt: time.UnixMilli(r.Time)}
	}
	return out, nil
}

func (b *BinanceClient) Symbols(ctx context.Context) (map[string]BinanceSymbol, error) {
	var response struct {
		Symbols []struct {
			Symbol       string `json:"symbol"`
			Status       string `json:"status"`
			Base         string `json:"baseAsset"`
			Quote        string `json:"quoteAsset"`
			ContractType string `json:"contractType"`
			Filters      []struct {
				Type     string `json:"filterType"`
				StepSize string `json:"stepSize"`
				MinQty   string `json:"minQty"`
				Notional string `json:"notional"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := b.public(ctx, "/fapi/v1/exchangeInfo", url.Values{}, &response); err != nil {
		return nil, err
	}
	out := map[string]BinanceSymbol{}
	for _, s := range response.Symbols {
		if s.Status != "TRADING" || s.ContractType != "PERPETUAL" || s.Quote != "USDT" {
			continue
		}
		v := BinanceSymbol{Symbol: s.Symbol, Status: s.Status, Base: s.Base, Quote: s.Quote}
		for _, f := range s.Filters {
			switch f.Type {
			case "LOT_SIZE", "MARKET_LOT_SIZE":
				if x, e := strconv.ParseFloat(f.StepSize, 64); e == nil && x > 0 {
					v.StepSize = x
				}
				if x, e := strconv.ParseFloat(f.MinQty, 64); e == nil && x > 0 {
					v.MinQty = x
				}
			case "MIN_NOTIONAL":
				v.MinNotional, _ = strconv.ParseFloat(f.Notional, 64)
			}
		}
		out[s.Symbol] = v
	}
	return out, nil
}

func (b *BinanceClient) FundingHistory(ctx context.Context, symbol string, start time.Time, limit int) ([]FundingRecord, error) {
	if err := b.waitFundingHistorySlot(ctx); err != nil {
		return nil, err
	}
	q := url.Values{"symbol": {symbol}, "limit": {strconv.Itoa(limit)}}
	if !start.IsZero() {
		q.Set("startTime", strconv.FormatInt(start.UnixMilli(), 10))
	}
	var rows []struct {
		Rate      string `json:"fundingRate"`
		Time      int64  `json:"fundingTime"`
		MarkPrice string `json:"markPrice"`
	}
	if err := b.public(ctx, "/fapi/v1/fundingRate", q, &rows); err != nil {
		return nil, err
	}
	out := make([]FundingRecord, 0, len(rows))
	for _, r := range rows {
		rate, rateErr := strconv.ParseFloat(r.Rate, 64)
		mark, _ := strconv.ParseFloat(r.MarkPrice, 64)
		if rateErr == nil && r.Time > 0 {
			out = append(out, FundingRecord{Time: time.UnixMilli(r.Time), Rate: rate, MarkPrice: mark})
		}
	}
	return out, nil
}

func (b *BinanceClient) FundingIntervals(ctx context.Context) (map[string]float64, error) {
	var rows []struct {
		Symbol               string `json:"symbol"`
		FundingIntervalHours int    `json:"fundingIntervalHours"`
	}
	if err := b.public(ctx, "/fapi/v1/fundingInfo", url.Values{}, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		if r.Symbol != "" && r.FundingIntervalHours > 0 {
			out[r.Symbol] = float64(r.FundingIntervalHours)
		}
	}
	return out, nil
}

func (b *BinanceClient) AccountRisk(ctx context.Context) (FuturesAccountRisk, error) {
	var row struct {
		TotalWalletBalance string `json:"totalWalletBalance"`
		TotalUnrealizedPnL string `json:"totalUnrealizedProfit"`
		TotalMarginBalance string `json:"totalMarginBalance"`
		TotalMaintMargin   string `json:"totalMaintMargin"`
		AvailableBalance   string `json:"availableBalance"`
	}
	if err := b.signed(ctx, http.MethodGet, "/fapi/v3/account", url.Values{}, &row); err != nil {
		return FuturesAccountRisk{}, err
	}
	parse := func(raw string) float64 {
		value, _ := strconv.ParseFloat(raw, 64)
		return value
	}
	risk := FuturesAccountRisk{
		TotalWalletBalance: parse(row.TotalWalletBalance),
		TotalUnrealizedPnL: parse(row.TotalUnrealizedPnL),
		TotalMarginBalance: parse(row.TotalMarginBalance),
		TotalMaintMargin:   parse(row.TotalMaintMargin),
		AvailableBalance:   parse(row.AvailableBalance),
	}
	if risk.TotalMarginBalance > 0 {
		risk.MarginRatioPercent = risk.TotalMaintMargin / risk.TotalMarginBalance * 100
	} else if risk.TotalMaintMargin > 0 {
		risk.MarginRatioPercent = 100
	}
	return risk, nil
}

func (b *BinanceClient) PositionRisks(ctx context.Context) (map[string]PositionRisk, error) {
	var rows []struct {
		Symbol           string `json:"symbol"`
		PositionAmt      string `json:"positionAmt"`
		MarkPrice        string `json:"markPrice"`
		LiquidationPrice string `json:"liquidationPrice"`
		UnrealizedProfit string `json:"unRealizedProfit"`
	}
	if err := b.signed(ctx, http.MethodGet, "/fapi/v3/positionRisk", url.Values{}, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]PositionRisk, len(rows))
	for _, r := range rows {
		p, _ := strconv.ParseFloat(r.PositionAmt, 64)
		m, _ := strconv.ParseFloat(r.MarkPrice, 64)
		l, _ := strconv.ParseFloat(r.LiquidationPrice, 64)
		u, _ := strconv.ParseFloat(r.UnrealizedProfit, 64)
		if r.Symbol != "" && p != 0 {
			out[r.Symbol] = PositionRisk{Symbol: r.Symbol, PositionAmount: p, MarkPrice: m, LiquidationPrice: l, UnrealizedProfit: u}
		}
	}
	return out, nil
}

func (b *BinanceClient) PositionRisk(ctx context.Context, symbol string) (PositionRisk, error) {
	risks, err := b.PositionRisks(ctx)
	if err != nil {
		return PositionRisk{}, err
	}
	if risk, ok := risks[symbol]; ok {
		return risk, nil
	}
	return PositionRisk{}, fmt.Errorf("Binance position %s not found", symbol)
}

func (b *BinanceClient) ConfigureSymbol(ctx context.Context, symbol string) error {
	var ignored any
	if err := b.signed(ctx, http.MethodPost, "/fapi/v1/leverage", url.Values{"symbol": {symbol}, "leverage": {strconv.Itoa(b.cfg.Leverage)}}, &ignored); err != nil {
		return err
	}
	// The requested margin type may already be set; Binance returns -4046 in that harmless case.
	err := b.signed(ctx, http.MethodPost, "/fapi/v1/marginType", url.Values{"symbol": {symbol}, "marginType": {b.cfg.MarginType}}, &ignored)
	if err != nil && !strings.Contains(err.Error(), "-4046") {
		return err
	}
	return nil
}

func (b *BinanceClient) MarketOrder(ctx context.Context, symbol, side string, qty float64, reduceOnly bool) (OrderFill, error) {
	q := url.Values{"symbol": {symbol}, "side": {side}, "type": {"MARKET"}, "quantity": {strconv.FormatFloat(qty, 'f', -1, 64)}, "newOrderRespType": {"RESULT"}}
	if reduceOnly {
		q.Set("reduceOnly", "true")
	}
	var r struct {
		OrderID     int64  `json:"orderId"`
		Symbol      string `json:"symbol"`
		Side        string `json:"side"`
		ExecutedQty string `json:"executedQty"`
		AvgPrice    string `json:"avgPrice"`
		CumQuote    string `json:"cumQuote"`
	}
	if err := b.signed(ctx, http.MethodPost, "/fapi/v1/order", q, &r); err != nil {
		return OrderFill{}, err
	}
	qtyOut, _ := strconv.ParseFloat(r.ExecutedQty, 64)
	price, _ := strconv.ParseFloat(r.AvgPrice, 64)
	quote, _ := strconv.ParseFloat(r.CumQuote, 64)
	if qtyOut <= 0 || price <= 0 {
		return OrderFill{}, fmt.Errorf("Binance order %d returned no fill", r.OrderID)
	}
	return OrderFill{OrderID: r.OrderID, Symbol: r.Symbol, Side: r.Side, ExecutedQty: qtyOut, AvgPrice: price, QuoteQty: quote}, nil
}

func floorStep(qty, step float64) float64 {
	if step <= 0 {
		return qty
	}
	return float64(int64((qty+step*1e-9)/step)) * step
}
