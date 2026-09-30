package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

const erc20ABIJSON = `[{"inputs":[{"name":"owner","type":"address"}],"name":"balanceOf","outputs":[{"name":"","type":"uint256"}],"stateMutability":"view","type":"function"},{"inputs":[{"name":"owner","type":"address"},{"name":"spender","type":"address"}],"name":"allowance","outputs":[{"name":"","type":"uint256"}],"stateMutability":"view","type":"function"}]`

type okxToken struct {
	Decimal              string `json:"decimal"`
	IsHoneyPot           bool   `json:"isHoneyPot"`
	TaxRate              string `json:"taxRate"`
	TokenContractAddress string `json:"tokenContractAddress"`
	TokenSymbol          string `json:"tokenSymbol"`
}

type okxRoute struct {
	DexProtocol struct {
		DexName string `json:"dexName"`
		Percent string `json:"percent"`
	} `json:"dexProtocol"`
}

type okxQuote struct {
	ChainIndex      string     `json:"chainIndex"`
	FromTokenAmount string     `json:"fromTokenAmount"`
	ToTokenAmount   string     `json:"toTokenAmount"`
	EstimateGasFee  string     `json:"estimateGasFee"`
	TradeFee        string     `json:"tradeFee"`
	PriceImpact     string     `json:"priceImpactPercent"`
	Router          string     `json:"router"`
	FromToken       okxToken   `json:"fromToken"`
	ToToken         okxToken   `json:"toToken"`
	DexRouterList   []okxRoute `json:"dexRouterList"`
}

type okxSwap struct {
	RouterResult okxQuote `json:"routerResult"`
	Tx           struct {
		From             string `json:"from"`
		To               string `json:"to"`
		Data             string `json:"data"`
		Value            string `json:"value"`
		Gas              string `json:"gas"`
		GasPrice         string `json:"gasPrice"`
		MinReceiveAmount string `json:"minReceiveAmount"`
	} `json:"tx"`
}

type okxApproval struct {
	Data               string `json:"data"`
	DexContractAddress string `json:"dexContractAddress"`
	GasLimit           string `json:"gasLimit"`
	GasPrice           string `json:"gasPrice"`
}

type OKXCatalogToken struct {
	Decimals             string `json:"decimals"`
	TokenContractAddress string `json:"tokenContractAddress"`
	TokenName            string `json:"tokenName"`
	TokenSymbol          string `json:"tokenSymbol"`
	Price                string `json:"price,omitempty"`
	Liquidity            string `json:"liquidity,omitempty"`
	Holders              string `json:"holders,omitempty"`
	CommunityRecognized  bool   `json:"-"`
}

type PositionExitQuote struct {
	TokenQty       float64
	ReferenceUSDT  float64
	OutputUSDT     float64
	ExecutionPrice float64
	PriceImpactBPS float64
}

type okxTokenTags struct {
	CommunityRecognized bool `json:"communityRecognized"`
}

type okxTokenSearchResult struct {
	ChainIndex           string       `json:"chainIndex"`
	Decimal              string       `json:"decimal"`
	TokenContractAddress string       `json:"tokenContractAddress"`
	TokenName            string       `json:"tokenName"`
	TokenSymbol          string       `json:"tokenSymbol"`
	Price                string       `json:"price"`
	Liquidity            string       `json:"liquidity"`
	Holders              string       `json:"holders"`
	TagList              okxTokenTags `json:"tagList"`
}

type okxEnvelope[T any] struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []T    `json:"data"`
}

type OKXDEXClient struct {
	bsc         BSCConfig
	cfg         OKXDEXConfig
	http        *http.Client
	client      *ethclient.Client
	erc20ABI    abi.ABI
	wallet      common.Address
	key         *ecdsa.PrivateKey
	apiKey      string
	secret      string
	passphrase  string
	projectID   string
	requestMu   sync.Mutex
	nextRequest time.Time
}

