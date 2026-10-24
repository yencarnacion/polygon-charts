package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SEC tests exercise the provider through its public query contract. No network
// request is permitted except the explicitly enabled live smoke test below.
func isolateSEC(t *testing.T, transport rtprTransport) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	oldProvider, oldAgent, oldKey := secProvider, secUserAgent, secAPIKey
	oldClient, oldInterval := edgarClient, edgarRequestInterval
	oldDefaultClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: rtprTransport(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("unexpected non-EDGAR request to %s", r.URL.Host)
	})}
	edgarRateMu.Lock()
	oldNextRequest := edgarNextRequest
	edgarNextRequest = time.Time{}
	edgarRateMu.Unlock()
	secProvider, secUserAgent, secAPIKey = "edgar", "Polygon Charts test@example.com", ""
	edgarClient = &http.Client{Transport: transport}
	edgarRequestInterval = 0
	t.Cleanup(func() {
		secProvider, secUserAgent, secAPIKey = oldProvider, oldAgent, oldKey
		edgarClient, edgarRequestInterval = oldClient, oldInterval
		http.DefaultClient = oldDefaultClient
		edgarRateMu.Lock()
		edgarNextRequest = oldNextRequest
		edgarRateMu.Unlock()
		_ = os.Chdir(cwd)
	})
}

type secFixtureRow struct {
	accession, form, filingDate, accepted, document, description string
}

func secFixtureColumns(rows ...secFixtureRow) map[string][]string {
	columns := map[string][]string{}
	for _, name := range []string{"accessionNumber", "form", "filingDate", "acceptanceDateTime", "primaryDocument", "primaryDocDescription"} {
		columns[name] = []string{}
	}
	for _, r := range rows {
		columns["accessionNumber"] = append(columns["accessionNumber"], r.accession)
		columns["form"] = append(columns["form"], r.form)
		columns["filingDate"] = append(columns["filingDate"], r.filingDate)
		columns["acceptanceDateTime"] = append(columns["acceptanceDateTime"], r.accepted)
		columns["primaryDocument"] = append(columns["primaryDocument"], r.document)
		columns["primaryDocDescription"] = append(columns["primaryDocDescription"], r.description)
	}
	return columns
}

func secFixtureJSON(v interface{}) *http.Response {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return mockResponse(string(b), http.StatusOK)
}

func secFixtureCompany(rows []secFixtureRow, files []map[string]interface{}) *http.Response {
	if files == nil {
		files = []map[string]interface{}{}
	}
	return secFixtureJSON(map[string]interface{}{
		"cik": 320193, "name": "Apple Inc.", "tickers": []string{"AAPL"},
		"filings": map[string]interface{}{"recent": secFixtureColumns(rows...), "files": files},
	})
}

const secFixtureMapping = `{"0":{"cik_str":320193,"ticker":"AAPL","title":"Apple Inc."}}`

func TestSECProviderConfiguration(t *testing.T) {
	oldProvider, oldAgent, oldKey := secProvider, secUserAgent, secAPIKey
	t.Cleanup(func() { secProvider, secUserAgent, secAPIKey = oldProvider, oldAgent, oldKey })
	for _, tc := range []struct {
		name, provider, key, want string
		wantErr                   bool
	}{
		{"free default without paid key", "", "", "edgar", false},
		{"old paid key does not override default", "", "existing-secret", "edgar", false},
		{"explicit free", "edgar", "", "edgar", false},
		{"paid is opt in", "sec-api", "test-secret", "sec-api", false},
		{"paid requires key", "sec-api", "", "", true},
		{"invalid provider rejected", "typo", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SEC_FILINGS_PROVIDER", tc.provider)
			t.Setenv("SEC_API_KEY", tc.key)
			t.Setenv("SEC_USER_AGENT", "")
			err := configureSECProvider()
			if (err != nil) != tc.wantErr {
				t.Fatalf("configure error=%v; wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && secProvider != tc.want {
				t.Fatalf("provider=%q; want %q", secProvider, tc.want)
			}
		})
	}
}

