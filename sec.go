package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	secProvider  = "edgar"
	secUserAgent string
	edgarClient  = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	// Five requests/second leaves headroom below SEC's aggregate ten/second limit.
	edgarRequestInterval   = 200 * time.Millisecond
	edgarRateMu            sync.Mutex
	edgarNextRequest       time.Time
	edgarFetchGate         = make(chan struct{}, 1)
	edgarAccessionPattern  = regexp.MustCompile(`^\d{10}-\d{2}-\d{6}$`)
	edgarHistoryPattern    = regexp.MustCompile(`^CIK\d{10}-submissions-\d+\.json$`)
	edgarAcceptancePattern = regexp.MustCompile(`(?i)<ACCEPTANCE-DATETIME>\s*(\d{14})`)
)

func configureSECProvider() error {
	secProvider = strings.ToLower(strings.TrimSpace(os.Getenv("SEC_FILINGS_PROVIDER")))
	if secProvider == "" {
		secProvider = "edgar"
	}
	secAPIKey = strings.TrimSpace(os.Getenv("SEC_API_KEY"))
	secUserAgent = strings.TrimSpace(os.Getenv("SEC_USER_AGENT"))
	if strings.ContainsAny(secUserAgent, "\r\n") {
		return fmt.Errorf("SEC_USER_AGENT must be a single-line app name and contact email")
	}
	switch secProvider {
	case "edgar":
		// Missing identification disables filing requests, not the chart application.
		return nil
	case "sec-api":
		if secAPIKey == "" {
			return fmt.Errorf("SEC_API_KEY is required when SEC_FILINGS_PROVIDER=sec-api")
		}
		return nil
	default:
		return fmt.Errorf("SEC_FILINGS_PROVIDER must be edgar or sec-api")
	}
}

func querySECFilings(ticker, dateStr string) ([]map[string]interface{}, error) {
	return querySECFilingsContext(context.Background(), ticker, dateStr)
}

func querySECFilingsContext(ctx context.Context, ticker, dateStr string) ([]map[string]interface{}, error) {
	ticker = strings.ToUpper(strings.TrimSpace(ticker))
	if !tickerPattern.MatchString(ticker) || !validISODate(dateStr) {
		return nil, fmt.Errorf("invalid SEC ticker or date")
	}
	switch secProvider {
	case "sec-api":
		return querySECAPIFilings(ticker, dateStr)
	case "", "edgar":
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return queryEDGARFilings(ctx, ticker, dateStr)
	default:
		return nil, fmt.Errorf("unknown SEC filings provider")
	}
}

type edgarTicker struct {
	CIK    int64  `json:"cik_str"`
	Ticker string `json:"ticker"`
	Title  string `json:"title"`
}

type edgarRows struct {
	AccessionNumber       []string `json:"accessionNumber"`
	FilingDate            []string `json:"filingDate"`
	AcceptanceDateTime    []string `json:"acceptanceDateTime"`
	Form                  []string `json:"form"`
	PrimaryDocument       []string `json:"primaryDocument"`
	PrimaryDocDescription []string `json:"primaryDocDescription"`
	Items                 []string `json:"items"`
}

type edgarSubmissions struct {
	Name    string `json:"name"`
	Filings struct {
		Recent edgarRows `json:"recent"`
		Files  []struct {
			Name       string `json:"name"`
			FilingFrom string `json:"filingFrom"`
			FilingTo   string `json:"filingTo"`
		} `json:"files"`
	} `json:"filings"`
}

func edgarWait(ctx context.Context) error {
	edgarRateMu.Lock()
	now := time.Now()
	start := now
	if edgarNextRequest.After(start) {
		start = edgarNextRequest
	}
	edgarNextRequest = start.Add(edgarRequestInterval)
	edgarRateMu.Unlock()
	timer := time.NewTimer(time.Until(start))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func edgarGet(ctx context.Context, endpoint string) ([]byte, error) {
	if secUserAgent == "" {
		return nil, fmt.Errorf("SEC_USER_AGENT is required for EDGAR requests")
	}
	if err := edgarWait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid EDGAR request")
	}
	req.Header.Set("User-Agent", secUserAgent)
	req.Header.Set("Accept", "application/json, text/html;q=0.9")
	resp, err := edgarClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("EDGAR request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Honor a brief backoff on throttling without retrying every open chart.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
			edgarRateMu.Lock()
			until := time.Now().Add(5 * time.Second)
			if until.After(edgarNextRequest) {
				edgarNextRequest = until
			}
			edgarRateMu.Unlock()
		}
		return nil, fmt.Errorf("EDGAR status %d", resp.StatusCode)
	}
	const maxBody = 16 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		return nil, fmt.Errorf("EDGAR unreadable or oversized response")
	}
	return raw, nil
}

