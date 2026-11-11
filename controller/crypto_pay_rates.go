package controller

// USD rates and quote math for the crypto payment rail
// (mmm/ZCASH-CONTRACT.md §3.3, mmm/ZCASH.md §4.5, decision 11, mmm/BTCPAY.md
// §17.4).
//
// A coin is quoted only from at least two fresh answers that agree: each
// configured source (Kraken and Coinbase for every coin they list; Monero,
// which Coinbase does not list, Kraken and CoinGecko) is asked at most once a
// minute per process, an answer older than two minutes is stale, and the
// quote is the median of the fresh answers when their spread is within 3%.
// Anything less refuses the quote (asset_unavailable): a wrong rate is a
// mispriced sale, and a refused quote is only "temporarily unavailable".
//
// CoinGecko is dropped for every coin but Monero (its keyless terms exclude
// production use). Monero keeps it until the owner names a second source:
// CoinPaprika's keyless tier, the planned one, is licensed for personal use
// only (cryptoRateSourceCoingecko).
//
// The requests carry no user data: they are the same public ticker calls
// whoever is buying, made from the server.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A rate source's request is abandoned after this long.
const cryptoRateHttpTimeout = 5 * time.Second

// After a failed request a source is not asked again for this long, so a
// source that is down costs one request per interval, not one per quote.
const cryptoRateFailureBackoff = 10 * time.Second

// Why a quote was refused (the metric's reason label).
const (
	cryptoQuoteRefusedNoSources  = "no_sources"
	cryptoQuoteRefusedTooFew     = "too_few_fresh_sources"
	cryptoQuoteRefusedDivergent  = "divergent"
	cryptoQuoteRefusedLagging    = "scanner_lagging"
	cryptoQuoteRefusedPoolEmpty  = "address_pool_empty"
	cryptoQuoteRefusedTooMany    = "too_many_open_invoices"
	cryptoQuoteRefusedBadAmount  = "bad_amount"
	cryptoQuoteRefusedDatabase   = "database"
	cryptoQuoteRefusedDisabled   = "disabled"
	cryptoQuoteRefusedOverMax    = "over_max_invoice"
	cryptoQuoteRefusedSolanaFail = "solana_intent"
	// the wallet's height or the network's is not known: no node configured,
	// or a node that answered none
	cryptoQuoteRefusedNoHeight = "network_height_unknown"
	// too many open code-delivery invoices of the coin
	cryptoQuoteRefusedTooManyCodes = "too_many_open_codes"
	// the coin does not deliver balance codes (code_delivery: false)
	cryptoQuoteRefusedNoCodes = "code_delivery_off"
	// the coin is switched on but the vault does not name everything selling
	// it needs (its chain source, or for a Coinbase coin the Coinbase key)
	cryptoQuoteRefusedNotConfigured = "not_configured"
)

// cryptoRateAnswer is one source's rate for one coin, kept with the quote.
type cryptoRateAnswer struct {
	Source  string  `json:"source"`
	RateUsd float64 `json:"rate_usd"`
	// when this process got the answer (what staleness is measured from)
	FetchedAt time.Time `json:"fetched_at"`
	// the source's own time for the rate, when it gives one
	SourceTime *time.Time `json:"source_time,omitempty"`
}

// asOf is when the answer's rate held: the source's own time when it gives an
// older one (CoinGecko's last_updated_at; a frozen price must read stale),
// else when this process got it.
func (self *cryptoRateAnswer) asOf() time.Time {
	if self.SourceTime != nil && self.SourceTime.Before(self.FetchedAt) {
		return *self.SourceTime
	}
	return self.FetchedAt
}

// cryptoRateQuote is the rate a quote is made at.
type cryptoRateQuote struct {
	RateUsd       float64
	Answers       []*cryptoRateAnswer
	SpreadPercent float64
}

// A source's fetch: the coin's USD rate and, when the source says, its time.
type cryptoRateFetch func(ctx context.Context, asset string) (rateUsd float64, sourceTime *time.Time, err error)