func TestEDGARQueryPreservesSchemaVerifiedTimesAndCache(t *testing.T) {
	rows := []secFixtureRow{
		{"0000320193-26-000020", "8-K", "2026-07-31", "2026-07-31T14:01:02Z", "event.htm", "Material event"},
		{"0000320193-26-000021", "4", "2026-07-31", "2026-08-01T01:30:00Z", "ownership.xml", "Ownership report"},
		{"0000320193-26-000022", "8-K", "2026-08-01", "2026-08-01T16:00:00Z", "tomorrow.htm", "Tomorrow"},
	}
	headers := map[string]string{
		"0000320193-26-000020": "20260731060102", // Feed is four hours wrong; SGML is authoritative.
		"0000320193-26-000021": "20260731213000", // UTC falls on the following day.
		"0000320193-26-000022": "20260801120000",
	}
	calls := map[string]int{}
	isolateSEC(t, func(r *http.Request) (*http.Response, error) {
		calls[r.URL.Path]++
		if r.Method != "GET" || r.Header.Get("User-Agent") != "Polygon Charts test@example.com" {
			t.Errorf("unexpected SEC request method or declared User-Agent")
		}
		switch r.URL.Path {
		case "/files/company_tickers.json":
			return mockResponse(secFixtureMapping, 200), nil
		case "/submissions/CIK0000320193.json":
			return secFixtureCompany(rows, nil), nil
		}
		for accession, accepted := range headers {
			if strings.HasSuffix(r.URL.Path, accession+"-index-headers.html") {
				return mockResponse("<html><pre>&lt;SEC-HEADER&gt;\n&lt;ACCEPTANCE-DATETIME&gt;"+accepted+"\n</pre></html>", 200), nil
			}
		}
		return nil, fmt.Errorf("unexpected SEC request %s", r.URL.Path)
	})
	// An existing paid key must not silently cause a paid request.
	secAPIKey = "unused-existing-key"
	items, err := querySECFilings(" aapl ", "2026-07-31")
	if err != nil || len(items) != 2 {
		t.Fatalf("filings=%+v error=%v", items, err)
	}
	for i, expected := range []string{"2026-08-01T01:30:00Z", "2026-07-31T10:01:02Z"} {
		got := parsePublished(firstNonEmptyString(items[i], "filedAt"))
		want, _ := time.Parse(time.RFC3339, expected)
		if !got.Equal(want) {
			t.Errorf("row %d timestamp=%s; want %s", i, got, want)
		}
		for _, field := range []string{"formType", "description", "companyName", "linkToFilingDetails"} {
			if firstNonEmptyString(items[i], field) == "" {
				t.Errorf("missing table/copy contract field %q: %+v", field, items[i])
			}
		}
		if !strings.HasPrefix(firstNonEmptyString(items[i], "linkToFilingDetails"), "https://www.sec.gov/Archives/edgar/data/320193/") {
			t.Errorf("invalid filing link: %+v", items[i])
		}
	}
	before := 0
	for _, n := range calls {
		before += n
	}
	if _, err := querySECFilings("AAPL", "2026-07-31"); err != nil {
		t.Fatal(err)
	}
	after := 0
	for _, n := range calls {
		after += n
	}
	if after != before {
		t.Fatalf("cached repeat fetched upstream again: before=%d after=%d", before, after)
	}
}

func TestEDGARHistoricalChunkIncludesNextBusinessDayFilingDate(t *testing.T) {
	oldAccession := "0000320193-24-000080"
	archiveCalls := 0
	isolateSEC(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/files/company_tickers.json":
			return mockResponse(secFixtureMapping, 200), nil
		case "/submissions/CIK0000320193.json":
			return secFixtureCompany(nil, []map[string]interface{}{
				{"name": "CIK0000320193-submissions-001.json", "filingFrom": "2024-07-08", "filingTo": "2025-12-31", "filingCount": 1},
				{"name": "CIK0000320193-submissions-002.json", "filingFrom": "2000-01-01", "filingTo": "2010-12-31", "filingCount": 1},
			}), nil
		case "/submissions/CIK0000320193-submissions-001.json":
			archiveCalls++
			return secFixtureJSON(secFixtureColumns(secFixtureRow{oldAccession, "S-3", "2024-07-08", "2024-07-06T01:58:00Z", "shelf.htm", "Shelf registration"})), nil
		}
		if strings.HasSuffix(r.URL.Path, oldAccession+"-index-headers.html") {
			return mockResponse("<ACCEPTANCE-DATETIME>20240705215800\n", 200), nil
		}
		return nil, fmt.Errorf("unexpected historical request %s", r.URL.Path)
	})
	items, err := querySECFilings("AAPL", "2024-07-05")
	if err != nil || len(items) != 1 || archiveCalls != 1 {
		t.Fatalf("historical filings=%+v archives=%d error=%v", items, archiveCalls, err)
	}
	if got := parsePublished(firstNonEmptyString(items[0], "filedAt")).In(marketNewsLocation).Format("2006-01-02 15:04:05"); got != "2024-07-05 21:58:00" {
		t.Fatalf("wrong historical acceptance time %s", got)
	}
}

