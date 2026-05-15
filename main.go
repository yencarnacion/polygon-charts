// main.go

package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

var (
	apiKeyFlag = flag.String("apikey", "", "Polygon.io API key (overrides .env)")
	portFlag   = flag.Int("port", 0, "HTTP port (overrides .env)")
)

var (
	polygonAPIKey    string
	fmpAPIKey        string
	secAPIKey        string
	marketauxAPIKey  string
	marketauxEnabled bool
	patternfolioURL  string
	ntfyEnabled      bool
	ntfyServer       string
	ntfyTopic        string
	ntfyTagOptions   []string
	listenPort       int
)

var defaultNtfyTagOptions = []string{
	"morning top",
	"morning bottom",
	"rubber band",
	"right side of the v",
	"flush base",
}

var marketNewsLocation = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.UTC
	}
	return loc
}()

//go:embed index.html
var indexHTML string

//go:embed chart.html
var chartHTML string

type polygonBar struct {
	T int64   `json:"t"`
	O float64 `json:"o"`
	H float64 `json:"h"`
	L float64 `json:"l"`
	C float64 `json:"c"`
	V float64 `json:"v"`
}

type polygonResp struct {
	Results []polygonBar `json:"results"`
}

type candlePoint struct {
	Time  int64   `json:"time"`
	Open  float64 `json:"open"`
	High  float64 `json:"high"`
	Low   float64 `json:"low"`
	Close float64 `json:"close"`
}

type linePoint struct {
	Time  int64   `json:"time"`
	Value float64 `json:"value"`
}

type payload struct {
	Candles    []candlePoint `json:"candles"`
	Volume     []linePoint   `json:"volume"`
	MinCandles []candlePoint `json:"minCandles"`
	MinVolume  []linePoint   `json:"minVolume"`
	VWAP       []linePoint   `json:"vwap"`
	SMA9       []linePoint   `json:"sma"`
	CCI        []linePoint   `json:"cci"`
}

type PolygonTickerDetails struct {
	Results struct {
		MarketCap float64 `json:"market_cap"`
	} `json:"results"`
}

type FMPFloat struct {
	Symbol      string  `json:"symbol"`
	FloatShares float64 `json:"floatShares"`
}

type NewsItem struct {
	Title     string `json:"title"`
	Source    string `json:"source"`
	URL       string `json:"url"`
	Published string `json:"published"`
}

type marketauxEntityStat struct {
	Date           string  `json:"date,omitempty"`
	Symbol         string  `json:"symbol,omitempty"`
	Name           string  `json:"name,omitempty"`
	Country        string  `json:"country,omitempty"`
	Exchange       string  `json:"exchange,omitempty"`
	Industry       string  `json:"industry,omitempty"`
	TotalDocuments int     `json:"total_documents"`
	SentimentAvg   float64 `json:"sentiment_avg"`
	Score          float64 `json:"score"`
}

type appConfig struct {
	Ntfy ntfyConfig `yaml:"ntfy"`
}

type ntfyConfig struct {
	Tags []string `yaml:"tags"`
}

type ntfyPublishRequest struct {
	PageURL string   `json:"pageUrl"`
	Note    string   `json:"note"`
	Tags    []string `json:"tags"`
}

type ntfyPublishResponse struct {
	OK          bool   `json:"ok"`
	Server      string `json:"server"`
	Topic       string `json:"topic"`
	MessageID   string `json:"messageId,omitempty"`
	PublishedAt string `json:"publishedAt,omitempty"`
}

type ntfyAPIResponse struct {
	ID   string `json:"id"`
	Time int64  `json:"time"`
}

func marketauxDateTimeUTC(t time.Time) string {
	// Marketaux expects UTC datetime strings like YYYY-MM-DDTHH:MM (without timezone suffix).
	return t.UTC().Format("2006-01-02T15:04")
}

func valueAsString(v interface{}) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case fmt.Stringer:
		return strings.TrimSpace(t.String())
	default:
		out := strings.TrimSpace(fmt.Sprintf("%v", v))
		if out == "<nil>" {
			return ""
		}
		return out
	}
}

func valueAsFloat(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case int32:
		return float64(t)
	case int16:
		return float64(t)
	case int8:
		return float64(t)
	case uint:
		return float64(t)
	case uint64:
		return float64(t)
	case uint32:
		return float64(t)
	case uint16:
		return float64(t)
	case uint8:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	default:
		return 0
	}
}

func valueAsInt(v interface{}) int {
	return int(math.Round(valueAsFloat(v)))
}

func roundTo(v float64, decimals int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	p := math.Pow10(decimals)
	return math.Round(v*p) / p
}