// The sources by config name. Replaceable only by tests, which answer without
// the network; production never mutates them.
var cryptoRateFetchers = map[string]cryptoRateFetch{
	cryptoRateSourceKraken:    krakenFetchRate,
	cryptoRateSourceCoingecko: coingeckoFetchRate,
	cryptoRateSourceCoinbase:  coinbaseFetchRate,
}

// The clock the rate cache reads. Replaceable only by tests.
var cryptoRateNow = server.NowUtc

// cryptoRateCacheEntry is a source's last good answer for a coin and when it
// was last asked.
type cryptoRateCacheEntry struct {
	// held while a request is in flight, so concurrent quotes wait for one
	// request instead of each making their own
	fetchLock     sync.Mutex
	stateLock     sync.Mutex
	answer        *cryptoRateAnswer
	lastAttemptAt time.Time
	lastFailedAt  time.Time
}

type cryptoRateCache struct {
	stateLock sync.Mutex
	entries   map[string]*cryptoRateCacheEntry
}

var cryptoRates = &cryptoRateCache{entries: map[string]*cryptoRateCacheEntry{}}

func (self *cryptoRateCache) entry(source string, asset string) *cryptoRateCacheEntry {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	key := source + "/" + asset
	entry, ok := self.entries[key]
	if !ok {
		entry = &cryptoRateCacheEntry{}
		self.entries[key] = entry
	}
	return entry
}

// reset forgets every cached answer (tests).
func (self *cryptoRateCache) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.entries = map[string]*cryptoRateCacheEntry{}
}

// answer is the source's answer for the coin: the cached one while it is
// younger than the cache interval, else a new request. A failed request keeps
// the previous answer, which the caller judges by its age.
func (self *cryptoRateCache) answer(ctx context.Context, source string, asset string, cacheFor time.Duration) *cryptoRateAnswer {
	fetch, ok := cryptoRateFetchers[source]
	if !ok {
		return nil
	}
	entry := self.entry(source, asset)

	current := func() (answer *cryptoRateAnswer, fresh bool, backingOff bool) {
		entry.stateLock.Lock()
		defer entry.stateLock.Unlock()
		now := cryptoRateNow()
		answer = entry.answer
		fresh = answer != nil && now.Sub(answer.FetchedAt) < cacheFor
		backingOff = !entry.lastFailedAt.IsZero() && now.Sub(entry.lastFailedAt) < cryptoRateFailureBackoff
		return
	}
	if answer, fresh, backingOff := current(); fresh || backingOff {
		return answer
	}

	entry.fetchLock.Lock()
	defer entry.fetchLock.Unlock()
	// another quote may have refreshed it while this one waited
	if answer, fresh, backingOff := current(); fresh || backingOff {
		return answer
	}

	fetchCtx, cancel := context.WithTimeout(ctx, cryptoRateHttpTimeout)
	defer cancel()
	rateUsd, sourceTime, err := func() (rateUsd float64, sourceTime *time.Time, err error) {
		// a source's client must never take a quote down with it
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("rate source panicked: %v", r)
			}
		}()
		return fetch(fetchCtx, asset)
	}()
	now := cryptoRateNow()

	entry.stateLock.Lock()
	defer entry.stateLock.Unlock()
	entry.lastAttemptAt = now
	if err == nil && !(0 < rateUsd && !math.IsInf(rateUsd, 0) && !math.IsNaN(rateUsd)) {
		err = fmt.Errorf("unusable rate %v", rateUsd)
	}
	if err != nil {
		entry.lastFailedAt = now
		cryptoPayRateSourceResults.WithLabelValues(asset, source, "failure").Inc()
		if glog.V(1) {
			glog.Infof("[cryptopay]rate source %s for %s failed: %s\n", source, asset, err)
		}
		return entry.answer
	}
	entry.lastFailedAt = time.Time{}
	entry.answer = &cryptoRateAnswer{
		Source:     source,
		RateUsd:    rateUsd,
		FetchedAt:  now,
		SourceTime: sourceTime,
	}
	cryptoPayRateSourceResults.WithLabelValues(asset, source, "answer").Inc()
	return entry.answer
}