func TestEDGARWinterAcceptanceTime(t *testing.T) {
	accession := "0000320193-26-000001"
	isolateSEC(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/files/company_tickers.json":
			return mockResponse(secFixtureMapping, 200), nil
		case "/submissions/CIK0000320193.json":
			return secFixtureCompany([]secFixtureRow{{accession, "8-K", "2026-01-05", "2026-01-05T16:00:00Z", "event.htm", ""}}, nil), nil
		}
		if strings.HasSuffix(r.URL.Path, accession+"-index-headers.html") {
			return mockResponse("<ACCEPTANCE-DATETIME>20260105160000\n", 200), nil
		}
		return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
	})
	items, err := querySECFilings("AAPL", "2026-01-05")
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v error=%v", items, err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-01-05T21:00:00Z")
	if !parsePublished(firstNonEmptyString(items[0], "filedAt")).Equal(want) {
		t.Fatalf("winter timezone conversion is wrong: %+v", items[0])
	}
	if firstNonEmptyString(items[0], "description") == "" {
		t.Fatal("empty SEC primary description must have a useful link label")
	}
}

func TestEDGARMissingIdentityAndUnknownTickerAreVisibleErrors(t *testing.T) {
	calls := 0
	isolateSEC(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return mockResponse(secFixtureMapping, 200), nil
	})
	secUserAgent = ""
	if _, err := querySECFilings("AAPL", "2026-07-31"); err == nil || !strings.Contains(err.Error(), "SEC_USER_AGENT") {
		t.Fatalf("missing identity should explain how to configure it: %v", err)
	}
	if calls != 0 {
		t.Fatal("made SEC request without a declared identity")
	}
	secUserAgent = "Polygon Charts test@example.com"
	if _, err := querySECFilings("NOTREAL", "2026-07-31"); err == nil {
		t.Fatal("unmapped ticker must not silently look like zero filings")
	}
}

func TestEDGARHTTPFailureIsNotCachedAsSuccess(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			fail, calls := true, 0
			isolateSEC(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if fail {
					return mockResponse("temporarily unavailable", status), nil
				}
				if r.URL.Path == "/files/company_tickers.json" {
					return mockResponse(secFixtureMapping, 200), nil
				}
				return secFixtureCompany(nil, nil), nil
			})
			if _, err := querySECFilings("AAPL", "2026-07-31"); err == nil {
				t.Fatalf("HTTP %d was accepted as an empty success", status)
			}
			before := calls
			fail = false
			if items, err := querySECFilings("AAPL", "2026-07-31"); err != nil || len(items) != 0 {
				t.Fatalf("recovery failed: items=%+v error=%v", items, err)
			}
			if calls == before {
				t.Fatal("previous failure was cached as successful empty data")
			}
		})
	}
}

func TestEDGARUnverifiedTimestampDoesNotEnterReplay(t *testing.T) {
	accession := "0000320193-26-000020"
	isolateSEC(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/files/company_tickers.json" {
			return mockResponse(secFixtureMapping, 200), nil
		}
		if r.URL.Path == "/submissions/CIK0000320193.json" {
			return secFixtureCompany([]secFixtureRow{{accession, "8-K", "2026-07-31", "2026-07-31T14:01:02Z", "event.htm", "Event"}}, nil), nil
		}
		return mockResponse("<html>Header is not yet available</html>", 200), nil
	})
	items, err := querySECFilings("AAPL", "2026-07-31")
	if err == nil || len(items) != 0 {
		t.Fatalf("unverified feed time must not appear as a replay-safe filing: items=%+v err=%v", items, err)
	}
}