func NewOKXDEXClient(ctx context.Context, bsc BSCConfig, cfg OKXDEXConfig, privateKeyHex string) (*OKXDEXClient, error) {
	apiKey, secret := os.Getenv(cfg.APIKeyEnv), os.Getenv(cfg.SecretKeyEnv)
	passphrase, projectID := os.Getenv(cfg.PassphraseEnv), os.Getenv(cfg.ProjectIDEnv)
	if apiKey == "" || secret == "" || passphrase == "" || projectID == "" {
		return nil, fmt.Errorf("OKX DEX API credentials are missing; set %s, %s, %s and %s", cfg.APIKeyEnv, cfg.SecretKeyEnv, cfg.PassphraseEnv, cfg.ProjectIDEnv)
	}
	client, err := ethclient.DialContext(ctx, bsc.RPCURL)
	if err != nil {
		return nil, err
	}
	chainID, err := client.ChainID(ctx)
	if err != nil {
		client.Close()
		return nil, err
	}
	if chainID.Int64() != bsc.ChainID {
		client.Close()
		return nil, fmt.Errorf("BSC RPC chain ID is %s, expected %d", chainID, bsc.ChainID)
	}
	ea, err := abi.JSON(strings.NewReader(erc20ABIJSON))
	if err != nil {
		client.Close()
		return nil, err
	}
	p := &OKXDEXClient{bsc: bsc, cfg: cfg, http: &http.Client{Timeout: time.Duration(cfg.RequestTimeoutSeconds) * time.Second}, client: client, erc20ABI: ea, wallet: common.HexToAddress(bsc.WalletAddress), apiKey: apiKey, secret: secret, passphrase: passphrase, projectID: projectID}
	if privateKeyHex != "" {
		key, err := crypto.HexToECDSA(strings.TrimPrefix(privateKeyHex, "0x"))
		if err != nil {
			client.Close()
			return nil, fmt.Errorf("invalid BSC private key: %w", err)
		}
		if derived := crypto.PubkeyToAddress(key.PublicKey); derived != p.wallet {
			client.Close()
			return nil, fmt.Errorf("BSC private key does not match configured wallet")
		}
		p.key = key
	}
	return p, nil
}

func (p *OKXDEXClient) Close() {
	if p.client != nil {
		p.client.Close()
	}
}

func units(v float64, decimals uint8) *big.Int {
	f := new(big.Float).SetPrec(256).Mul(big.NewFloat(v), new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)))
	out, _ := f.Int(nil)
	return out
}

func decimal(v *big.Int, decimals uint8) float64 {
	if v == nil {
		return 0
	}
	f := new(big.Float).SetInt(v)
	f.Quo(f, new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)))
	out, _ := f.Float64()
	return out
}

func parseBigDecimal(s string) (*big.Int, error) {
	if s == "" {
		return big.NewInt(0), nil
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.Sign() < 0 {
		return nil, fmt.Errorf("invalid decimal integer %q", s)
	}
	return v, nil
}

func okxSignature(secret, timestamp, method, requestPath string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + method + requestPath))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (p *OKXDEXClient) waitForRequestSlot(ctx context.Context) error {
	const minimumInterval = 600 * time.Millisecond
	p.requestMu.Lock()
	now := time.Now()
	slot := now
	if p.nextRequest.After(now) {
		slot = p.nextRequest
	}
	p.nextRequest = slot.Add(minimumInterval)
	p.requestMu.Unlock()
	wait := time.Until(slot)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *OKXDEXClient) coolDownRequests(delay time.Duration) {
	p.requestMu.Lock()
	defer p.requestMu.Unlock()
	until := time.Now().Add(delay)
	if until.After(p.nextRequest) {
		p.nextRequest = until
	}
}

func (p *OKXDEXClient) get(ctx context.Context, path string, q url.Values, out any) error {
	query := q.Encode()
	requestPath := path
	if query != "" {
		requestPath += "?" + query
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := p.waitForRequestSlot(ctx); err != nil {
			return err
		}
		timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.cfg.BaseURL, "/")+requestPath, nil)
		if err != nil {
			return err
		}
		req.Header.Set("OK-ACCESS-KEY", p.apiKey)
		req.Header.Set("OK-ACCESS-SIGN", okxSignature(p.secret, timestamp, http.MethodGet, requestPath))
		req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
		req.Header.Set("OK-ACCESS-PASSPHRASE", p.passphrase)
		req.Header.Set("OK-ACCESS-PROJECT", p.projectID)
		resp, err := p.http.Do(req)
		if err != nil {
			return err
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 2 {
			delay := time.Duration(2<<attempt) * time.Second
			p.coolDownRequests(delay)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("OKX DEX GET %s HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("OKX DEX %s decode: %w", path, err)
		}
		return nil
	}
	return fmt.Errorf("OKX DEX GET %s exhausted retries", path)
}