// cryptoQuoteRate is the rate to quote a coin at now, or why there is none.
func cryptoQuoteRate(ctx context.Context, config *CryptoPayConfig, asset string) (*cryptoRateQuote, string) {
	assetConfig := config.Asset(asset)
	if assetConfig == nil || len(assetConfig.RateSources) == 0 {
		return nil, cryptoQuoteRefusedNoSources
	}
	// ask every source at once: a slow source costs one timeout, not one each
	answers := make([]*cryptoRateAnswer, len(assetConfig.RateSources))
	var wait sync.WaitGroup
	for i, source := range assetConfig.RateSources {
		wait.Add(1)
		go func() {
			defer wait.Done()
			answers[i] = cryptoRates.answer(ctx, source, asset, config.Quote.RateCache)
		}()
	}
	wait.Wait()
	return cryptoDecideRate(answers, cryptoRateNow(), config.Quote.RateStaleAfter, config.Quote.RateMaxSpreadPercent)
}

// The coin list's rate refusal: the coin's sources were asked recently and
// left fewer than two fresh answers within the spread.
const cryptoQuoteRefusedRates = "rates_unavailable"

// cryptoCachedRateAvailable is the rate part of the coin list's availability,
// from this process's cached answers alone: it never asks a source, so the
// list and the balance never wait on one. A coin whose sources were asked
// within rate_stale_after and left fewer than two fresh answers that agree is
// unavailable (rates_unavailable). A coin no quote asked about lately is not
// judged here (available): the intent path asks its sources and refuses with
// the same rule.
func cryptoCachedRateAvailable(config *CryptoPayConfig, asset string, now time.Time) (bool, string) {
	assetConfig := config.Asset(asset)
	if assetConfig == nil || len(assetConfig.RateSources) == 0 {
		return false, cryptoQuoteRefusedNoSources
	}
	answers := []*cryptoRateAnswer{}
	asked := false
	for _, source := range assetConfig.RateSources {
		entry := cryptoRates.entry(source, asset)
		entry.stateLock.Lock()
		if !entry.lastAttemptAt.IsZero() && now.Sub(entry.lastAttemptAt) <= config.Quote.RateStaleAfter {
			asked = true
		}
		answers = append(answers, entry.answer)
		entry.stateLock.Unlock()
	}
	if !asked {
		return true, ""
	}
	if quote, _ := cryptoDecideRate(answers, now, config.Quote.RateStaleAfter, config.Quote.RateMaxSpreadPercent); quote == nil {
		return false, cryptoQuoteRefusedRates
	}
	return true, ""
}

// cryptoDecideRate applies the fail-closed rule to the answers in hand: at
// least two fresh answers whose spread (max over min) is within the limit,
// quoted at their median. Pure, so the rule is tested without sources.
func cryptoDecideRate(
	answers []*cryptoRateAnswer,
	now time.Time,
	staleAfter time.Duration,
	maxSpreadPercent float64,
) (*cryptoRateQuote, string) {
	fresh := []*cryptoRateAnswer{}
	for _, answer := range answers {
		if answer == nil || !(0 < answer.RateUsd) {
			continue
		}
		if staleAfter < now.Sub(answer.asOf()) {
			continue
		}
		fresh = append(fresh, answer)
	}
	if len(fresh) < 2 {
		return nil, cryptoQuoteRefusedTooFew
	}
	rates := make([]float64, 0, len(fresh))
	for _, answer := range fresh {
		rates = append(rates, answer.RateUsd)
	}
	sort.Float64s(rates)
	minRate, maxRate := rates[0], rates[len(rates)-1]
	spreadPercent := (maxRate - minRate) / minRate * 100
	if maxSpreadPercent < spreadPercent {
		return nil, cryptoQuoteRefusedDivergent
	}
	var median float64
	if len(rates)%2 == 1 {
		median = rates[len(rates)/2]
	} else {
		median = (rates[len(rates)/2-1] + rates[len(rates)/2]) / 2
	}
	return &cryptoRateQuote{
		RateUsd:       median,
		Answers:       fresh,
		SpreadPercent: spreadPercent,
	}, ""
}