func TestEDGARStaleVerifiedTimestampSurvivesHeaderOutage(t *testing.T) {
	accession := "0000320193-26-000020"
	headerUnavailable, headerCalls := false, 0
	isolateSEC(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/files/company_tickers.json":
			return mockResponse(secFixtureMapping, 200), nil
		case "/submissions/CIK0000320193.json":
			return secFixtureCompany([]secFixtureRow{{accession, "8-K", "2026-07-31", "2026-07-31T14:01:02Z", "event.htm", "Event"}}, nil), nil
		}
		if strings.HasSuffix(r.URL.Path, accession+"-index-headers.html") {
			headerCalls++
			if headerUnavailable {
				return mockResponse("temporary header outage", 503), nil
			}
			return mockResponse("<ACCEPTANCE-DATETIME>20260731060102\n", 200), nil
		}
		return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
	})
	before, err := querySECFilings("AAPL", "2026-07-31")
	if err != nil || len(before) != 1 {
		t.Fatalf("could not prime verified cache: items=%+v error=%v", before, err)
	}
	paths, err := filepath.Glob(filepath.Join(dataDir, "edgar_acceptance_*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("expected one cached verified timestamp, paths=%v error=%v", paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var cache jsonCacheFile
	if err := json.Unmarshal(raw, &cache); err != nil {
		t.Fatal(err)
	}
	cache.FetchedAt = time.Now().Add(-48 * time.Hour).Unix()
	cache.Complete = false
	raw, err = json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[0], raw, 0644); err != nil {
		t.Fatal(err)
	}
	headerUnavailable = true
	after, err := querySECFilings("AAPL", "2026-07-31")
	if err == nil || len(after) != 1 || headerCalls != 2 {
		t.Fatalf("cached filing should survive with coverage error: items=%+v calls=%d error=%v", after, headerCalls, err)
	}
	if after[0]["filedAt"] != before[0]["filedAt"] || after[0]["formType"] != before[0]["formType"] {
		t.Fatalf("outage changed previously verified filing: before=%+v after=%+v", before, after)
	}
}

func TestEDGARMalformedRowsKeepValidPartialCoverage(t *testing.T) {
	for _, missing := range []string{"form", "dates"} {
		t.Run(missing, func(t *testing.T) {
			goodAccession := "0000320193-26-000020"
			rows := secFixtureColumns(
				secFixtureRow{goodAccession, "8-K", "2026-07-31", "2026-07-31T14:01:02Z", "event.htm", "Valid event"},
				secFixtureRow{"0000320193-26-000021", "4", "2026-07-31", "2026-07-31T20:00:00Z", "ownership.xml", "Broken event"},
			)
			if missing == "form" {
				rows["form"] = rows["form"][:1]
			} else {
				rows["filingDate"] = rows["filingDate"][:1]
				rows["acceptanceDateTime"] = rows["acceptanceDateTime"][:1]
			}
			headerCalls := 0
			isolateSEC(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/files/company_tickers.json":
					return mockResponse(secFixtureMapping, 200), nil
				case "/submissions/CIK0000320193.json":
					return secFixtureJSON(map[string]interface{}{
						"name":    "Apple Inc.",
						"filings": map[string]interface{}{"recent": rows, "files": []interface{}{}},
					}), nil
				}
				if strings.HasSuffix(r.URL.Path, goodAccession+"-index-headers.html") {
					headerCalls++
					return mockResponse("<ACCEPTANCE-DATETIME>20260731060102\n", 200), nil
				}
				return nil, fmt.Errorf("unexpected request for malformed filing %s", r.URL.Path)
			})
			items, err := querySECFilings("AAPL", "2026-07-31")
			if err == nil || len(items) != 1 || headerCalls != 1 || items[0]["formType"] != "8-K" {
				t.Fatalf("malformed row must be excluded with partial coverage: items=%+v calls=%d error=%v", items, headerCalls, err)
			}
		})
	}
}

func TestEDGARRequestRateSpacing(t *testing.T) {
	var times []time.Time
	isolateSEC(t, func(r *http.Request) (*http.Response, error) {
		times = append(times, time.Now())
		return mockResponse(`{}`, 200), nil
	})
	edgarRequestInterval = 20 * time.Millisecond
	for range 3 {
		if _, err := edgarGet(context.Background(), "https://data.sec.gov/submissions/CIK0000320193.json"); err != nil {
			t.Fatal(err)
		}
	}
	// Permit small timer variation while catching a missing/shared limiter bypass.
	if elapsed := times[2].Sub(times[0]); elapsed < 35*time.Millisecond {
		t.Fatalf("SEC requests were not spaced by the limiter: elapsed=%s", elapsed)
	}
}

func TestSECExplicitPaidProvider(t *testing.T) {
	isolateSEC(t, func(r *http.Request) (*http.Response, error) {
		t.Fatal("explicit paid provider made an EDGAR request")
		return nil, fmt.Errorf("unexpected EDGAR request")
	})
	previous := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = previous })
	secProvider, secAPIKey = "sec-api", "test-paid-key"
	http.DefaultClient = &http.Client{Transport: rtprTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.sec-api.io" || r.Method != "POST" || r.URL.Query().Get("token") != secAPIKey {
			t.Fatalf("wrong paid request %s %s", r.Method, r.URL.Host)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "ticker:AAPL") || !strings.Contains(string(body), "2026-07-31") {
			t.Fatalf("paid query lost ticker/date: %s", body)
		}
		return mockResponse(`{"filings":[{"filedAt":"2026-07-31T06:01:02-04:00","formType":"8-K"}]}`, 200), nil
	})}
	items, err := querySECFilings("AAPL", "2026-07-31")
	if err != nil || len(items) != 1 {
		t.Fatalf("paid opt-in broken: items=%+v error=%v", items, err)
	}
}