func clamp(v, minV, maxV float64) float64 {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

func calculateCCI(candles []candlePoint, period int) []linePoint {
	if period < 2 || len(candles) < period {
		return []linePoint{}
	}
	tp := make([]float64, len(candles))
	for i, c := range candles {
		tp[i] = (c.High + c.Low + c.Close) / 3
	}
	out := make([]linePoint, 0, len(candles)-period+1)
	var sum float64
	for i := range candles {
		sum += tp[i]
		if i >= period {
			sum -= tp[i-period]
		}
		if i < period-1 {
			continue
		}
		sma := sum / float64(period)
		var md float64
		start := i - period + 1
		for j := start; j <= i; j++ {
			md += math.Abs(tp[j] - sma)
		}
		md /= float64(period)
		if md == 0 {
			continue
		}
		out = append(out, linePoint{
			Time:  candles[i].Time,
			Value: (tp[i] - sma) / (0.015 * md),
		})
	}
	return out
}

func normalizeTrendScore(raw float64) float64 {
	if math.IsNaN(raw) || math.IsInf(raw, 0) {
		return 0
	}
	if raw <= 0 {
		return 0
	}
	if raw <= 1 {
		return raw * 100
	}
	return math.Min(raw, 100)
}

func parseMarketauxRecord(record map[string]interface{}, fallbackDate string) marketauxEntityStat {
	symbol := strings.ToUpper(valueAsString(record["symbol"]))
	if symbol == "" {
		symbol = strings.ToUpper(valueAsString(record["key"]))
	}
	score := valueAsFloat(record["score"])
	if score == 0 {
		score = valueAsFloat(record["relevance_score"])
	}
	date := valueAsString(record["date"])
	if date == "" {
		date = fallbackDate
	}
	stat := marketauxEntityStat{
		Date:           date,
		Symbol:         symbol,
		Name:           valueAsString(record["name"]),
		Country:        strings.ToUpper(valueAsString(record["country"])),
		Exchange:       strings.ToUpper(valueAsString(record["exchange"])),
		Industry:       valueAsString(record["industry"]),
		TotalDocuments: valueAsInt(record["total_documents"]),
		SentimentAvg:   valueAsFloat(record["sentiment_avg"]),
		Score:          score,
	}
	if math.IsNaN(stat.SentimentAvg) || math.IsInf(stat.SentimentAvg, 0) {
		stat.SentimentAvg = 0
	}
	if math.IsNaN(stat.Score) || math.IsInf(stat.Score, 0) {
		stat.Score = 0
	}
	return stat
}

func parseMarketauxRows(data interface{}) []marketauxEntityStat {
	items, ok := data.([]interface{})
	if !ok {
		return nil
	}
	out := make([]marketauxEntityStat, 0, len(items))
	for _, item := range items {
		row, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fallbackDate := valueAsString(row["date"])
		if nested, hasNested := row["data"].([]interface{}); hasNested {
			for _, inner := range nested {
				innerRow, ok := inner.(map[string]interface{})
				if !ok {
					continue
				}
				out = append(out, parseMarketauxRecord(innerRow, fallbackDate))
			}
			continue
		}
		out = append(out, parseMarketauxRecord(row, fallbackDate))
	}
	return out
}

func queryMarketaux(path string, params url.Values) ([]marketauxEntityStat, error) {
	if marketauxAPIKey == "" {
		return nil, fmt.Errorf("MARKETAUX_API_KEY missing")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	p := url.Values{}
	for k, vals := range params {
		for _, val := range vals {
			p.Add(k, val)
		}
	}
	p.Set("api_token", marketauxAPIKey)
	endpoint := "https://api.marketaux.com" + path + "?" + p.Encode()
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("Marketaux %s: %s", path, msg)
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return parseMarketauxRows(payload["data"]), nil
}

func pickStatForSymbol(stats []marketauxEntityStat, symbol string) marketauxEntityStat {
	needle := strings.ToUpper(strings.TrimSpace(symbol))
	for _, stat := range stats {
		if strings.EqualFold(stat.Symbol, needle) {
			return stat
		}
	}
	if len(stats) > 0 {
		return stats[0]
	}
	return marketauxEntityStat{Symbol: needle}
}

func statToMap(stat marketauxEntityStat) map[string]interface{} {
	return map[string]interface{}{
		"date":            stat.Date,
		"symbol":          stat.Symbol,
		"name":            stat.Name,
		"country":         stat.Country,
		"exchange":        stat.Exchange,
		"industry":        stat.Industry,
		"total_documents": stat.TotalDocuments,
		"sentiment_avg":   roundTo(stat.SentimentAvg, 4),
		"score":           roundTo(normalizeTrendScore(stat.Score), 2),
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func buildLeaderReason(stat marketauxEntityStat) string {
	docs := stat.TotalDocuments
	sent := stat.SentimentAvg
	score := normalizeTrendScore(stat.Score)
	switch {
	case docs >= 15 && sent >= 0.25:
		return "Headline surge with bullish tone"
	case docs >= 15 && sent <= -0.25:
		return "Headline surge with bearish tone"
	case score >= 70 && sent >= 0.15:
		return "High trend score and positive narrative"
	case score >= 70 && sent <= -0.15:
		return "High trend score and negative narrative"
	case docs >= 10 && math.Abs(sent) < 0.1:
		return "Heavy headline flow, direction still mixed"
	case sent >= 0.35:
		return "Strong positive sentiment skew"
	case sent <= -0.35:
		return "Strong negative sentiment skew"
	default:
		return "Moderate news pressure"
	}
}

func buildTickerRadar(m15, m60, session, trend marketauxEntityStat) map[string]interface{} {
	docs15 := float64(m15.TotalDocuments)
	docs60 := float64(m60.TotalDocuments)
	sent15 := m15.SentimentAvg
	sent60 := m60.SentimentAvg
	sentShift := sent15 - sent60
	trendScore := normalizeTrendScore(trend.Score)
	if trendScore == 0 {
		trendScore = normalizeTrendScore(session.Score)
	}
	avgPer15m := math.Max(docs60/4.0, 1)
	docsAcceleration := docs15 / avgPer15m
	pulseScore := clamp(
		docsAcceleration*28+
			math.Abs(sentShift)*110+
			math.Min(docs15, 12)*2.2+
			trendScore*0.35,
		0,
		100,
	)
	confidence := clamp(
		math.Min(docs15, 20)*3.2+
			math.Abs(sent15)*45+
			math.Abs(sentShift)*35,
		0,
		100,
	)
	bias := "NEUTRAL"
	reason := "News flow is balanced; use price action confirmation."
	switch {
	case docs15 >= 5 && sent15 >= 0.22:
		bias = "LONG"
		reason = "Fresh positive headlines with elevated mention velocity."
	case docs15 >= 5 && sent15 <= -0.22:
		bias = "SHORT"
		reason = "Fresh negative headlines with elevated mention velocity."
	case sentShift >= 0.2 && docs15 >= 3:
		bias = "LONG"
		reason = "Sentiment is improving quickly versus the prior hour."
	case sentShift <= -0.2 && docs15 >= 3:
		bias = "SHORT"
		reason = "Sentiment is deteriorating quickly versus the prior hour."
	}
	return map[string]interface{}{
		"pulse_score":        roundTo(pulseScore, 1),
		"confidence":         roundTo(confidence, 1),
		"bias":               bias,
		"bias_reason":        reason,
		"docs_acceleration":  roundTo(docsAcceleration, 2),
		"sentiment_shift":    roundTo(sentShift, 4),
		"trend_score":        roundTo(trendScore, 2),
		"documents_last_15m": m15.TotalDocuments,
		"documents_last_60m": m60.TotalDocuments,
		"documents_session":  session.TotalDocuments,
		"sentiment_15m":      roundTo(sent15, 4),
		"sentiment_60m":      roundTo(sent60, 4),
		"sentiment_session":  roundTo(session.SentimentAvg, 4),
	}
}

func queryPolygon(sym string, mult int, span, from, to string) ([]polygonBar, error) {
	url := fmt.Sprintf(
		"https://api.massive.com/v2/aggs/ticker/%s/range/%d/%s/%s/%s?adjusted=true&sort=asc&apiKey=%s",
		sym, mult, span, from, to, polygonAPIKey)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Polygon: %s", resp.Status)
	}
	var pr polygonResp
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return pr.Results, nil
}

func queryPolygonTickerDetails(ticker string) (*PolygonTickerDetails, error) {
	url := fmt.Sprintf("https://api.massive.com/v3/reference/tickers/%s?apiKey=%s", ticker, polygonAPIKey)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Polygon Ticker Details: %s", resp.Status)
	}
	var details PolygonTickerDetails
	if err := json.NewDecoder(resp.Body).Decode(&details); err != nil {
		return nil, err
	}
	return &details, nil
}

func queryFMPFloat(ticker string) ([]FMPFloat, error) {
	url := fmt.Sprintf("https://financialmodelingprep.com/api/v4/shares_float?symbol=%s&apikey=%s", ticker, fmpAPIKey)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("FMP Float: %s", resp.Status)
	}
	var floats []FMPFloat
	if err := json.NewDecoder(resp.Body).Decode(&floats); err != nil {
		return nil, err
	}
	return floats, nil
}

func queryFMPProfile(symbol string) ([]map[string]interface{}, error) {
	url := fmt.Sprintf("https://financialmodelingprep.com/api/v3/profile/%s?apikey=%s", symbol, fmpAPIKey)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("FMP Profile: %s", resp.Status)
	}
	var profile []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return nil, err
	}
	return profile, nil
}

// queryMassiveBenzingaNews uses Massive real-time Benzinga news:
// https://massive.com/docs/rest/partners/benzinga/news
func queryMassiveBenzingaNews(ticker, dateStr string) ([]map[string]interface{}, error) {
	nextD := nextDay(dateStr)
	params := url.Values{}
	params.Set("tickers", strings.ToUpper(strings.TrimSpace(ticker)))
	params.Set("published.gte", dateStr+"T00:00:00Z")
	params.Set("published.lt", nextD+"T00:00:00Z")
	params.Set("limit", "50")
	params.Set("sort", "published")
	params.Set("order", "desc")
	params.Set("apiKey", polygonAPIKey)
	endpoint := "https://api.massive.com/benzinga/v2/news?" + params.Encode()
	resp, err := http.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Massive Benzinga News: %s", resp.Status)
	}
	var pr struct {
		Results []map[string]interface{} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return pr.Results, nil
}

func queryFMPNews(ticker, dateStr string) ([]map[string]interface{}, error) {
	url := fmt.Sprintf("https://financialmodelingprep.com/api/v3/stock_news?tickers=%s&from=%s&to=%s&limit=50&apikey=%s",
		ticker, dateStr, dateStr, fmpAPIKey)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("FMP News: %s", resp.Status)
	}
	var news []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&news); err != nil {
		return nil, err
	}
	return news, nil
}

func firstNonEmptyString(values map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if val, ok := values[key]; ok {
			s := valueAsString(val)
			if s != "" {
				return s
			}
		}
	}
	return ""
}