// ----- quote math -----

// The quoted amount has at most this many decimals, whatever the coin's
// atomic unit: short enough to type, and the same on every invoice.
const cryptoQuoteDecimals = 6

// cryptoQuoteAmountAtomic is the coin amount for a USD price at a rate, with
// the buffer, rounded UP to cryptoQuoteDecimals decimals and returned in the
// coin's atomic unit. Exact rational arithmetic: the rounding is a function of
// the inputs alone, never of float dust. ok = false for an unusable input.
func cryptoQuoteAmountAtomic(priceUsd float64, rateUsd float64, bufferPercent float64, decimals int) (int64, bool) {
	return cryptoQuoteAmountAtomicAt(priceUsd, rateUsd, bufferPercent, decimals, cryptoQuoteDecimals)
}

// cryptoAssetQuoteAmountAtomic is cryptoQuoteAmountAtomic at the coin's own
// decimals and quote decimals (Bitcoin to the satoshi, mmm/BTCPAY.md §17.7).
func cryptoAssetQuoteAmountAtomic(asset string, priceUsd float64, rateUsd float64, bufferPercent float64) (int64, bool) {
	return cryptoQuoteAmountAtomicAt(priceUsd, rateUsd, bufferPercent, cryptoAssetDecimals(asset), cryptoAssetQuoteDecimals(asset))
}

// cryptoQuoteAmountAtomicAt is the quote math rounded up to quoteDecimals.
func cryptoQuoteAmountAtomicAt(priceUsd float64, rateUsd float64, bufferPercent float64, decimals int, quoteDecimals int) (int64, bool) {
	if !(0 < priceUsd) || !(0 < rateUsd) || bufferPercent < 0 || quoteDecimals <= 0 || decimals < quoteDecimals ||
		math.IsInf(priceUsd, 0) || math.IsInf(rateUsd, 0) || math.IsNaN(bufferPercent) || math.IsInf(bufferPercent, 0) {
		return 0, false
	}
	price := new(big.Rat).SetFloat64(priceUsd)
	rate := new(big.Rat).SetFloat64(rateUsd)
	buffer := new(big.Rat).SetFloat64(bufferPercent)
	if price == nil || rate == nil || buffer == nil {
		return 0, false
	}
	// coins = price / rate * (1 + buffer / 100)
	coins := new(big.Rat).Quo(price, rate)
	factor := new(big.Rat).Add(big.NewRat(1, 1), new(big.Rat).Quo(buffer, big.NewRat(100, 1)))
	coins.Mul(coins, factor)
	// ceil(coins * 10^q) in units of 10^-q coin
	units := new(big.Rat).Mul(coins, new(big.Rat).SetInt(pow10Int(quoteDecimals)))
	unitsCeil := ratCeil(units)
	atomic := new(big.Int).Mul(unitsCeil, pow10Int(decimals-quoteDecimals))
	if !atomic.IsInt64() || atomic.Sign() <= 0 {
		return 0, false
	}
	return atomic.Int64(), true
}

// cryptoAcceptThresholdAtomic is the least a payment of amountAtomic may be
// to count as paid: acceptPercent of it, rounded up (a payment of 99.5% of the
// quote is the rounding of a wallet, not an underpayment).
func cryptoAcceptThresholdAtomic(amountAtomic int64, acceptPercent float64) int64 {
	if amountAtomic <= 0 {
		return 0
	}
	accept := new(big.Rat).SetFloat64(acceptPercent)
	if accept == nil {
		return amountAtomic
	}
	threshold := new(big.Rat).Mul(new(big.Rat).SetInt64(amountAtomic), accept)
	threshold.Quo(threshold, big.NewRat(100, 1))
	ceil := ratCeil(threshold)
	if !ceil.IsInt64() {
		return amountAtomic
	}
	return min(ceil.Int64(), amountAtomic)
}