func (p *OKXDEXClient) validateRoute(r okxQuote, tokenIn, tokenOut string, amountIn *big.Int) (*big.Int, error) {
	if r.ChainIndex != strconv.FormatInt(p.bsc.ChainID, 10) ||
		!strings.EqualFold(r.FromToken.TokenContractAddress, tokenIn) ||
		!strings.EqualFold(r.ToToken.TokenContractAddress, tokenOut) {
		return nil, fmt.Errorf("OKX DEX quote token or chain mismatch")
	}
	input, err := parseBigDecimal(r.FromTokenAmount)
	if err != nil || input.Cmp(amountIn) != 0 {
		return nil, fmt.Errorf("OKX DEX input amount mismatch")
	}
	if r.FromToken.IsHoneyPot || r.ToToken.IsHoneyPot {
		return nil, fmt.Errorf("OKX DEX marked a token as honeypot")
	}
	fromTax, fromTaxErr := strconv.ParseFloat(r.FromToken.TaxRate, 64)
	toTax, toTaxErr := strconv.ParseFloat(r.ToToken.TaxRate, 64)
	if fromTaxErr != nil || toTaxErr != nil || fromTax != 0 || toTax != 0 {
		return nil, fmt.Errorf("OKX DEX reports token tax or invalid tax data")
	}
	out, err := parseBigDecimal(r.ToTokenAmount)
	if err != nil || out.Sign() <= 0 {
		return nil, fmt.Errorf("OKX DEX invalid output amount")
	}
	return out, nil
}

func (p *OKXDEXClient) quote(ctx context.Context, tokenIn, tokenOut string, amountIn *big.Int) (okxQuote, *big.Int, error) {
	q := url.Values{"chainIndex": {strconv.FormatInt(p.bsc.ChainID, 10)}, "amount": {amountIn.String()}, "fromTokenAddress": {tokenIn}, "toTokenAddress": {tokenOut}, "swapMode": {"exactIn"}}
	if p.cfg.DisableRFQ {
		q.Set("disableRFQ", "true")
	}
	var env okxEnvelope[okxQuote]
	if err := p.get(ctx, "/api/v6/dex/aggregator/quote", q, &env); err != nil {
		return okxQuote{}, nil, err
	}
	if env.Code != "0" || len(env.Data) != 1 {
		return okxQuote{}, nil, fmt.Errorf("OKX DEX quote code=%s msg=%s records=%d", env.Code, env.Msg, len(env.Data))
	}
	r := env.Data[0]
	out, err := p.validateRoute(r, tokenIn, tokenOut, amountIn)
	if err != nil {
		return okxQuote{}, nil, err
	}
	return r, out, nil
}

func (p *OKXDEXClient) TokenCatalog(ctx context.Context) ([]OKXCatalogToken, error) {
	q := url.Values{"chainIndex": {strconv.FormatInt(p.bsc.ChainID, 10)}}
	var env okxEnvelope[OKXCatalogToken]
	if err := p.get(ctx, "/api/v6/dex/aggregator/all-tokens", q, &env); err != nil {
		return nil, err
	}
	if env.Code != "0" || len(env.Data) == 0 {
		return nil, fmt.Errorf("OKX DEX token catalog code=%s msg=%s records=%d", env.Code, env.Msg, len(env.Data))
	}
	return env.Data, nil
}

func exactSearchTokens(rows []okxTokenSearchResult, chainID int64, symbol string) []OKXCatalogToken {
	wantChain := strconv.FormatInt(chainID, 10)
	wantSymbol := strings.ToUpper(strings.TrimSpace(symbol))
	byAddress := map[string]OKXCatalogToken{}
	for _, row := range rows {
		address := strings.ToLower(strings.TrimSpace(row.TokenContractAddress))
		if row.ChainIndex != wantChain || strings.ToUpper(strings.TrimSpace(row.TokenSymbol)) != wantSymbol || !addressPattern.MatchString(address) || strings.EqualFold(address, "0x0000000000000000000000000000000000000000") {
			continue
		}
		decimals, err := strconv.ParseUint(row.Decimal, 10, 8)
		if err != nil || decimals > 36 {
			continue
		}
		byAddress[address] = OKXCatalogToken{
			Decimals:             row.Decimal,
			TokenContractAddress: row.TokenContractAddress,
			TokenName:            row.TokenName,
			TokenSymbol:          row.TokenSymbol,
			Price:                row.Price,
			Liquidity:            row.Liquidity,
			Holders:              row.Holders,
			CommunityRecognized:  row.TagList.CommunityRecognized,
		}
	}
	out := make([]OKXCatalogToken, 0, len(byAddress))
	for _, token := range byAddress {
		out = append(out, token)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].TokenContractAddress) < strings.ToLower(out[j].TokenContractAddress)
	})
	return out
}

const maxTokenMatchPriceDeviationPercent = 25.0