func parseMassiveNewsItem(raw map[string]interface{}) (NewsItem, bool) {
	title := firstNonEmptyString(raw, "title", "headline")
	if title == "" {
		return NewsItem{}, false
	}

	articleURL := firstNonEmptyString(raw, "article_url", "url", "amp_url")
	if articleURL == "" {
		return NewsItem{}, false
	}

	published := firstNonEmptyString(raw, "published", "published_utc", "updated", "created")
	if published == "" {
		return NewsItem{}, false
	}

	source := firstNonEmptyString(raw, "source")
	if source == "" {
		if publisher, ok := raw["publisher"].(map[string]interface{}); ok {
			source = firstNonEmptyString(publisher, "name", "title")
		}
	}
	if source == "" {
		source = firstNonEmptyString(raw, "author")
	}

	return NewsItem{
		Title:     title,
		Source:    source,
		URL:       articleURL,
		Published: normalizePublished(published),
	}, true
}

func appendMassiveNewsItems(dst []NewsItem, rawItems []map[string]interface{}) []NewsItem {
	for _, raw := range rawItems {
		if item, ok := parseMassiveNewsItem(raw); ok {
			dst = append(dst, item)
		}
	}
	return dst
}

func isClassActionLawsuitNews(item NewsItem) bool {
	title := strings.ToLower(strings.TrimSpace(item.Title))
	if title == "" {
		return false
	}

	strongPhrases := []string{
		"class action",
		"class action lawsuit",
		"securities fraud lawsuit",
		"shareholder alert",
		"investor alert",
		"lead plaintiff",
		"investor counsel",
	}
	for _, phrase := range strongPhrases {
		if strings.Contains(title, phrase) {
			return true
		}
	}

	if strings.Contains(title, "lawsuit") {
		if strings.Contains(title, "investor") || strings.Contains(title, "shareholder") || strings.Contains(title, "law firm") {
			return true
		}
	}

	if strings.Contains(title, "law firm") && (strings.Contains(title, "investor") || strings.Contains(title, "shareholder")) {
		return true
	}

	return false
}