// cryptoFormatAtomic prints an atomic amount as an exact decimal string in
// coin units, without trailing zeros: 72386000000 piconero is "0.072386".
func cryptoFormatAtomic(amountAtomic int64, decimals int) string {
	negative := amountAtomic < 0
	value := new(big.Int).SetInt64(amountAtomic)
	value.Abs(value)
	text := value.String()
	if decimals <= 0 {
		if negative {
			return "-" + text
		}
		return text
	}
	if len(text) <= decimals {
		text = strings.Repeat("0", decimals-len(text)+1) + text
	}
	whole := text[:len(text)-decimals]
	fraction := strings.TrimRight(text[len(text)-decimals:], "0")
	out := whole
	if fraction != "" {
		out = whole + "." + fraction
	}
	if negative {
		return "-" + out
	}
	return out
}

// cryptoAtomicUsdValueNanoCents is the USD value of an atomic amount at a
// rate, in nano cents, rounded down.
func cryptoAtomicUsdValueNanoCents(amountAtomic int64, decimals int, rateUsd float64) model.NanoCents {
	if amountAtomic <= 0 || !(0 < rateUsd) || math.IsInf(rateUsd, 0) {
		return 0
	}
	value := new(big.Rat).SetInt64(amountAtomic)
	value.Quo(value, new(big.Rat).SetInt(pow10Int(decimals)))
	rate := new(big.Rat).SetFloat64(rateUsd)
	if rate == nil {
		return 0
	}
	value.Mul(value, rate)
	// usd -> nano cents: 1 usd = 10^9 nano cents (model.UsdToNanoCents)
	value.Mul(value, new(big.Rat).SetInt(pow10Int(9)))
	floor := new(big.Int).Quo(value.Num(), value.Denom())
	if !floor.IsInt64() {
		return math.MaxInt64
	}
	return floor.Int64()
}

func pow10Int(exponent int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
}

