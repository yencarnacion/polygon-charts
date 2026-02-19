// main.go

package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

var (
	apiKeyFlag = flag.String("apikey", "", "Polygon.io API key (overrides .env)")
	portFlag   = flag.Int("port", 0, "HTTP port (overrides .env)")
)

var (
	polygonAPIKey   string
	fmpAPIKey       string
	secAPIKey       string
	patternfolioURL string
	listenPort      int
)

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

func queryPolygon(sym string, mult int, span, from, to string) ([]polygonBar, error) {
	url := fmt.Sprintf(
		"https://api.polygon.io/v2/aggs/ticker/%s/range/%d/%s/%s/%s?adjusted=true&sort=asc&apiKey=%s",
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
	url := fmt.Sprintf("https://api.polygon.io/v3/reference/tickers/%s?apiKey=%s", ticker, polygonAPIKey)
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

func queryPolygonNews(ticker, dateStr string) ([]map[string]interface{}, error) {
	nextD := nextDay(dateStr)
	url := fmt.Sprintf("https://api.polygon.io/v2/reference/news?ticker=%s&published_utc.gte=%s&published_utc.lt=%s&limit=50&sort=published_utc.desc&apiKey=%s",
		ticker, dateStr, nextD, polygonAPIKey)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Polygon News: %s", resp.Status)
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
	if strings.Contains(s, "T") {
		t, _ := time.Parse(time.RFC3339, s)
		return t
	} else {
		t, _ := time.Parse("2006-01-02 15:04:05", s)
		return t
	}
}

func openBrowser(u string) {
	if err := exec.Command("google-chrome", "--new-tab", u).Start(); err != nil {
		_ = exec.Command("xdg-open", u).Start()
	}
}

func rootHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	html := indexHTML
	html = strings.ReplaceAll(html, "{{PATTERNFOLIO_URL}}", patternfolioURL)
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
		minBars, minErr = queryPolygon(symbol, 1, "minute", from, to)
		if minErr != nil {
			http.Error(w, minErr.Error(), 502)
			return
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
	out := payload{Candles: candles, Volume: vol, MinCandles: minCandles, MinVolume: minVolume, VWAP: vwap, SMA9: sma}
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
	// Fetch for current day
	pNews, _ := queryPolygonNews(ticker, dateStr)
	for _, p := range pNews {
		val, exists := p["title"]
		if !exists || val == nil {
			continue
		}
		title, ok := val.(string)
		if !ok {
			continue
		}
		val, exists = p["url"]
		if !exists || val == nil {
			continue
		}
		url, ok := val.(string)
		if !ok {
			continue
		}
		val, exists = p["published_utc"]
		if !exists || val == nil {
			continue
		}
		pub, ok := val.(string)
		if !ok {
			continue
		}
		source := ""
		if publisher, ok := p["publisher"].(map[string]interface{}); ok {
			if name, ok := publisher["name"].(string); ok {
				source = name
			}
		}
		allNewsItems = append(allNewsItems, NewsItem{
			Title:     title,
			Source:    source,
			URL:       url,
			Published: pub,
		})
	}
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
			Published: pub,
		})
	}
	filings, _ := querySECFilings(ticker, dateStr)
	allFilings = append(allFilings, filings...)
	profile, _ = queryFMPProfile(ticker)
	// Fetch for prior day if days=2
	if days == 2 {
		priorStr, err := getPriorTradingDate(ticker, dateStr)
		if err == nil {
			pNews, _ = queryPolygonNews(ticker, priorStr)
			for _, p := range pNews {
				val, exists := p["title"]
				if !exists || val == nil {
					continue
				}
				title, ok := val.(string)
				if !ok {
					continue
				}
				val, exists = p["url"]
				if !exists || val == nil {
					continue
				}
				url, ok := val.(string)
				if !ok {
					continue
				}
				val, exists = p["published_utc"]
				if !exists || val == nil {
					continue
				}
				pub, ok := val.(string)
				if !ok {
					continue
				}
				source := ""
				if publisher, ok := p["publisher"].(map[string]interface{}); ok {
					if name, ok := publisher["name"].(string); ok {
						source = name
					}
				}
				allNewsItems = append(allNewsItems, NewsItem{
					Title:     title,
					Source:    source,
					URL:       url,
					Published: pub,
				})
			}
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
					Published: pub,
				})
			}
			filings, _ := querySECFilings(ticker, priorStr)
			allFilings = append(allFilings, filings...)
		} else {
			log.Println("Could not find prior trading date:", err)
		}
	}
	// Deduplicate news by URL
	uniqueMap := make(map[string]NewsItem)
	for _, n := range allNewsItems {
		uniqueMap[n.URL] = n
	}
	uniqueNews := []NewsItem{}
	for _, n := range uniqueMap {
		uniqueNews = append(uniqueNews, n)
	}
	sort.Slice(uniqueNews, func(i, j int) bool {
		ti := parsePublished(uniqueNews[i].Published)
		tj := parsePublished(uniqueNews[j].Published)
		return ti.After(tj)
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
	html = strings.ReplaceAll(html, "{{PATTERNFOLIO_URL}}", patternfolioURL)
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
	if len(extendedCandles) > 0 && targetHour < 16 {
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
	priorClose, priorVolume := getPriorDayData(symbol, dateStr)
	metrics := calculateMetrics(extendedCandles, extendedVolume, priorClose, priorVolume)
	var allNewsItems []NewsItem
	var allFilings []map[string]interface{}
	// Fetch current day
	pNews, _ := queryPolygonNews(symbol, dateStr)
	for _, p := range pNews {
		val, exists := p["title"]
		if !exists || val == nil {
			continue
		}
		title, ok := val.(string)
		if !ok {
			continue
		}
		val, exists = p["url"]
		if !exists || val == nil {
			continue
		}
		url, ok := val.(string)
		if !ok {
			continue
		}
		val, exists = p["published_utc"]
		if !exists || val == nil {
			continue
		}
		pub, ok := val.(string)
		if !ok {
			continue
		}
		source := ""
		if publisher, ok := p["publisher"].(map[string]interface{}); ok {
			if name, ok := publisher["name"].(string); ok {
				source = name
			}
		}
		allNewsItems = append(allNewsItems, NewsItem{Title: title, Source: source, URL: url, Published: pub})
	}
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
		allNewsItems = append(allNewsItems, NewsItem{Title: title, Source: site, URL: url, Published: pub})
	}
	filings, _ := querySECFilings(symbol, dateStr)
	allFilings = append(allFilings, filings...)
	// Fetch prior day
	priorStr, err := getPriorTradingDate(symbol, dateStr)
	if err == nil {
		pNewsPrior, _ := queryPolygonNews(symbol, priorStr)
		for _, p := range pNewsPrior {
			val, exists := p["title"]
			if !exists || val == nil {
				continue
			}
			title, ok := val.(string)
			if !ok {
				continue
			}
			val, exists = p["url"]
			if !exists || val == nil {
				continue
			}
			url, ok := val.(string)
			if !ok {
				continue
			}
			val, exists = p["published_utc"]
			if !exists || val == nil {
				continue
			}
			pub, ok := val.(string)
			if !ok {
				continue
			}
			source := ""
			if publisher, ok := p["publisher"].(map[string]interface{}); ok {
				if name, ok := publisher["name"].(string); ok {
					source = name
				}
			}
			allNewsItems = append(allNewsItems, NewsItem{Title: title, Source: source, URL: url, Published: pub})
		}
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
			allNewsItems = append(allNewsItems, NewsItem{Title: title, Source: site, URL: url, Published: pub})
		}
		filingsPrior, _ := querySECFilings(symbol, priorStr)
		allFilings = append(allFilings, filingsPrior...)
	}
	// Deduplicate and sort news
	uniqueMap := make(map[string]NewsItem)
	for _, n := range allNewsItems {
		uniqueMap[n.URL] = n
	}
	uniqueNews := []NewsItem{}
	for _, n := range uniqueMap {
		uniqueNews = append(uniqueNews, n)
	}
	sort.Slice(uniqueNews, func(i, j int) bool {
		ti := parsePublished(uniqueNews[i].Published)
		tj := parsePublished(uniqueNews[j].Published)
		return ti.After(tj)
	})
	// Sort filings
	sort.Slice(allFilings, func(i, j int) bool {
		ti := parsePublished(allFilings[i]["filedAt"].(string))
		tj := parsePublished(allFilings[j]["filedAt"].(string))
		return ti.After(tj)
	})
	var profile []map[string]interface{}
	profile, _ = queryFMPProfile(symbol)
	var profileData map[string]interface{}
	if len(profile) > 0 {
		profileData = profile[0]
	}
	out := map[string]interface{}{
		"candles":    candles,
		"volume":     vol,
		"minCandles": extendedCandles,
		"minVolume":  extendedVolume,
		"vwap":       vwap,
		"sma":        sma,
		"metrics":    metrics,
		"news":       uniqueNews,
		"filings":    allFilings,
		"profile":    profileData,
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
		maxSpikingDown := (low - open) / open * 100
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
	http.HandleFunc("/api/extra", extraHandler)
	http.HandleFunc("/chart", chartHandler)
	http.HandleFunc("/api/open-chart", openChartHandler)
	http.HandleFunc("/api/open-chart/", openChartHandler)
	http.HandleFunc("/api/chart-data", chartDataHandler)
	addr := fmt.Sprintf(":%d", listenPort)
	go func() {
		time.Sleep(500 * time.Millisecond)
		openBrowser("http://localhost" + addr)
	}()
	log.Printf("Serving on http://localhost%s …", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