func filterTradeableNews(items []NewsItem) []NewsItem {
	out := make([]NewsItem, 0, len(items))
	for _, item := range items {
		if isClassActionLawsuitNews(item) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func querySECFilings(ticker, dateStr string) ([]map[string]interface{}, error) {
	payload := map[string]interface{}{
		"query": fmt.Sprintf("ticker:%s AND filedAt:[%s TO %s]", ticker, dateStr, dateStr),
		"from":  "0",
		"size":  "50",
		"sort":  []map[string]map[string]string{{"filedAt": {"order": "desc"}}},
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", "https://api.sec-api.io?token="+secAPIKey, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SEC API: %s", resp.Status)
	}
	var result struct {
		Filings []map[string]interface{} `json:"filings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Filings, nil
}

func nextDay(dateStr string) string {
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return ""
	}
	return t.Add(24 * time.Hour).Format("2006-01-02")
}

func parsePublished(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}

	if unix, err := strconv.ParseInt(s, 10, 64); err == nil {
		if len(s) >= 13 {
			return time.UnixMilli(unix).UTC()
		}
		if len(s) >= 10 {
			return time.Unix(unix, 0).UTC()
		}
	}

	withZone := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999 -07:00",
		"2006-01-02 15:04:05 -07:00",
		"2006-01-02 15:04:05.999999999 -0700",
		"2006-01-02 15:04:05 -0700",
	}
	for _, layout := range withZone {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}

	withoutZone := []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02",
	}
	for _, layout := range withoutZone {
		if t, err := time.ParseInLocation(layout, s, marketNewsLocation); err == nil {
			return t.UTC()
		}
	}

	return time.Time{}
}

func normalizePublished(s string) string {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return ""
	}
	t := parsePublished(raw)
	if t.IsZero() {
		return raw
	}
	return t.UTC().Format(time.RFC3339)
}

func isNewsNewer(a, b NewsItem) bool {
	ta := parsePublished(a.Published)
	tb := parsePublished(b.Published)

	switch {
	case ta.After(tb):
		return true
	case tb.After(ta):
		return false
	}

	if a.Published != b.Published {
		return a.Published > b.Published
	}
	if a.Title != b.Title {
		return a.Title < b.Title
	}
	return a.URL < b.URL
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func normalizeNtfyServer(raw string) string {
	server := strings.TrimSpace(raw)
	if server == "" {
		server = "push.example.com"
	}
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	return strings.TrimRight(server, "/")
}

func cleanStringSlice(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, trimmed)
	}
	return out
}

func loadAppConfig(path string) appConfig {
	cfg := appConfig{
		Ntfy: ntfyConfig{
			Tags: append([]string(nil), defaultNtfyTagOptions...),
		},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("config.yaml read error: %v", err)
		}
		return cfg
	}
	var parsed appConfig
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		log.Printf("config.yaml parse error: %v", err)
		return cfg
	}
	if tags := cleanStringSlice(parsed.Ntfy.Tags); len(tags) > 0 {
		cfg.Ntfy.Tags = tags
	}
	return cfg
}

func jsValue(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func ntfyEnabledClass() string {
	if ntfyEnabled {
		return ""
	}
	return "d-none"
}

func applySharedTemplateVars(html string) string {
	html = strings.ReplaceAll(html, "{{PATTERNFOLIO_URL}}", patternfolioURL)
	html = strings.ReplaceAll(html, "{{MARKETAUX_ENABLED}}", strconv.FormatBool(marketauxEnabled))
	html = strings.ReplaceAll(html, "{{NTFY_ENABLED}}", strconv.FormatBool(ntfyEnabled))
	html = strings.ReplaceAll(html, "{{NTFY_SERVER_JSON}}", jsValue(ntfyServer))
	html = strings.ReplaceAll(html, "{{NTFY_TOPIC_JSON}}", jsValue(ntfyTopic))
	html = strings.ReplaceAll(html, "{{NTFY_TAG_OPTIONS_JSON}}", jsValue(ntfyTagOptions))
	html = strings.ReplaceAll(html, "{{NTFY_ENABLED_CLASS}}", ntfyEnabledClass())
	return html
}

func normalizePageURL(raw string) (string, error) {
	pageURL := strings.TrimSpace(raw)
	if pageURL == "" {
		return "", fmt.Errorf("page URL is required")
	}
	u, err := url.Parse(pageURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("page URL must be absolute")
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("page URL must use http or https")
	}
	return u.String(), nil
}

func filterSelectedNtfyTags(selected []string) []string {
	selectedSet := make(map[string]bool, len(selected))
	for _, raw := range selected {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key != "" {
			selectedSet[key] = true
		}
	}
	out := make([]string, 0, len(selectedSet))
	for _, option := range ntfyTagOptions {
		key := strings.ToLower(strings.TrimSpace(option))
		if selectedSet[key] {
			out = append(out, option)
		}
	}
	return out
}

func ntfyHeaderTag(label string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(label)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevDash = false
		case !prevDash && b.Len() > 0:
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func buildNtfyHeaderTags(tags []string) string {
	out := make([]string, 0, len(tags))
	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		headerTag := ntfyHeaderTag(tag)
		if headerTag == "" || seen[headerTag] {
			continue
		}
		seen[headerTag] = true
		out = append(out, headerTag)
	}
	return strings.Join(out, ",")
}

func buildNtfyTitle(pageURL string) string {
	u, err := url.Parse(pageURL)
	if err != nil {
		return "Trade note"
	}
	q := u.Query()
	parts := []string{}
	if ticker := strings.ToUpper(strings.TrimSpace(q.Get("ticker"))); ticker != "" {
		parts = append(parts, ticker)
	}
	if signal := strings.ToUpper(strings.TrimSpace(q.Get("signal"))); signal != "" {
		parts = append(parts, signal)
	}
	if timeStr, err := normalizeHHMM(q.Get("time")); err == nil && timeStr != "" {
		parts = append(parts, timeStr)
	}
	if dateStr := strings.TrimSpace(q.Get("date")); dateStr != "" {
		parts = append(parts, dateStr)
	}
	if len(parts) == 0 {
		return "Trade note"
	}
	return strings.Join(parts, " ")
}

func buildNtfyMarkdown(pageURL, note string, tags []string) string {
	lines := []string{"## Trade Note", ""}
	if len(tags) > 0 {
		lines = append(lines, fmt.Sprintf("**Tags:** %s", strings.Join(tags, ", ")), "")
	}
	trimmedNote := strings.TrimSpace(note)
	if trimmedNote != "" {
		lines = append(lines, "**Notes**", trimmedNote, "")
	}
	lines = append(lines,
		fmt.Sprintf("**Chart:** [Open signal chart](%s)", pageURL),
		"",
		pageURL,
	)
	return strings.Join(lines, "\n")
}

func ntfyPublishHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !ntfyEnabled {
		http.Error(w, "ntfy is disabled", http.StatusNotFound)
		return
	}
	var reqBody ntfyPublishRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	pageURL, err := normalizePageURL(reqBody.PageURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	selectedTags := filterSelectedNtfyTags(reqBody.Tags)
	message := buildNtfyMarkdown(pageURL, reqBody.Note, selectedTags)
	endpoint := fmt.Sprintf("%s/%s", strings.TrimRight(ntfyServer, "/"), url.PathEscape(ntfyTopic))
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(message))
	if err != nil {
		http.Error(w, "failed to build ntfy request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "text/markdown; charset=utf-8")
	req.Header.Set("Markdown", "yes")
	req.Header.Set("Title", buildNtfyTitle(pageURL))
	req.Header.Set("Click", pageURL)
	req.Header.Set("Accept", "application/json")
	if headerTags := buildNtfyHeaderTags(selectedTags); headerTags != "" {
		req.Header.Set("Tags", headerTags)
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "failed to reach ntfy server: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		http.Error(w, "ntfy publish failed: "+message, http.StatusBadGateway)
		return
	}
	var ntfyResp ntfyAPIResponse
	_ = json.Unmarshal(body, &ntfyResp)
	publishedAt := ""
	if ntfyResp.Time > 0 {
		publishedAt = time.Unix(ntfyResp.Time, 0).UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ntfyPublishResponse{
		OK:          true,
		Server:      ntfyServer,
		Topic:       ntfyTopic,
		MessageID:   ntfyResp.ID,
		PublishedAt: publishedAt,
	})
}

func openBrowser(u string) {
	if err := exec.Command("google-chrome", "--new-tab", u).Start(); err != nil {
		_ = exec.Command("xdg-open", u).Start()
	}
}

func rootHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	html := applySharedTemplateVars(indexHTML)
	fmt.Fprint(w, html)
}

func validISODate(dateStr string) bool {
	_, err := time.Parse("2006-01-02", dateStr)
	return err == nil
}

func normalizeHHMM(timeStr string) (string, error) {
	s := strings.TrimSpace(timeStr)
	if s == "" {
		return "", nil
	}
	s = strings.ReplaceAll(s, ":", "")
	if len(s) != 4 {
		return "", fmt.Errorf("time must be in HHMM or HH:MM format")
	}
	h, err := strconv.Atoi(s[:2])
	if err != nil {
		return "", fmt.Errorf("time must be in HHMM or HH:MM format")
	}
	m, err := strconv.Atoi(s[2:])
	if err != nil {
		return "", fmt.Errorf("time must be in HHMM or HH:MM format")
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return "", fmt.Errorf("time must be a valid 24-hour value")
	}
	return s, nil
}

func validResolution(tf string) bool {
	switch tf {
	case "1m", "2m", "3m", "5m", "10m", "15m", "30m", "1h":
		return true
	default:
		return false
	}
}

func openChartHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/open-chart"), "/")
	q := r.URL.Query()

	var ticker, dateStr, timeStr string
	if path != "" {
		parts := strings.Split(path, "/")
		if len(parts) < 2 || len(parts) > 3 {
			http.Error(w, "path must be /api/open-chart/{ticker}/{date}[/time]", http.StatusBadRequest)
			return
		}
		ticker = parts[0]
		dateStr = parts[1]
		if len(parts) == 3 {
			timeStr = parts[2]
		}
	} else {
		ticker = q.Get("ticker")
		dateStr = q.Get("date")
		timeStr = q.Get("time")
	}

	ticker = strings.ToUpper(strings.TrimSpace(ticker))
	dateStr = strings.TrimSpace(dateStr)
	if ticker == "" || dateStr == "" {
		http.Error(w, "ticker and date are required", http.StatusBadRequest)
		return
	}
	if !validISODate(dateStr) {
		http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	normTime, err := normalizeHHMM(timeStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resolution := strings.ToLower(strings.TrimSpace(q.Get("resolution")))
	if resolution == "" {
		resolution = "1m"
	}
	if !validResolution(resolution) {
		http.Error(w, "unsupported resolution", http.StatusBadRequest)
		return
	}

	signal := strings.ToLower(strings.TrimSpace(q.Get("signal")))
	if signal == "" {
		signal = "buy"
	}
	if signal != "buy" && signal != "sell" {
		http.Error(w, "signal must be buy or sell", http.StatusBadRequest)
		return
	}

	params := url.Values{}
	params.Set("ticker", ticker)
	params.Set("date", dateStr)
	params.Set("resolution", resolution)
	params.Set("signal", signal)
	if normTime != "" {
		params.Set("time", normTime)
	}
	http.Redirect(w, r, "/chart?"+params.Encode(), http.StatusFound)
}

func candlesHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	symbol := strings.ToUpper(q.Get("ticker"))
	tf := strings.ToLower(strings.TrimSpace(q.Get("timeframe")))
	from := q.Get("from")
	to := q.Get("to")
	if to == "" {
		to = q.Get("date")
	}
	if from == "" {
		from = q.Get("date")
	}
	isValid := func(s string) bool { _, err := time.Parse("2006-01-02", s); return err == nil }
	if from == "" || to == "" || !isValid(from) || !isValid(to) {
		http.Error(w, "from/to/date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	extended := q.Get("extended") == "true"
	if symbol == "" || tf == "" {
		http.Error(w, "ticker, timeframe required", 400)
		return
	}
	var mult int
	var span string
	switch tf {
	case "1m", "1min":
		mult, span = 1, "minute"
	case "2m", "2min":
		mult, span = 2, "minute"
	case "3m", "3min":
		mult, span = 3, "minute"
	case "5m", "5min":
		mult, span = 5, "minute"
	case "10m", "10min":
		mult, span = 10, "minute"
	case "15m", "15min":
		mult, span = 15, "minute"
	case "30m", "30min":
		mult, span = 30, "minute"
	case "1h", "1hr":
		mult, span = 1, "hour"
	case "1d":
		mult, span = 1, "day"
	default:
		http.Error(w, "unsupported timeframe", 400)
		return
	}
	bars, err := queryPolygon(symbol, mult, span, from, to)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	loc, _ := time.LoadLocation("America/New_York")
	candles := make([]candlePoint, 0, len(bars))
	vol := make([]linePoint, 0, len(bars))
	for _, b := range bars {
		ts := time.UnixMilli(b.T).In(loc)
		h := ts.Hour()
		if !extended && span == "minute" && (h < 7 || h >= 16) {
			continue
		}
		candles = append(candles, candlePoint{
			Time:  b.T / 1000,
			Open:  b.O,
			High:  b.H,
			Low:   b.L,
			Close: b.C,
		})
		vol = append(vol, linePoint{Time: b.T / 1000, Value: b.V})
	}
	var minBars []polygonBar
	var minErr error
	if span != "day" {
		if span == "minute" && mult == 1 {
			minBars = bars
		} else {
			minBars, minErr = queryPolygon(symbol, 1, "minute", from, to)
			if minErr != nil {
				http.Error(w, minErr.Error(), 502)
				return
			}
		}
	}
	minCandles := make([]candlePoint, 0)
	minVolume := make([]linePoint, 0)
	vwap := make([]linePoint, 0)
	if span != "day" {
		var cumPV, cumV float64
		for _, b := range minBars {
			ts := time.UnixMilli(b.T).In(loc)
			h := ts.Hour()
			if !extended && (h < 7 || h >= 16) {
				continue
			}
			typ := (b.H + b.L + b.C) / 3
			cumPV += typ * b.V
			cumV += b.V
			if cumV > 0 {
				vwap = append(vwap, linePoint{Time: b.T / 1000, Value: cumPV / cumV})
			} else if len(vwap) > 0 {
				vwap = append(vwap, linePoint{Time: b.T / 1000, Value: vwap[len(vwap)-1].Value})
			}
			minCandles = append(minCandles, candlePoint{
				Time:  b.T / 1000,
				Open:  b.O,
				High:  b.H,
				Low:   b.L,
				Close: b.C,
			})
			minVolume = append(minVolume, linePoint{Time: b.T / 1000, Value: b.V})
		}
	}
	var sum float64
	sma := make([]linePoint, 0, len(candles))
	for i, c := range candles {
		sum += c.Close
		if i >= 9 {
			sum -= candles[i-9].Close
		}
		if i >= 8 {
			sma = append(sma, linePoint{Time: c.Time, Value: sum / 9})
		}
	}
	cci := calculateCCI(candles, 27)
	out := payload{Candles: candles, Volume: vol, MinCandles: minCandles, MinVolume: minVolume, VWAP: vwap, SMA9: sma, CCI: cci}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func tickerDetailsHandler(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.URL.Query().Get("ticker"))
	if ticker == "" {
		http.Error(w, "ticker required", 400)
		return
	}
	details, err := queryPolygonTickerDetails(ticker)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(details)
}

func shareFloatHandler(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.URL.Query().Get("ticker"))
	if ticker == "" {
		http.Error(w, "ticker required", 400)
		return
	}
	floats, err := queryFMPFloat(ticker)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(floats)
}

func profileHandler(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("ticker")))
	if ticker == "" {
		http.Error(w, "ticker required", 400)
		return
	}
	profile, err := queryFMPProfile(ticker)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	var profileData map[string]interface{}
	if len(profile) > 0 {
		profileData = profile[0]
	} else {
		profileData = map[string]interface{}{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(profileData)
}

func extraHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ticker := strings.ToUpper(q.Get("ticker"))
	dateStr := q.Get("date")
	days, _ := strconv.Atoi(q.Get("days"))
	if days == 0 {
		days = 1
	}
	if ticker == "" || dateStr == "" {
		http.Error(w, "ticker and date required", 400)
		return
	}
	var allNewsItems []NewsItem
	var allFilings []map[string]interface{}
	var profile []map[string]interface{}
	includeProfile := strings.ToLower(strings.TrimSpace(q.Get("profile"))) != "false"
	// Fetch for current day
	pNews, _ := queryMassiveBenzingaNews(ticker, dateStr)
	allNewsItems = appendMassiveNewsItems(allNewsItems, pNews)
	fNews, _ := queryFMPNews(ticker, dateStr)
	for _, f := range fNews {
		val, exists := f["title"]
		if !exists || val == nil {
			continue
		}
		title, ok := val.(string)
		if !ok {
			continue
		}
		val, exists = f["site"]
		if !exists || val == nil {
			continue
		}
		site, ok := val.(string)
		if !ok {
			continue
		}
		val, exists = f["url"]
		if !exists || val == nil {
			continue
		}
		url, ok := val.(string)
		if !ok {
			continue
		}
		val, exists = f["publishedDate"]
		if !exists || val == nil {
			continue
		}
		pub, ok := val.(string)
		if !ok {
			continue
		}
		allNewsItems = append(allNewsItems, NewsItem{
			Title:     title,
			Source:    site,
			URL:       url,
			Published: normalizePublished(pub),
		})
	}
	filings, _ := querySECFilings(ticker, dateStr)
	allFilings = append(allFilings, filings...)
	if includeProfile {
		profile, _ = queryFMPProfile(ticker)
	}
	// Fetch for prior day if days=2
	if days == 2 {
		priorStr, err := getPriorTradingDate(ticker, dateStr)
		if err == nil {
			pNews, _ = queryMassiveBenzingaNews(ticker, priorStr)
			allNewsItems = appendMassiveNewsItems(allNewsItems, pNews)
			fNews, _ = queryFMPNews(ticker, priorStr)
			for _, f := range fNews {
				val, exists := f["title"]
				if !exists || val == nil {
					continue
				}
				title, ok := val.(string)
				if !ok {
					continue
				}
				val, exists = f["site"]
				if !exists || val == nil {
					continue
				}
				site, ok := val.(string)
				if !ok {
					continue
				}
				val, exists = f["url"]
				if !exists || val == nil {
					continue
				}
				url, ok := val.(string)
				if !ok {
					continue
				}
				val, exists = f["publishedDate"]
				if !exists || val == nil {
					continue
				}
				pub, ok := val.(string)
				if !ok {
					continue
				}
				allNewsItems = append(allNewsItems, NewsItem{
					Title:     title,
					Source:    site,
					URL:       url,
					Published: normalizePublished(pub),
				})
			}
			filings, _ := querySECFilings(ticker, priorStr)
			allFilings = append(allFilings, filings...)
		} else {
			log.Println("Could not find prior trading date:", err)
		}
	}
	// Deduplicate news by URL
	allNewsItems = filterTradeableNews(allNewsItems)
	uniqueMap := make(map[string]NewsItem)
	for _, n := range allNewsItems {
		uniqueMap[n.URL] = n
	}
	uniqueNews := []NewsItem{}
	for _, n := range uniqueMap {
		uniqueNews = append(uniqueNews, n)
	}
	sort.Slice(uniqueNews, func(i, j int) bool {
		return isNewsNewer(uniqueNews[i], uniqueNews[j])
	})
	// Sort filings by filedAt desc
	sort.Slice(allFilings, func(i, j int) bool {
		ti := parsePublished(allFilings[i]["filedAt"].(string))
		tj := parsePublished(allFilings[j]["filedAt"].(string))
		return ti.After(tj)
	})
	var profileData map[string]interface{}
	if len(profile) > 0 {
		profileData = profile[0]
	}
	out := map[string]interface{}{
		"news":    uniqueNews,
		"filings": allFilings,
		"profile": profileData,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func marketStatsHandler(w http.ResponseWriter, r *http.Request) {
	if marketauxAPIKey == "" {
		http.Error(w, "MARKETAUX_API_KEY missing", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	ticker := strings.ToUpper(strings.TrimSpace(q.Get("ticker")))
	if ticker == "" {
		http.Error(w, "ticker required", http.StatusBadRequest)
		return
	}
	loc, _ := time.LoadLocation("America/New_York")
	nowET := time.Now().In(loc)
	day := time.Date(nowET.Year(), nowET.Month(), nowET.Day(), 0, 0, 0, 0, loc)
	if raw := strings.TrimSpace(q.Get("date")); raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, loc)
		if err != nil {
			http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		day = parsed
	}
	sessionStart := time.Date(day.Year(), day.Month(), day.Day(), 4, 0, 0, 0, loc)
	sessionEnd := time.Date(day.Year(), day.Month(), day.Day(), 20, 0, 0, 0, loc)
	queryEnd := sessionEnd
	if day.Year() == nowET.Year() && day.YearDay() == nowET.YearDay() && nowET.Before(sessionEnd) {
		queryEnd = nowET
	}
	if queryEnd.Before(sessionStart) {
		queryEnd = sessionStart.Add(15 * time.Minute)
	}
	m15Start := maxTime(sessionStart, queryEnd.Add(-15*time.Minute))
	m60Start := maxTime(sessionStart, queryEnd.Add(-60*time.Minute))
	intradayStart := maxTime(sessionStart, queryEnd.Add(-180*time.Minute))
	leadersStart := maxTime(sessionStart, queryEnd.Add(-90*time.Minute))

	aggregationQuery := func(from, to time.Time) ([]marketauxEntityStat, error) {
		params := url.Values{}
		params.Set("symbols", ticker)
		params.Set("group_by", "symbol")
		params.Set("published_after", marketauxDateTimeUTC(from))
		params.Set("published_before", marketauxDateTimeUTC(to))
		params.Set("limit", "20")
		return queryMarketaux("/v1/entity/stats/aggregation", params)
	}

	type statsResult struct {
		rows []marketauxEntityStat
		err  error
	}
	results := map[string]statsResult{}
	var mu sync.Mutex
	setResult := func(key string, rows []marketauxEntityStat, err error) {
		mu.Lock()
		defer mu.Unlock()
		results[key] = statsResult{rows: rows, err: err}
	}

	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		rows, err := aggregationQuery(m15Start, queryEnd)
		setResult("m15", rows, err)
	}()
	go func() {
		defer wg.Done()
		rows, err := aggregationQuery(m60Start, queryEnd)
		setResult("m60", rows, err)
	}()
	go func() {
		defer wg.Done()
		rows, err := aggregationQuery(sessionStart, queryEnd)
		setResult("session", rows, err)
	}()
	go func() {
		defer wg.Done()
		params := url.Values{}
		params.Set("symbols", ticker)
		params.Set("interval", "minute")
		params.Set("group_by", "symbol")
		params.Set("published_after", marketauxDateTimeUTC(intradayStart))
		params.Set("published_before", marketauxDateTimeUTC(queryEnd))
		rows, err := queryMarketaux("/v1/entity/stats/intraday", params)
		setResult("intraday", rows, err)
	}()
	go func() {
		defer wg.Done()
		params := url.Values{}
		params.Set("countries", "us")
		params.Set("group_by", "symbol")
		params.Set("published_after", marketauxDateTimeUTC(leadersStart))
		params.Set("published_before", marketauxDateTimeUTC(queryEnd))
		params.Set("min_doc_count", "4")
		params.Set("limit", "20")
		rows, err := queryMarketaux("/v1/entity/trending/aggregation", params)
		setResult("leaders", rows, err)
	}()
	wg.Wait()

	var warnings []string
	for key, result := range results {
		if result.err != nil {
			warnings = append(warnings, key+": "+result.err.Error())
		}
	}
	allFailed := true
	for _, result := range results {
		if result.err == nil {
			allFailed = false
			break
		}
	}
	if allFailed {
		out := map[string]interface{}{
			"enabled":      false,
			"provider":     "Marketaux Market Stats",
			"ticker":       ticker,
			"date":         day.Format("2006-01-02"),
			"published_at": time.Now().UTC().Format(time.RFC3339),
			"query_window": map[string]string{
				"published_after":  marketauxDateTimeUTC(sessionStart),
				"published_before": marketauxDateTimeUTC(queryEnd),
			},
			"windows": map[string]interface{}{
				"m15":     statToMap(marketauxEntityStat{Symbol: ticker}),
				"m60":     statToMap(marketauxEntityStat{Symbol: ticker}),
				"session": statToMap(marketauxEntityStat{Symbol: ticker}),
			},
			"radar": map[string]interface{}{
				"pulse_score":        0,
				"confidence":         0,
				"bias":               "NEUTRAL",
				"bias_reason":        "Marketaux data temporarily unavailable.",
				"docs_acceleration":  0,
				"sentiment_shift":    0,
				"trend_score":        0,
				"documents_last_15m": 0,
				"documents_last_60m": 0,
				"documents_session":  0,
				"sentiment_15m":      0,
				"sentiment_60m":      0,
				"sentiment_session":  0,
			},
			"intraday": []map[string]interface{}{},
			"leaders":  []map[string]interface{}{},
			"warnings": warnings,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
		return
	}

	m15Stat := pickStatForSymbol(results["m15"].rows, ticker)
	m60Stat := pickStatForSymbol(results["m60"].rows, ticker)
	sessionStat := pickStatForSymbol(results["session"].rows, ticker)
	trendStat := pickStatForSymbol(results["leaders"].rows, ticker)

	intradayRows := make([]map[string]interface{}, 0, len(results["intraday"].rows))
	for _, stat := range results["intraday"].rows {
		if stat.Symbol != "" && !strings.EqualFold(stat.Symbol, ticker) {
			continue
		}
		intradayRows = append(intradayRows, map[string]interface{}{
			"date":            stat.Date,
			"total_documents": stat.TotalDocuments,
			"sentiment_avg":   roundTo(stat.SentimentAvg, 4),
		})
	}
	sort.Slice(intradayRows, func(i, j int) bool {
		return valueAsString(intradayRows[i]["date"]) < valueAsString(intradayRows[j]["date"])
	})
	if len(intradayRows) > 120 {
		intradayRows = intradayRows[len(intradayRows)-120:]
	}

	leaderMap := make(map[string]marketauxEntityStat)
	for _, stat := range results["leaders"].rows {
		sym := strings.ToUpper(strings.TrimSpace(stat.Symbol))
		if sym == "" {
			continue
		}
		prev, exists := leaderMap[sym]
		if !exists || normalizeTrendScore(stat.Score) > normalizeTrendScore(prev.Score) || stat.TotalDocuments > prev.TotalDocuments {
			leaderMap[sym] = stat
		}
	}
	leaders := make([]marketauxEntityStat, 0, len(leaderMap))
	for _, stat := range leaderMap {
		leaders = append(leaders, stat)
	}
	sort.Slice(leaders, func(i, j int) bool {
		si, sj := normalizeTrendScore(leaders[i].Score), normalizeTrendScore(leaders[j].Score)
		if si == sj {
			return leaders[i].TotalDocuments > leaders[j].TotalDocuments
		}
		return si > sj
	})
	if len(leaders) > 12 {
		leaders = leaders[:12]
	}
	leaderRows := make([]map[string]interface{}, 0, len(leaders))
	for _, stat := range leaders {
		leaderRows = append(leaderRows, map[string]interface{}{
			"symbol":          stat.Symbol,
			"name":            stat.Name,
			"exchange":        stat.Exchange,
			"country":         stat.Country,
			"industry":        stat.Industry,
			"total_documents": stat.TotalDocuments,
			"sentiment_avg":   roundTo(stat.SentimentAvg, 4),
			"score":           roundTo(normalizeTrendScore(stat.Score), 2),
			"why":             buildLeaderReason(stat),
		})
	}

	out := map[string]interface{}{
		"enabled":      true,
		"provider":     "Marketaux Market Stats",
		"ticker":       ticker,
		"date":         day.Format("2006-01-02"),
		"published_at": time.Now().UTC().Format(time.RFC3339),
		"query_window": map[string]string{
			"published_after":  marketauxDateTimeUTC(sessionStart),
			"published_before": marketauxDateTimeUTC(queryEnd),
		},
		"windows": map[string]interface{}{
			"m15":     statToMap(m15Stat),
			"m60":     statToMap(m60Stat),
			"session": statToMap(sessionStat),
		},
		"radar":    buildTickerRadar(m15Stat, m60Stat, sessionStat, trendStat),
		"intraday": intradayRows,
		"leaders":  leaderRows,
		"warnings": warnings,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func getPriorTradingDate(ticker, dateStr string) (string, error) {
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return "", err
	}
	for i := 1; i <= 10; i++ {
		prior := date.AddDate(0, 0, -i)
		for prior.Weekday() == time.Saturday || prior.Weekday() == time.Sunday {
			prior = prior.AddDate(0, 0, -1)
		}
		priorStr := prior.Format("2006-01-02")
		bars, err := queryPolygon(ticker, 1, "minute", priorStr, priorStr)
		if err == nil && len(bars) > 0 {
			return priorStr, nil
		}
	}
	return "", fmt.Errorf("no prior trading day found")
}

func chartHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	date := strings.TrimSpace(q.Get("date"))
	ticker := strings.ToUpper(strings.TrimSpace(q.Get("ticker")))
	timeStr, err := normalizeHHMM(q.Get("time"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	signal := strings.ToLower(strings.TrimSpace(q.Get("signal")))
	if signal == "" {
		signal = "buy"
	}
	if signal != "buy" && signal != "sell" {
		http.Error(w, "signal must be buy or sell", http.StatusBadRequest)
		return
	}
	resolution := strings.ToLower(strings.TrimSpace(q.Get("resolution")))
	if date == "" || ticker == "" {
		http.Error(w, "date and ticker are required", http.StatusBadRequest)
		return
	}
	if !validISODate(date) {
		http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	if resolution == "" {
		resolution = "1m"
	}
	if !validResolution(resolution) {
		http.Error(w, "unsupported resolution", http.StatusBadRequest)
		return
	}
	timeDisplay := timeStr
	if timeDisplay == "" {
		timeDisplay = "FULL DAY"
	}
	w.Header().Set("Content-Type", "text/html")
	html := strings.ReplaceAll(chartHTML, "{{DATE}}", date)
	html = strings.ReplaceAll(html, "{{TICKER}}", ticker)
	html = strings.ReplaceAll(html, "{{TIME}}", timeStr)
	html = strings.ReplaceAll(html, "{{TIME_DISPLAY}}", timeDisplay)
	html = strings.ReplaceAll(html, "{{SIGNAL}}", signal)
	html = strings.ReplaceAll(html, "{{SIGNAL_LABEL}}", strings.ToUpper(signal))
	html = strings.ReplaceAll(html, "{{RESOLUTION}}", resolution)
	html = applySharedTemplateVars(html)
	fmt.Fprint(w, html)
}

func chartDataHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	symbol := strings.ToUpper(strings.TrimSpace(q.Get("ticker")))
	dateStr := strings.TrimSpace(q.Get("date"))
	timeStr, err := normalizeHHMM(q.Get("time"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tf := strings.ToLower(strings.TrimSpace(q.Get("resolution")))
	if symbol == "" || dateStr == "" {
		http.Error(w, "ticker and date required", http.StatusBadRequest)
		return
	}
	if !validISODate(dateStr) {
		http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	if tf == "" {
		tf = "1m"
	}
	if !validResolution(tf) {
		http.Error(w, "unsupported timeframe", http.StatusBadRequest)
		return
	}
	targetHour, targetMinute := 16, 0
	if timeStr != "" {
		targetHour, _ = strconv.Atoi(timeStr[:2])
		targetMinute, _ = strconv.Atoi(timeStr[2:])
	}
	minBars, err := queryPolygon(symbol, 1, "minute", dateStr, dateStr)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	loc, _ := time.LoadLocation("America/New_York")
	var lastClose float64
	var lastTime int64
	extendedCandles := make([]candlePoint, 0)
	extendedVolume := make([]linePoint, 0)
	for _, b := range minBars {
		ts := time.UnixMilli(b.T).In(loc)
		h, m := ts.Hour(), ts.Minute()
		if h < 7 || h >= 16 {
			continue
		}
		if h > targetHour || (h == targetHour && m > targetMinute) {
			break
		}
		extendedCandles = append(extendedCandles, candlePoint{
			Time:  b.T / 1000,
			Open:  b.O,
			High:  b.H,
			Low:   b.L,
			Close: b.C,
		})
		extendedVolume = append(extendedVolume, linePoint{
			Time:  b.T / 1000,
			Value: b.V,
		})
		lastClose = b.C
		lastTime = b.T
	}
	if len(extendedCandles) > 0 {
		extendTime := time.UnixMilli(lastTime).In(loc).Add(time.Minute)
		for extendTime.Hour() < 16 {
			if extendTime.Hour() >= 7 {
				extendedCandles = append(extendedCandles, candlePoint{
					Time:  extendTime.Unix(),
					Open:  lastClose,
					High:  lastClose,
					Low:   lastClose,
					Close: lastClose,
				})
				extendedVolume = append(extendedVolume, linePoint{
					Time:  extendTime.Unix(),
					Value: 0,
				})
			}
			extendTime = extendTime.Add(time.Minute)
		}
	}
	var candles []candlePoint
	var vol []linePoint
	if tf == "1m" {
		candles = extendedCandles
		vol = extendedVolume
	} else {
		var mult int
		switch tf {
		case "2m":
			mult = 2
		case "3m":
			mult = 3
		case "5m":
			mult = 5
		case "10m":
			mult = 10
		case "15m":
			mult = 15
		case "30m":
			mult = 30
		case "1h":
			mult = 60
		default:
			http.Error(w, "unsupported timeframe", 400)
			return
		}
		for i := 0; i < len(extendedCandles); i += mult {
			end := i + mult
			if end > len(extendedCandles) {
				end = len(extendedCandles)
			}
			group := extendedCandles[i:end]
			if len(group) == 0 {
				break
			}
			o := group[0].Open
			h := group[0].High
			l := group[0].Low
			c := group[len(group)-1].Close
			v := 0.0
			for j := i; j < end && j < len(extendedVolume); j++ {
				v += extendedVolume[j].Value
			}
			for _, g := range group {
				if g.High > h {
					h = g.High
				}
				if g.Low < l {
					l = g.Low
				}
			}
			candles = append(candles, candlePoint{
				Time:  group[0].Time,
				Open:  o,
				High:  h,
				Low:   l,
				Close: c,
			})
			vol = append(vol, linePoint{Time: group[0].Time, Value: v})
		}
	}
	var cumPV, cumV float64
	vwap := make([]linePoint, 0)
	for i, c := range extendedCandles {
		if i < len(extendedVolume) {
			typ := (c.High + c.Low + c.Close) / 3
			cumPV += typ * extendedVolume[i].Value
			cumV += extendedVolume[i].Value
			if cumV > 0 {
				vwap = append(vwap, linePoint{Time: c.Time, Value: cumPV / cumV})
			}
		}
	}
	var sum float64
	sma := make([]linePoint, 0)
	for i, c := range candles {
		sum += c.Close
		if i >= 9 {
			sum -= candles[i-9].Close
		}
		if i >= 8 {
			sma = append(sma, linePoint{Time: c.Time, Value: sum / 9})
		}
	}
	cci := calculateCCI(candles, 27)
	priorTradingDate, _ := getPriorTradingDate(symbol, dateStr)
	priorClose, priorVolume := getPriorDayData(symbol, dateStr)
	metrics := calculateMetrics(extendedCandles, extendedVolume, priorClose, priorVolume)
	includeExtras := strings.ToLower(strings.TrimSpace(q.Get("includeExtras"))) != "false"
	uniqueNews := []NewsItem{}
	var allFilings []map[string]interface{}
	var profileData map[string]interface{}
	if includeExtras {
		var allNewsItems []NewsItem
		// Fetch current day
		pNews, _ := queryMassiveBenzingaNews(symbol, dateStr)
		allNewsItems = appendMassiveNewsItems(allNewsItems, pNews)
		fNews, _ := queryFMPNews(symbol, dateStr)
		for _, f := range fNews {
			val, exists := f["title"]
			if !exists || val == nil {
				continue
			}
			title, ok := val.(string)
			if !ok {
				continue
			}
			val, exists = f["site"]
			if !exists || val == nil {
				continue
			}
			site, ok := val.(string)
			if !ok {
				continue
			}
			val, exists = f["url"]
			if !exists || val == nil {
				continue
			}
			url, ok := val.(string)
			if !ok {
				continue
			}
			val, exists = f["publishedDate"]
			if !exists || val == nil {
				continue
			}
			pub, ok := val.(string)
			if !ok {
				continue
			}
			allNewsItems = append(allNewsItems, NewsItem{Title: title, Source: site, URL: url, Published: normalizePublished(pub)})
		}
		filings, _ := querySECFilings(symbol, dateStr)
		allFilings = append(allFilings, filings...)
		// Fetch prior day
		priorStr, err := getPriorTradingDate(symbol, dateStr)
		if err == nil {
			pNewsPrior, _ := queryMassiveBenzingaNews(symbol, priorStr)
			allNewsItems = appendMassiveNewsItems(allNewsItems, pNewsPrior)
			fNewsPrior, _ := queryFMPNews(symbol, priorStr)
			for _, f := range fNewsPrior {
				val, exists := f["title"]
				if !exists || val == nil {
					continue
				}
				title, ok := val.(string)
				if !ok {
					continue
				}
				val, exists = f["site"]
				if !exists || val == nil {
					continue
				}
				site, ok := val.(string)
				if !ok {
					continue
				}
				val, exists = f["url"]
				if !exists || val == nil {
					continue
				}
				url, ok := val.(string)
				if !ok {
					continue
				}
				val, exists = f["publishedDate"]
				if !exists || val == nil {
					continue
				}
				pub, ok := val.(string)
				if !ok {
					continue
				}
				allNewsItems = append(allNewsItems, NewsItem{Title: title, Source: site, URL: url, Published: normalizePublished(pub)})
			}
			filingsPrior, _ := querySECFilings(symbol, priorStr)
			allFilings = append(allFilings, filingsPrior...)
		}
		// Deduplicate and sort news
		allNewsItems = filterTradeableNews(allNewsItems)
		uniqueMap := make(map[string]NewsItem)
		for _, n := range allNewsItems {
			uniqueMap[n.URL] = n
		}
		for _, n := range uniqueMap {
			uniqueNews = append(uniqueNews, n)
		}
		sort.Slice(uniqueNews, func(i, j int) bool {
			return isNewsNewer(uniqueNews[i], uniqueNews[j])
		})
		// Sort filings
		sort.Slice(allFilings, func(i, j int) bool {
			ti := parsePublished(allFilings[i]["filedAt"].(string))
			tj := parsePublished(allFilings[j]["filedAt"].(string))
			return ti.After(tj)
		})
		var profile []map[string]interface{}
		profile, _ = queryFMPProfile(symbol)
		if len(profile) > 0 {
			profileData = profile[0]
		}
	}
	out := map[string]interface{}{
		"candles":            candles,
		"volume":             vol,
		"minCandles":         extendedCandles,
		"minVolume":          extendedVolume,
		"vwap":               vwap,
		"sma":                sma,
		"cci":                cci,
		"metrics":            metrics,
		"prior_trading_date": priorTradingDate,
		"news":               uniqueNews,
		"filings":            allFilings,
		"profile":            profileData,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func getPriorDayData(ticker, dateStr string) (float64, float64) {
	date, _ := time.Parse("2006-01-02", dateStr)
	loc, _ := time.LoadLocation("America/New_York")
	for i := 0; i < 10; i++ {
		date = date.AddDate(0, 0, -1)
		if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
			continue
		}
		priorDateStr := date.Format("2006-01-02")
		bars, err := queryPolygon(ticker, 1, "minute", priorDateStr, priorDateStr)
		if err != nil || len(bars) == 0 {
			continue
		}
		var lastClose float64
		var totalVolume float64
		for _, b := range bars {
			ts := time.UnixMilli(b.T).In(loc)
			h, m := ts.Hour(), ts.Minute()
			if h >= 9 && (h > 9 || m >= 30) && h < 16 {
				lastClose = b.C
				totalVolume += b.V
			}
		}
		if lastClose > 0 {
			return lastClose, totalVolume
		}
	}
	return 0, 0
}

func calculateMetrics(candles []candlePoint, volumes []linePoint, priorClose, priorVolume float64) map[string]string {
	if len(candles) == 0 {
		return map[string]string{}
	}
	open := candles[0].Open
	close := candles[len(candles)-1].Close
	high := candles[0].High
	low := candles[0].Low
	for _, c := range candles {
		if c.High > high {
			high = c.High
		}
		if c.Low < low {
			low = c.Low
		}
	}
	totalVolume := 0.0
	for _, v := range volumes {
		totalVolume += v.Value
	}
	metrics := map[string]string{
		"open_price":  fmt.Sprintf("%.2f", open),
		"close_price": fmt.Sprintf("%.2f", close),
		"high_of_day": fmt.Sprintf("%.2f", high),
		"low_of_day":  fmt.Sprintf("%.2f", low),
		"volume":      fmt.Sprintf("%.0f", totalVolume),
	}
	if open > 0 {
		percentGain := (close - open) / open * 100
		maxSpikingUp := (high - open) / open * 100
		maxSpikingDown := (open - low) / open * 100
		metrics["percent_gain_eod"] = fmt.Sprintf("%.2f", percentGain)
		metrics["max_spiking_up_percent"] = fmt.Sprintf("%.2f", maxSpikingUp)
		metrics["max_spiking_down_percent"] = fmt.Sprintf("%.2f", maxSpikingDown)
	}
	if priorClose > 0 {
		gap := (open - priorClose) / priorClose * 100
		metrics["percent_gap_from_prior"] = fmt.Sprintf("%.2f", gap)
	}
	if priorVolume > 0 {
		metrics["prior_day_volume"] = fmt.Sprintf("%.0f", priorVolume)
	}
	return metrics
}

func main() {
	_ = godotenv.Load()
	flag.Parse()
	polygonAPIKey = *apiKeyFlag
	if polygonAPIKey == "" {
		polygonAPIKey = os.Getenv("POLYGON_API_KEY")
	}
	if polygonAPIKey == "" {
		log.Fatal("Polygon API key missing")
	}
	fmpAPIKey = os.Getenv("FMP_API_KEY")
	if fmpAPIKey == "" {
		log.Fatal("FMP API key missing")
	}
	secAPIKey = os.Getenv("SEC_API_KEY")
	if secAPIKey == "" {
		log.Fatal("SEC API key missing")
	}
	marketauxAPIKey = strings.TrimSpace(os.Getenv("MARKETAUX_API_KEY"))
	if marketauxAPIKey == "" {
		marketauxAPIKey = strings.TrimSpace(os.Getenv("MARKETAUX_API_TOKEN"))
	}
	marketauxEnabled = marketauxAPIKey != ""
	appCfg := loadAppConfig("config.yaml")
	ntfyEnabled = envBool("NTFY_ENABLED")
	ntfyServer = normalizeNtfyServer(os.Getenv("NTFY_SERVER"))
	ntfyTopic = strings.TrimSpace(os.Getenv("NTFY_TOPIC"))
	if ntfyTopic == "" {
		ntfyTopic = "hello"
	}
	ntfyTagOptions = cleanStringSlice(appCfg.Ntfy.Tags)
	if len(ntfyTagOptions) == 0 {
		ntfyTagOptions = append([]string(nil), defaultNtfyTagOptions...)
	}
	patternfolioURL = os.Getenv("PATTERNFOLIO_URL")
	if patternfolioURL == "" {
		patternfolioURL = "http://localhost:8082"
	}
	if *portFlag != 0 {
		listenPort = *portFlag
	} else if p := os.Getenv("PORT"); p != "" {
		fmt.Sscanf(p, "%d", &listenPort)
	}
	if listenPort == 0 {
		listenPort = 8081
	}
	http.HandleFunc("/", rootHandler)
	http.HandleFunc("/api/candles", candlesHandler)
	http.HandleFunc("/api/ticker-details", tickerDetailsHandler)
	http.HandleFunc("/api/share-float", shareFloatHandler)
	http.HandleFunc("/api/profile", profileHandler)
	http.HandleFunc("/api/extra", extraHandler)
	http.HandleFunc("/api/market-stats", marketStatsHandler)
	http.HandleFunc("/chart", chartHandler)
	http.HandleFunc("/api/open-chart", openChartHandler)
	http.HandleFunc("/api/open-chart/", openChartHandler)
	http.HandleFunc("/api/chart-data", chartDataHandler)
	http.HandleFunc("/api/ntfy/publish", ntfyPublishHandler)
	addr := fmt.Sprintf(":%d", listenPort)
	go func() {
		time.Sleep(500 * time.Millisecond)
		openBrowser("http://localhost" + addr)
	}()
	log.Printf("Serving on http://localhost%s …", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