// selectUniqueSearchToken resolves the common OKX Wallet case where the UI
// presents one useful token but the API also returns exact-symbol copy tokens.
// A duplicate is accepted only when Binance's mark price and OKX's on-chain
// market metadata identify exactly one credible BSC contract.
func selectUniqueSearchToken(tokens []OKXCatalogToken, markPrice, minLiquidity float64) (OKXCatalogToken, string, bool) {
	if len(tokens) == 1 {
		return tokens[0], "unique_exact_symbol", true
	}
	if len(tokens) == 0 || markPrice <= 0 {
		return OKXCatalogToken{}, "no_exact_market_match", false
	}
	type candidate struct {
		token OKXCatalogToken
	}
	credible := make([]candidate, 0, len(tokens))
	for _, token := range tokens {
		price, priceErr := strconv.ParseFloat(strings.TrimSpace(token.Price), 64)
		liquidity, liquidityErr := strconv.ParseFloat(strings.TrimSpace(token.Liquidity), 64)
		if priceErr != nil || price <= 0 {
			continue
		}
		deviation := math.Abs(price/markPrice-1) * 100
		liquidEnough := liquidityErr == nil && liquidity >= minLiquidity
		if deviation <= maxTokenMatchPriceDeviationPercent && (token.CommunityRecognized || liquidEnough) {
			credible = append(credible, candidate{token: token})
		}
	}
	if len(credible) == 1 {
		return credible[0].token, "unique_price_liquidity_match", true
	}
	if len(credible) > 1 {
		var recognized []candidate
		for _, item := range credible {
			if item.token.CommunityRecognized {
				recognized = append(recognized, item)
			}
		}
		if len(recognized) == 1 {
			return recognized[0].token, "unique_recognized_market_match", true
		}
	}
	return OKXCatalogToken{}, "ambiguous_exact_symbol", false
}

// SearchTokens mirrors the token search used by the OKX Wallet UI. Unlike
// all-tokens, this endpoint can return tokens outside OKX's curated major-token
// catalog. Only exact symbol matches on the configured chain are returned.
func (p *OKXDEXClient) SearchTokens(ctx context.Context, symbol string) ([]OKXCatalogToken, error) {
	q := url.Values{
		"chains": {strconv.FormatInt(p.bsc.ChainID, 10)},
		"search": {strings.ToUpper(strings.TrimSpace(symbol))},
		"limit":  {"100"},
	}
	var env okxEnvelope[okxTokenSearchResult]
	if err := p.get(ctx, "/api/v6/dex/market/token/search", q, &env); err != nil {
		return nil, err
	}
	if env.Code != "0" {
		return nil, fmt.Errorf("OKX token search code=%s msg=%s", env.Code, env.Msg)
	}
	return exactSearchTokens(env.Data, p.bsc.ChainID, symbol), nil
}

func (p *OKXDEXClient) SearchTokenAddress(ctx context.Context, address string) ([]OKXCatalogToken, error) {
	address = strings.TrimSpace(address)
	if !addressPattern.MatchString(address) {
		return nil, fmt.Errorf("invalid BSC token address %q", address)
	}
	q := url.Values{
		"chains": {strconv.FormatInt(p.bsc.ChainID, 10)},
		"search": {address},
		"limit":  {"100"},
	}
	var env okxEnvelope[okxTokenSearchResult]
	if err := p.get(ctx, "/api/v6/dex/market/token/search", q, &env); err != nil {
		return nil, err
	}
	if env.Code != "0" {
		return nil, fmt.Errorf("OKX token address search code=%s msg=%s", env.Code, env.Msg)
	}
	byAddress := map[string]OKXCatalogToken{}
	for _, row := range env.Data {
		rowAddress := strings.ToLower(strings.TrimSpace(row.TokenContractAddress))
		decimals, err := strconv.ParseUint(row.Decimal, 10, 8)
		if row.ChainIndex != strconv.FormatInt(p.bsc.ChainID, 10) || !strings.EqualFold(rowAddress, address) || err != nil || decimals > 36 {
			continue
		}
		byAddress[rowAddress] = OKXCatalogToken{
			Decimals:             row.Decimal,
			TokenContractAddress: row.TokenContractAddress,
			TokenName:            row.TokenName,
			TokenSymbol:          row.TokenSymbol,
			Price:                row.Price,
			Liquidity:            row.Liquidity,
			Holders:              row.Holders,
			CommunityRecognized:  row.TagList.CommunityRecognized,
		}
	}
	out := make([]OKXCatalogToken, 0, len(byAddress))
	for _, token := range byAddress {
		out = append(out, token)
	}
	return out, nil
}