// ratCeil is the least integer not below r.
func ratCeil(r *big.Rat) *big.Int {
	quotient, remainder := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if 0 < remainder.Sign() {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}

// ----- the sources -----

// Kraken's USD pair per coin, by its alt name (mmm/BTCPAY.md §17.4).
var cryptoKrakenPairs = map[string]string{
	model.CryptoAssetXmr: "XMRUSD",
	model.CryptoAssetZec: "ZECUSD",
	model.CryptoAssetTao: "TAOUSD",
	model.CryptoAssetBtc: "XBTUSD",
	model.CryptoAssetEth: "ETHUSD",
	model.CryptoAssetSol: "SOLUSD",
}

// Coinbase's currency code per coin. Coinbase lists no Monero.
var cryptoCoinbaseRateCodes = map[string]string{
	model.CryptoAssetZec: "ZEC",
	model.CryptoAssetTao: "TAO",
	model.CryptoAssetBtc: "BTC",
	model.CryptoAssetEth: "ETH",
	model.CryptoAssetSol: "SOL",
}

// Kraken's public ticker. The coin's pair is asked by its alt name; the one
// result entry is keyed by Kraken's own name (XXMRZUSD, XXBTZUSD), and c[0] is
// the last trade's price.
func krakenFetchRate(ctx context.Context, asset string) (float64, *time.Time, error) {
	pair, ok := cryptoKrakenPairs[asset]
	if !ok {
		return 0, nil, fmt.Errorf("kraken has no pair for %s", asset)
	}
	type krakenTicker struct {
		Error  []string `json:"error"`
		Result map[string]struct {
			LastTrade []string `json:"c"`
		} `json:"result"`
	}
	ticker, err := server.HttpGetRequireStatusOk(
		ctx,
		fmt.Sprintf("%s/0/public/Ticker?pair=%s", cryptoKrakenBaseUrl, pair),
		cryptoRateHeaders,
		server.ResponseJsonObject[*krakenTicker],
	)
	if err != nil {
		return 0, nil, err
	}
	if ticker == nil || 0 < len(ticker.Error) {
		return 0, nil, fmt.Errorf("kraken answered an error")
	}
	if len(ticker.Result) != 1 {
		return 0, nil, fmt.Errorf("kraken answered %d pairs, want 1", len(ticker.Result))
	}
	for _, entry := range ticker.Result {
		if len(entry.LastTrade) == 0 {
			return 0, nil, fmt.Errorf("kraken answered no last trade")
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(entry.LastTrade[0]), 64)
		if err != nil {
			return 0, nil, err
		}
		return rate, nil, nil
	}
	return 0, nil, errors.New("unreachable")
}

// CoinGecko's simple price, for Monero only (cryptoRateSourceCoingecko): its
// keyless terms exclude production use, so no other coin asks it, and it
// stays only until the owner names Monero's second source.
func coingeckoFetchRate(ctx context.Context, asset string) (float64, *time.Time, error) {
	if asset != model.CryptoAssetXmr {
		return 0, nil, fmt.Errorf("coingecko is not a rate source for %s", asset)
	}
	type coingeckoPrice struct {
		Usd           float64 `json:"usd"`
		LastUpdatedAt int64   `json:"last_updated_at"`
	}
	prices, err := server.HttpGetRequireStatusOk(
		ctx,
		fmt.Sprintf("%s/api/v3/simple/price?ids=monero&vs_currencies=usd&include_last_updated_at=true", cryptoCoingeckoBaseUrl),
		cryptoRateHeaders,
		server.ResponseJsonObject[map[string]*coingeckoPrice],
	)
	if err != nil {
		return 0, nil, err
	}
	price := prices["monero"]
	if price == nil {
		return 0, nil, fmt.Errorf("coingecko answered no price for monero")
	}
	var sourceTime *time.Time
	if 0 < price.LastUpdatedAt {
		t := time.Unix(price.LastUpdatedAt, 0).UTC()
		sourceTime = &t
	}
	return price.Usd, sourceTime, nil
}

// Coinbase's exchange rates (the existing client): data.rates.USD is the USD
// price of one coin. Coinbase has no XMR.
func coinbaseFetchRate(ctx context.Context, asset string) (float64, *time.Time, error) {
	code, ok := cryptoCoinbaseRateCodes[asset]
	if !ok {
		return 0, nil, fmt.Errorf("coinbase has no rate for %s", asset)
	}
	rates, err := NewCoinbaseClient().FetchExchangeRates(ctx, code)
	if err != nil {
		return 0, nil, err
	}
	if rates == nil {
		return 0, nil, fmt.Errorf("coinbase answered no rates")
	}
	usd, ok := rates.Rates["USD"]
	if !ok {
		return 0, nil, fmt.Errorf("coinbase answered no USD rate")
	}
	rate, err := strconv.ParseFloat(strings.TrimSpace(usd), 64)
	if err != nil {
		return 0, nil, err
	}
	return rate, nil, nil
}

// The sources' base urls. Replaceable only by tests.
var (
	cryptoKrakenBaseUrl    = "https://api.kraken.com"
	cryptoCoingeckoBaseUrl = "https://api.coingecko.com"
)

func cryptoRateHeaders(header http.Header) {
	header.Set("Accept", "application/json")
}

// cryptoRateSourcesJson is what a quote keeps of the answers it was made from.
func cryptoRateSourcesJson(quote *cryptoRateQuote) string {
	if quote == nil {
		return ""
	}
	value, err := json.Marshal(map[string]any{
		"median_usd":     quote.RateUsd,
		"spread_percent": quote.SpreadPercent,
		"answers":        quote.Answers,
	})
	if err != nil {
		return ""
	}
	return string(value)
}