func TestEDGARCancelledQuery(t *testing.T) {
	isolateSEC(t, func(r *http.Request) (*http.Response, error) {
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := querySECFilingsContext(ctx, "AAPL", "2026-07-31"); err == nil {
		t.Fatal("cancelled query unexpectedly succeeded")
	}
}

func TestEDGARExtraHandlerDefaultProviderAndCoverageWarning(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unavailable=%v", unavailable), func(t *testing.T) {
			accession := "0000320193-26-000020"
			isolateSEC(t, func(r *http.Request) (*http.Response, error) {
				if unavailable {
					return mockResponse("unavailable", 503), nil
				}
				switch r.URL.Path {
				case "/files/company_tickers.json":
					return mockResponse(secFixtureMapping, 200), nil
				case "/submissions/CIK0000320193.json":
					return secFixtureCompany([]secFixtureRow{{accession, "8-K", "2026-07-31", "2026-07-31T14:01:02Z", "event.htm", "Material event"}}, nil), nil
				}
				if strings.HasSuffix(r.URL.Path, accession+"-index-headers.html") {
					return mockResponse("<ACCEPTANCE-DATETIME>20260731060102\n", 200), nil
				}
				return nil, fmt.Errorf("unexpected SEC request %s", r.URL.Path)
			})
			previousRTPR := rtprAPIKey
			rtprAPIKey = ""
			t.Cleanup(func() { rtprAPIKey = previousRTPR })
			http.DefaultClient = &http.Client{Transport: rtprTransport(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/aggs/ticker/SPY/") {
					return mockResponse(`{"results":[{"t":1785427200000,"c":100}]}`, 200), nil
				}
				if strings.Contains(r.URL.Path, "/benzinga/") {
					return mockResponse(`{"results":[]}`, 200), nil
				}
				if r.URL.Host == "financialmodelingprep.com" {
					return mockResponse(`[]`, 200), nil
				}
				return nil, fmt.Errorf("unexpected non-EDGAR request %s", r.URL.Host)
			})}
			response := httptest.NewRecorder()
			extraHandler(response, httptest.NewRequest("GET", "/api/extra?ticker=AAPL&date=2026-07-31&days=1&profile=false", nil))
			var got struct {
				Filings  []map[string]interface{} `json:"filings"`
				Warnings []string                 `json:"warnings"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil {
				t.Fatalf("bad response: %s", response.Body.String())
			}
			if unavailable {
				if len(got.Warnings) == 0 || len(got.Filings) != 0 || !strings.Contains(strings.Join(got.Warnings, " "), "SEC") {
					t.Fatalf("missing user-visible filing coverage warning: %s", response.Body.String())
				}
			} else if len(got.Warnings) != 0 || len(got.Filings) != 1 || got.Filings[0]["catalyst_period"] != "recent" {
				t.Fatalf("default EDGAR integration did not preserve catalysts: %s", response.Body.String())
			}
		})
	}
}

func TestEDGARLive(t *testing.T) {
	if os.Getenv("EDGAR_LIVE_TEST") != "1" {
		t.Skip("opt-in live SEC check; set EDGAR_LIVE_TEST=1 and SEC_USER_AGENT")
	}
	userAgent := strings.TrimSpace(os.Getenv("SEC_USER_AGENT"))
	if userAgent == "" {
		t.Fatal("SEC_USER_AGENT is required for the live check")
	}
	isolateSEC(t, http.DefaultTransport.RoundTrip)
	secUserAgent = userAgent
	edgarRequestInterval = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	items, err := querySECFilingsContext(ctx, "AAPL", "2026-07-31")
	if err != nil || len(items) == 0 {
		t.Fatalf("live SEC filings=%d error=%v", len(items), err)
	}
	for _, item := range items {
		if parsePublished(firstNonEmptyString(item, "filedAt")).IsZero() || !strings.HasPrefix(firstNonEmptyString(item, "linkToFilingDetails"), "https://www.sec.gov/") {
			t.Fatalf("invalid live filing %+v", item)
		}
	}
}