func quoteDEXes(r okxQuote) []string {
	seen := map[string]bool{}
	var out []string
	for _, route := range r.DexRouterList {
		name := strings.TrimSpace(route.DexProtocol.DexName)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (p *OKXDEXClient) BestQuote(ctx context.Context, t TokenConfig, notional, reference float64) (ChainQuote, error) {
	if notional <= 0 || reference <= 0 {
		return ChainQuote{}, fmt.Errorf("quote amounts must be positive")
	}
	buy, buyOut, err := p.quote(ctx, p.bsc.USDTAddress, t.BSCAddress, units(notional, p.bsc.USDTDecimals))
	if err != nil {
		return ChainQuote{}, err
	}
	tokens := decimal(buyOut, t.Decimals)
	if tokens <= 0 {
		return ChainQuote{}, fmt.Errorf("OKX DEX returned zero %s amount", t.Symbol)
	}
	_, sellOut, err := p.quote(ctx, t.BSCAddress, p.bsc.USDTAddress, buyOut)
	if err != nil {
		return ChainQuote{}, err
	}
	sellUSDT := decimal(sellOut, p.bsc.USDTDecimals)
	buyPrice := notional / tokens
	gas, _ := strconv.ParseUint(buy.EstimateGasFee, 10, 64)
	okxImpact, _ := strconv.ParseFloat(buy.PriceImpact, 64)
	return ChainQuote{Symbol: t.Symbol, Route: buy.Router, DEXes: quoteDEXes(buy), InputUSDT: notional, OutputTokens: tokens, BuyPrice: buyPrice, SellPrice: sellUSDT / tokens, RoundTripLossBPS: math.Max(0, (notional-sellUSDT)/notional*10000), DepthImpactBPS: math.Abs(okxImpact) * 100, GasEstimate: gas, OKXPriceImpactPercent: okxImpact}, nil
}

func (p *OKXDEXClient) positionExitQuote(ctx context.Context, t TokenConfig, tokenQty, referencePrice float64) (PositionExitQuote, error) {
	if tokenQty <= 0 {
		return PositionExitQuote{}, fmt.Errorf("sell quantity must be positive")
	}
	if referencePrice <= 0 {
		return PositionExitQuote{}, fmt.Errorf("sell reference price must be positive")
	}
	quote, out, err := p.quote(ctx, t.BSCAddress, p.bsc.USDTAddress, units(tokenQty, t.Decimals))
	if err != nil {
		return PositionExitQuote{}, err
	}
	value := decimal(out, p.bsc.USDTDecimals)
	if value <= 0 {
		return PositionExitQuote{}, fmt.Errorf("OKX DEX sell quote for %s returned zero", t.Symbol)
	}
	impactPercent, err := strconv.ParseFloat(strings.TrimSpace(quote.PriceImpact), 64)
	if err != nil || math.IsNaN(impactPercent) || math.IsInf(impactPercent, 0) {
		return PositionExitQuote{}, fmt.Errorf("OKX DEX sell quote for %s returned invalid price impact %q", t.Symbol, quote.PriceImpact)
	}
	return PositionExitQuote{
		TokenQty:       tokenQty,
		ReferenceUSDT:  tokenQty * referencePrice,
		OutputUSDT:     value,
		ExecutionPrice: value / tokenQty,
		PriceImpactBPS: math.Abs(impactPercent) * 100,
	}, nil
}

func (p *OKXDEXClient) SellQuote(ctx context.Context, t TokenConfig, tokenQty float64) (float64, error) {
	if tokenQty <= 0 {
		return 0, fmt.Errorf("sell quantity must be positive")
	}
	_, out, err := p.quote(ctx, t.BSCAddress, p.bsc.USDTAddress, units(tokenQty, t.Decimals))
	if err != nil {
		return 0, err
	}
	value := decimal(out, p.bsc.USDTDecimals)
	if value <= 0 {
		return 0, fmt.Errorf("OKX DEX sell quote for %s returned zero", t.Symbol)
	}
	return value, nil
}

// PositionExitCapacity probes the actual held token quantity in the executable
// token-to-USDT direction. It intentionally does not reuse entry depth: failure
// of a target or 3x target buy quote must only block entries, never force-close
// a smaller existing position.
func (p *OKXDEXClient) PositionExitCapacity(ctx context.Context, t TokenConfig, tokenQty, referencePrice, maxImpactBPS float64) (float64, PositionExitQuote, error) {
	if tokenQty <= 0 || referencePrice <= 0 || maxImpactBPS <= 0 {
		return 0, PositionExitQuote{}, fmt.Errorf("position exit capacity inputs must be positive")
	}
	currentNotional := tokenQty * referencePrice
	minProbeNotional := math.Min(currentNotional, 1)
	fractions := []float64{1, .75, .5, .25, .125, .0625}
	var last PositionExitQuote
	for _, fraction := range fractions {
		probeNotional := currentNotional * fraction
		if probeNotional+1e-9 < minProbeNotional {
			continue
		}
		quote, err := p.positionExitQuote(ctx, t, tokenQty*fraction, referencePrice)
		if err != nil {
			return 0, PositionExitQuote{}, err
		}
		last = quote
		if quote.PriceImpactBPS <= maxImpactBPS {
			return probeNotional, quote, nil
		}
	}
	return 0, last, nil
}

func (p *OKXDEXClient) DepthCapacity(ctx context.Context, t TokenConfig, c Config) (float64, ChainQuote, error) {
	ceiling := math.Min(c.Risk.MaxNotionalPerCoinUSDT, c.Risk.TotalCapitalUSDT*c.Risk.MaxCapitalPerCoinPercent/100) * c.Risk.DepthSafetyMultiplier
	if t.MaxNotionalUSDT > 0 {
		ceiling = math.Min(ceiling, t.MaxNotionalUSDT*c.Risk.DepthSafetyMultiplier)
	}
	required := math.Min(ceiling, c.Risk.TargetNotionalPerCoinUSDT*c.Risk.DepthSafetyMultiplier)
	probes := []float64{required}
	if required > c.Risk.TargetNotionalPerCoinUSDT+1e-9 {
		probes = append(probes, c.Risk.TargetNotionalPerCoinUSDT)
	}
	var last ChainQuote
	for _, probe := range probes {
		q, err := p.BestQuote(ctx, t, probe, c.Risk.ReferenceQuoteUSDT)
		if err != nil {
			return 0, ChainQuote{}, err
		}
		last = q
		if q.DepthImpactBPS <= c.Risk.MaxChainPriceImpactBPS && math.Abs(q.OKXPriceImpactPercent)*100 <= c.Risk.MaxChainPriceImpactBPS {
			return probe, q, nil
		}
	}
	return 0, last, nil
}

func (p *OKXDEXClient) tokenContract(address string) *bind.BoundContract {
	return bind.NewBoundContract(common.HexToAddress(address), p.erc20ABI, p.client, p.client, p.client)
}

func (p *OKXDEXClient) Balance(ctx context.Context, token string, decimals uint8) (float64, error) {
	var result []any
	if err := p.tokenContract(token).Call(&bind.CallOpts{Context: ctx}, &result, "balanceOf", p.wallet); err != nil {
		return 0, err
	}
	v, ok := result[0].(*big.Int)
	if !ok {
		return 0, fmt.Errorf("balanceOf returned %T", result[0])
	}
	return decimal(v, decimals), nil
}

func (p *OKXDEXClient) approval(ctx context.Context, token string, amount *big.Int) (okxApproval, error) {
	q := url.Values{"chainIndex": {strconv.FormatInt(p.bsc.ChainID, 10)}, "tokenContractAddress": {token}, "approveAmount": {amount.String()}}
	var env okxEnvelope[okxApproval]
	if err := p.get(ctx, "/api/v6/dex/aggregator/approve-transaction", q, &env); err != nil {
		return okxApproval{}, err
	}
	if env.Code != "0" || len(env.Data) != 1 {
		return okxApproval{}, fmt.Errorf("OKX DEX approval code=%s msg=%s records=%d", env.Code, env.Msg, len(env.Data))
	}
	return env.Data[0], nil
}

func validateApprovalData(encoded string, spender common.Address, amount *big.Int) ([]byte, error) {
	data, err := hexutil.Decode(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode approval calldata: %w", err)
	}
	if len(data) != 68 || !bytes.Equal(data[:4], []byte{0x09, 0x5e, 0xa7, 0xb3}) {
		return nil, fmt.Errorf("approval calldata is not approve(address,uint256)")
	}
	if common.BytesToAddress(data[16:36]) != spender {
		return nil, fmt.Errorf("approval calldata spender mismatch")
	}
	if new(big.Int).SetBytes(data[36:68]).Cmp(amount) < 0 {
		return nil, fmt.Errorf("approval calldata amount is too small")
	}
	return data, nil
}

func (p *OKXDEXClient) ensureAllowance(ctx context.Context, token string, amount *big.Int) error {
	a, err := p.approval(ctx, token, amount)
	if err != nil {
		return err
	}
	if !common.IsHexAddress(a.DexContractAddress) {
		return fmt.Errorf("OKX DEX returned invalid approval spender")
	}
	spender := common.HexToAddress(a.DexContractAddress)
	var result []any
	if err := p.tokenContract(token).Call(&bind.CallOpts{Context: ctx}, &result, "allowance", p.wallet, spender); err != nil {
		return err
	}
	allowance, ok := result[0].(*big.Int)
	if !ok {
		return fmt.Errorf("allowance returned %T", result[0])
	}
	if allowance.Cmp(amount) >= 0 {
		return nil
	}
	data, err := validateApprovalData(a.Data, spender, amount)
	if err != nil {
		return fmt.Errorf("OKX DEX approval calldata validation failed: %w", err)
	}
	gas, _ := strconv.ParseUint(a.GasLimit, 10, 64)
	gasPrice, err := parseBigDecimal(a.GasPrice)
	if err != nil {
		return err
	}
	_, err = p.sendTransaction(ctx, common.HexToAddress(token), big.NewInt(0), data, gas, gasPrice)
	return err
}

func (p *OKXDEXClient) swapData(ctx context.Context, tokenIn, tokenOut string, amount *big.Int) (okxSwap, error) {
	q := url.Values{"chainIndex": {strconv.FormatInt(p.bsc.ChainID, 10)}, "amount": {amount.String()}, "fromTokenAddress": {tokenIn}, "toTokenAddress": {tokenOut}, "slippagePercent": {strconv.FormatFloat(p.bsc.SlippageBPS/100, 'f', -1, 64)}, "userWalletAddress": {p.wallet.Hex()}, "swapMode": {"exactIn"}}
	if p.cfg.DisableRFQ {
		q.Set("disableRFQ", "true")
	}
	var env okxEnvelope[okxSwap]
	if err := p.get(ctx, "/api/v6/dex/aggregator/swap", q, &env); err != nil {
		return okxSwap{}, err
	}
	if env.Code != "0" || len(env.Data) != 1 {
		return okxSwap{}, fmt.Errorf("OKX DEX swap code=%s msg=%s records=%d", env.Code, env.Msg, len(env.Data))
	}
	s := env.Data[0]
	if !strings.EqualFold(s.Tx.From, p.wallet.Hex()) || !common.IsHexAddress(s.Tx.To) || common.HexToAddress(s.Tx.To) == (common.Address{}) || !strings.EqualFold(s.RouterResult.FromToken.TokenContractAddress, tokenIn) || !strings.EqualFold(s.RouterResult.ToToken.TokenContractAddress, tokenOut) {
		return okxSwap{}, fmt.Errorf("OKX DEX swap response validation failed")
	}
	if _, err := p.validateRoute(s.RouterResult, tokenIn, tokenOut, amount); err != nil {
		return okxSwap{}, fmt.Errorf("OKX DEX swap route validation failed: %w", err)
	}
	return s, nil
}

func (p *OKXDEXClient) SwapExactInput(ctx context.Context, tokenIn, tokenOut string, inDecimals, outDecimals uint8, amountInFloat, quotedOutFloat float64) (float64, common.Hash, error) {
	return p.swapExactInput(ctx, tokenIn, tokenOut, inDecimals, outDecimals, amountInFloat, quotedOutFloat, nil)
}

func (p *OKXDEXClient) SwapExactInputTracked(ctx context.Context, tokenIn, tokenOut string, inDecimals, outDecimals uint8, amountInFloat, quotedOutFloat float64, onBroadcast func(common.Hash) error) (float64, common.Hash, error) {
	return p.swapExactInput(ctx, tokenIn, tokenOut, inDecimals, outDecimals, amountInFloat, quotedOutFloat, onBroadcast)
}

func (p *OKXDEXClient) swapExactInput(ctx context.Context, tokenIn, tokenOut string, inDecimals, outDecimals uint8, amountInFloat, quotedOutFloat float64, onBroadcast func(common.Hash) error) (float64, common.Hash, error) {
	if p.key == nil {
		return 0, common.Hash{}, fmt.Errorf("BSC private key is missing")
	}
	amount := units(amountInFloat, inDecimals)
	if err := p.ensureAllowance(ctx, tokenIn, amount); err != nil {
		return 0, common.Hash{}, err
	}
	s, err := p.swapData(ctx, tokenIn, tokenOut, amount)
	if err != nil {
		return 0, common.Hash{}, err
	}
	out, err := parseBigDecimal(s.RouterResult.ToTokenAmount)
	if err != nil || out.Sign() <= 0 {
		return 0, common.Hash{}, fmt.Errorf("invalid OKX DEX output amount")
	}
	expected := decimal(out, outDecimals)
	if expected < quotedOutFloat*(1-p.bsc.SlippageBPS/10000) {
		return 0, common.Hash{}, fmt.Errorf("fresh OKX DEX swap quote deteriorated beyond slippage limit")
	}
	minOut, err := parseBigDecimal(s.Tx.MinReceiveAmount)
	if err != nil || minOut.Sign() <= 0 || minOut.Cmp(out) > 0 {
		return 0, common.Hash{}, fmt.Errorf("invalid OKX DEX minimum receive amount")
	}
	minimumAllowed := new(big.Int).Mul(out, big.NewInt(int64(10000-p.bsc.SlippageBPS)))
	minimumAllowed.Div(minimumAllowed, big.NewInt(10000))
	if minOut.Cmp(minimumAllowed) < 0 {
		return 0, common.Hash{}, fmt.Errorf("OKX DEX minimum receive amount exceeds configured slippage")
	}
	data, err := hexutil.Decode(s.Tx.Data)
	if err != nil || len(data) < 4 {
		return 0, common.Hash{}, fmt.Errorf("invalid OKX DEX transaction data")
	}
	value, err := parseBigDecimal(s.Tx.Value)
	if err != nil || value.Sign() != 0 {
		return 0, common.Hash{}, fmt.Errorf("OKX DEX ERC-20 swap returned unexpected native value")
	}
	gas, err := strconv.ParseUint(s.Tx.Gas, 10, 64)
	if err != nil || gas == 0 {
		return 0, common.Hash{}, fmt.Errorf("invalid OKX DEX gas")
	}
	gasPrice, err := parseBigDecimal(s.Tx.GasPrice)
	if err != nil {
		return 0, common.Hash{}, err
	}
	receipt, txHash, err := p.sendTransactionTracked(ctx, common.HexToAddress(s.Tx.To), value, data, gas, gasPrice, onBroadcast)
	if err != nil {
		return expected, txHash, err
	}
	return expected, receipt.TxHash, nil
}

func (p *OKXDEXClient) sendTransaction(ctx context.Context, to common.Address, value *big.Int, data []byte, gas uint64, gasPrice *big.Int) (*types.Receipt, error) {
	receipt, _, err := p.sendTransactionTracked(ctx, to, value, data, gas, gasPrice, nil)
	return receipt, err
}

func (p *OKXDEXClient) sendTransactionTracked(ctx context.Context, to common.Address, value *big.Int, data []byte, gas uint64, gasPrice *big.Int, onBroadcast func(common.Hash) error) (*types.Receipt, common.Hash, error) {
	if p.key == nil {
		return nil, common.Hash{}, fmt.Errorf("BSC private key is missing")
	}
	if gas == 0 {
		estimated, err := p.client.EstimateGas(ctx, ethereum.CallMsg{From: p.wallet, To: &to, Value: value, Data: data})
		if err != nil {
			return nil, common.Hash{}, err
		}
		gas = estimated
	}
	if gas > ^uint64(0)/3*2 {
		return nil, common.Hash{}, fmt.Errorf("BSC gas limit overflow")
	}
	gas = gas + gas/2
	if gas > p.bsc.MaxGasLimit {
		return nil, common.Hash{}, fmt.Errorf("BSC gas limit %d exceeds configured maximum %d", gas, p.bsc.MaxGasLimit)
	}
	if gasPrice == nil || gasPrice.Sign() <= 0 {
		var err error
		gasPrice, err = p.client.SuggestGasPrice(ctx)
		if err != nil {
			return nil, common.Hash{}, err
		}
	}
	if gasPrice.Cmp(units(p.bsc.MaxGasPriceGwei, 9)) > 0 {
		return nil, common.Hash{}, fmt.Errorf("BSC gas price exceeds configured maximum %.4g gwei", p.bsc.MaxGasPriceGwei)
	}
	nonce, err := p.client.PendingNonceAt(ctx, p.wallet)
	if err != nil {
		return nil, common.Hash{}, err
	}
	tx := types.NewTransaction(nonce, to, value, gas, gasPrice, data)
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(p.bsc.ChainID)), p.key)
	if err != nil {
		return nil, common.Hash{}, err
	}
	if err := p.client.SendTransaction(ctx, signed); err != nil {
		return nil, common.Hash{}, err
	}
	txHash := signed.Hash()
	if onBroadcast != nil {
		if err := onBroadcast(txHash); err != nil {
			return nil, txHash, fmt.Errorf("BSC transaction %s broadcast but journal persistence failed: %w", txHash, err)
		}
	}
	receipt, err := bind.WaitMined(ctx, p.client, signed)
	if err != nil {
		return nil, txHash, err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return nil, txHash, fmt.Errorf("BSC transaction reverted: %s", signed.Hash())
	}
	if p.bsc.Confirmations <= 1 {
		return receipt, txHash, nil
	}
	target := receipt.BlockNumber.Uint64() + p.bsc.Confirmations - 1
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return receipt, txHash, ctx.Err()
		case <-ticker.C:
			h, e := p.client.HeaderByNumber(ctx, nil)
			if e == nil && h.Number.Uint64() >= target {
				return receipt, txHash, nil
			}
		}
	}
}

func (p *OKXDEXClient) TransactionStatus(ctx context.Context, hash common.Hash) (string, error) {
	receipt, err := p.client.TransactionReceipt(ctx, hash)
	if errors.Is(err, ethereum.NotFound) {
		return "pending", nil
	}
	if err != nil {
		return "", err
	}
	if receipt.Status == types.ReceiptStatusSuccessful {
		return "success", nil
	}
	return "reverted", nil
}