// decode validates both fresh responses and cached data before they are used.
// Stale data is returned together with an error so the UI can label incomplete coverage.
func edgarCached(ctx context.Context, cachePath, endpoint string, ttl time.Duration, decode func([]byte) error) error {
	if raw, _, fresh := readJSONCache(cachePath, ttl, time.Now()); fresh && decode(raw) == nil {
		return nil
	}
	select {
	case edgarFetchGate <- struct{}{}:
		defer func() { <-edgarFetchGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	// Coalesce concurrent chart requests after the first has populated the disk cache.
	cached, hasCached, fresh := readJSONCache(cachePath, ttl, time.Now())
	if fresh && decode(cached) == nil {
		return nil
	}
	raw, err := edgarGet(ctx, endpoint)
	if err == nil {
		err = decode(raw)
		if err == nil {
			writeJSONCache(cachePath, raw, false, time.Now())
			return nil
		}
	}
	if hasCached && decode(cached) == nil {
		return fmt.Errorf("EDGAR cached data may be stale: %w", err)
	}
	return err
}

func edgarResolveTicker(ctx context.Context, ticker string) (edgarTicker, error) {
	var entries map[string]edgarTicker
	err := edgarCached(ctx, jsonCachePath("edgar_tickers_v1"), "https://www.sec.gov/files/company_tickers.json", 24*time.Hour, func(raw []byte) error {
		var decoded map[string]edgarTicker
		if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded) == 0 {
			return fmt.Errorf("invalid EDGAR ticker directory")
		}
		entries = decoded
		return nil
	})
	// SEC uses hyphens for share classes; also accept chart symbols such as BRK.B.
	normalized := strings.ReplaceAll(ticker, ".", "-")
	for _, entry := range entries {
		if (strings.EqualFold(entry.Ticker, ticker) || strings.EqualFold(entry.Ticker, normalized)) && entry.CIK > 0 && entry.CIK < 10000000000 {
			return entry, err
		}
	}
	if err != nil {
		return edgarTicker{}, err
	}
	return edgarTicker{}, fmt.Errorf("EDGAR has no CIK mapping for %s", ticker)
}

func edgarRowValue(values []string, index int) string {
	if index >= len(values) {
		return ""
	}
	return values[index]
}

// SEC submissions timestamps have had inconsistent UTC offsets. Use the original
// SEC header's Eastern acceptance time for intraday grouping and replay instead.
func edgarAcceptedAt(ctx context.Context, base, accession string) (time.Time, error) {
	cachePath := jsonCachePath("edgar_acceptance_v1", accession)
	readCachedTime := func() (time.Time, bool) {
		raw, _, fresh := readJSONCache(cachePath, 24*time.Hour, time.Now())
		var value string
		if json.Unmarshal(raw, &value) == nil {
			if t, err := time.Parse(time.RFC3339, value); err == nil {
				return t, fresh
			}
		}
		return time.Time{}, false
	}
	if accepted, fresh := readCachedTime(); fresh {
		return accepted, nil
	}
	select {
	case edgarFetchGate <- struct{}{}:
		defer func() { <-edgarFetchGate }()
	case <-ctx.Done():
		return time.Time{}, ctx.Err()
	}
	cached, fresh := readCachedTime()
	if fresh {
		return cached, nil
	}
	failed := func(err error) (time.Time, error) {
		// A previously verified time remains useful when a refresh fails. Keep
		// the error as well, so incomplete/stale coverage is visible in the UI.
		return cached, err
	}
	raw, err := edgarGet(ctx, base+accession+"-index-headers.html")
	if err != nil {
		return failed(err)
	}
	match := edgarAcceptancePattern.FindStringSubmatch(html.UnescapeString(string(raw)))
	if len(match) != 2 {
		return failed(fmt.Errorf("EDGAR acceptance time unavailable for %s", accession))
	}
	accepted, err := time.ParseInLocation("20060102150405", match[1], marketNewsLocation)
	if err != nil {
		return failed(fmt.Errorf("invalid EDGAR acceptance time"))
	}
	encoded, _ := json.Marshal(accepted.Format(time.RFC3339))
	writeJSONCache(cachePath, encoded, false, time.Now())
	return accepted, nil
}

func queryEDGARFilings(ctx context.Context, ticker, dateStr string) ([]map[string]interface{}, error) {
	if secUserAgent == "" {
		return nil, fmt.Errorf("set SEC_USER_AGENT to your app name and contact email for free EDGAR access")
	}
	company, mappingErr := edgarResolveTicker(ctx, ticker)
	if company.CIK == 0 {
		return nil, mappingErr
	}
	cik := strconv.FormatInt(company.CIK, 10)
	paddedCIK := fmt.Sprintf("CIK%010d", company.CIK)
	var submissions edgarSubmissions
	err := edgarCached(ctx, jsonCachePath("edgar_submissions_v1", paddedCIK), "https://data.sec.gov/submissions/"+paddedCIK+".json", 60*time.Second, func(raw []byte) error {
		var decoded edgarSubmissions
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Filings.Recent.AccessionNumber == nil {
			return fmt.Errorf("invalid EDGAR submissions response")
		}
		submissions = decoded
		return nil
	})
	coverageErr := errors.Join(mappingErr, err)
	if submissions.Filings.Recent.AccessionNumber == nil {
		return nil, coverageErr
	}
	day, _ := time.ParseInLocation("2006-01-02", dateStr, marketNewsLocation)
	// Filing dates can move to the next business day after acceptance. Include a
	// week beyond the requested day to cover weekends and holidays, then filter
	// using the verified header timestamp. A one-day margin covers UTC offsets.
	low := day.AddDate(0, 0, -1).Format("2006-01-02")
	high := day.AddDate(0, 0, 7).Format("2006-01-02")
	groups := []edgarRows{submissions.Filings.Recent}
	for _, file := range submissions.Filings.Files {
		if file.FilingFrom > high || file.FilingTo < low {
			continue
		}
		if !edgarHistoryPattern.MatchString(file.Name) || !strings.HasPrefix(file.Name, paddedCIK+"-") {
			coverageErr = errors.Join(coverageErr, fmt.Errorf("invalid EDGAR history filename"))
			continue
		}
		var rows edgarRows
		err := edgarCached(ctx, jsonCachePath("edgar_history_v1", file.Name), "https://data.sec.gov/submissions/"+file.Name, 24*time.Hour, func(raw []byte) error {
			var decoded edgarRows
			if err := json.Unmarshal(raw, &decoded); err != nil || decoded.AccessionNumber == nil {
				return fmt.Errorf("invalid EDGAR history response")
			}
			rows = decoded
			return nil
		})
		coverageErr = errors.Join(coverageErr, err)
		groups = append(groups, rows)
	}
	companyName := submissions.Name
	if companyName == "" {
		companyName = company.Title
	}
	out := []map[string]interface{}{}
	seen := map[string]bool{}
	for _, rows := range groups {
		for i, accession := range rows.AccessionNumber {
			filed := edgarRowValue(rows.FilingDate, i)
			hint := edgarRowValue(rows.AcceptanceDateTime, i)
			form := strings.TrimSpace(edgarRowValue(rows.Form, i))
			if form == "" || (filed == "" && hint == "") {
				coverageErr = errors.Join(coverageErr, fmt.Errorf("incomplete EDGAR filing metadata"))
				continue
			}
			if len(hint) >= 10 {
				hint = hint[:10]
			}
			if !(filed >= low && filed <= high) && !(hint >= low && hint <= high) {
				continue
			}
			if seen[accession] {
				continue
			}
			seen[accession] = true
			if !edgarAccessionPattern.MatchString(accession) {
				coverageErr = errors.Join(coverageErr, fmt.Errorf("invalid EDGAR accession"))
				continue
			}
			base := "https://www.sec.gov/Archives/edgar/data/" + cik + "/" + strings.ReplaceAll(accession, "-", "") + "/"
			accepted, err := edgarAcceptedAt(ctx, base, accession)
			if err != nil {
				coverageErr = errors.Join(coverageErr, err)
			}
			if accepted.IsZero() {
				continue // Never introduce guessed timestamps into historical replay.
			}
			if nyDateString(accepted) != dateStr {
				continue
			}
			description := strings.TrimSpace(edgarRowValue(rows.PrimaryDocDescription, i))
			if description == "" {
				description = form + " filing"
			}
			link := base + accession + "-index.html"
			if document := edgarRowValue(rows.PrimaryDocument, i); document != "" {
				parts := strings.Split(document, "/")
				safe := true
				for j, part := range parts {
					if part == "" || part == "." || part == ".." {
						safe = false
					}
					parts[j] = url.PathEscape(part)
				}
				if safe {
					link = base + strings.Join(parts, "/")
				}
			}
			out = append(out, map[string]interface{}{
				"id": accession, "accessionNo": accession, "cik": cik,
				"ticker": ticker, "companyName": companyName,
				"filedAt": accepted.Format(time.RFC3339), "formType": form,
				"description": description, "linkToFilingDetails": link,
				"linkToHtml": link, "source": "SEC EDGAR",
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return parsePublished(out[i]["filedAt"].(string)).After(parsePublished(out[j]["filedAt"].(string)))
	})
	return out, coverageErr
}
