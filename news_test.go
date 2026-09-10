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

	"github.com/joho/godotenv"
)

type rtprTransport func(*http.Request) (*http.Response, error)

func (f rtprTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mockResponse(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func isolateRTPR(t *testing.T, transport rtprTransport) {
	t.Helper()
	cwd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	oldKey, oldClient := rtprAPIKey, rtprClient
	rtprAPIKey = "test-secret"
	rtprClient = &http.Client{Transport: transport}
	t.Cleanup(func() { rtprAPIKey = oldKey; rtprClient = oldClient; _ = os.Chdir(cwd) })
}

func TestRTPRPaginationDateWindowAndCache(t *testing.T) {
	calls, articleCalls := 0, 0
	isolateRTPR(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.rtpr.io" {
			calls++
			if r.Header.Get("Authorization") != "Bearer test-secret" || r.URL.Path != "/feed/articles/AAPL" {
				t.Errorf("unexpected feed request")
			}
			if calls == 1 {
				return mockResponse(`{"articles":[{"ticker":"AAPL","article_published_at":"2026-06-10T04:00:00Z","article_url":"https://rtpr.io/a/future"}],"next_token":"opaque+/="}`, 200), nil
			}
			if r.URL.Query().Get("next_token") != "opaque+/=" {
				t.Errorf("cursor not preserved")
			}
			// NY late evening must be included even though its UTC date is the next day.
			return mockResponse(`{"articles":[{"ticker":"AAPL","article_published_at":"2026-06-10T03:59:00Z","article_url":"https://rtpr.io/a/id-20260610?sig=keep&exp=1"},{"ticker":"AAPL","article_published_at":"2026-06-09T03:59:59Z","article_url":"https://rtpr.io/a/old"}],"next_token":"more"}`, 200), nil
		}
		articleCalls++
		if r.Header.Get("X-API-Key") != "test-secret" || r.URL.RawQuery != "sig=keep&exp=1" {
			t.Errorf("article auth or signed URL changed")
		}
		return mockResponse(`<html><title>Fallback — RTPR</title><span class="meta-source">Business Wire</span><h1>Company &amp; Partner <em>announce</em> a deal</h1></html>`, 200), nil
	})
	news, err := queryRTPRNews(context.Background(), "AAPL", "2026-06-09", "2026-06-09")
	if err != nil || len(news) != 1 {
		t.Fatalf("news=%+v err=%v", news, err)
	}
	if news[0].Title != "Company & Partner announce a deal" || news[0].Source != "Business Wire · RTPR" || strings.Contains(news[0].URL, "test-secret") {
		t.Fatalf("bad article %+v", news[0])
	}
	_, err = queryRTPRNews(context.Background(), "AAPL", "2026-06-09", "2026-06-09")
	if err != nil || calls != 2 || articleCalls != 1 {
		t.Fatalf("cache/pagination calls=%d articles=%d err=%v", calls, articleCalls, err)
	}
}

func TestRTPRDisabledAndFailures(t *testing.T) {
	isolateRTPR(t, func(r *http.Request) (*http.Response, error) { return mockResponse(`{"error":"no access"}`, 403), nil })
	rtprAPIKey = ""
	if items, err := queryRTPRNews(context.Background(), "AAPL", "2026-06-09", "2026-06-09"); err != nil || len(items) != 0 {
		t.Fatalf("disabled: %v", err)
	}
	rtprAPIKey = "test-secret"
	if _, err := queryRTPRNews(context.Background(), "AAPL", "2026-06-09", "2026-06-09"); err == nil || strings.Contains(err.Error(), rtprAPIKey) {
		t.Fatalf("bad error %v", err)
	}
	matches, _ := filepath.Glob("data/rtpr_news*")
	if len(matches) != 0 {
		t.Fatal("failure was cached as success")
	}
}

func TestRTPRRejectsUnsafeURLsAndEscapesReader(t *testing.T) {
	for _, s := range []string{"http://rtpr.io/a/id", "https://evil.example/a/id", "https://rtpr.io.evil/a/id", "https://rtpr.io/a/../x", "https://rtpr.io/a/%2fsecret"} {
		if _, ok := rtprArticleID(s); ok {
			t.Errorf("accepted %q", s)
		}
	}
	isolateRTPR(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-API-Key") != "test-secret" {
			t.Error("reader request contract")
		}
		return mockResponse(`<h1>Company &amp; Partner</h1><div class="article-body is-html"><script>alert("bad")</script><a href="https://example.com/report" target="_self" onclick="bad()">Report</a><a href="javascript:alert(1)">Bad</a><table><tr><td>Revenue</td><td>$1.5</td></tr></table></div>`, 200), nil
	})
	w := httptest.NewRecorder()
	rtprArticleHandler(w, httptest.NewRequest("GET", "/api/rtpr/article?id=opaque-id", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "onclick") || strings.Contains(w.Body.String(), "javascript:") || !strings.Contains(w.Body.String(), `href="https://example.com/report" target="_blank" rel="noopener noreferrer"`) || !strings.Contains(w.Body.String(), "<table>") {
		t.Fatalf("unsafe reader %s", w.Body.String())
	}
}

func TestDeduplicateAcrossSources(t *testing.T) {
	items := []NewsItem{
		{Title: "Acme Announces Record Second Quarter Results", URL: "https://wire.example/release", Published: "2026-09-08T19:58:00Z", Source: "Wire"},
		{Title: "ACME announces record second-quarter results!", URL: "https://rtpr.io/a/other-id", Published: "2026-09-08T20:01:00Z", Source: "RTPR"},
		{Title: "An edited headline", URL: "http://www.wire.example/release/?utm_source=foo#top", Published: "2026-09-08T20:02:00Z"},
		{Title: "Acme Announces Record Third Quarter Results", URL: "https://wire.example/third", Published: "2026-09-08T20:03:00Z"},
		{Title: "Acme Announces Record Second Quarter Results", URL: "https://wire.example/next-year", Published: "2027-09-08T20:03:00Z"},
	}
	got := deduplicateNews(items)
	if len(got) != 3 {
		t.Fatalf("duplicates not collapsed: %+v", got)
	}
	if got[2].Published != "2026-09-08T19:58:00Z" {
		t.Fatal("syndicated copy shifted catalyst time")
	}
	if canonicalNewsURL("https://x.example/story?id=1") == canonicalNewsURL("https://x.example/story?id=2") {
		t.Fatal("meaningful queries removed")
	}
}

func TestCatalystCutoff(t *testing.T) {
	for _, tc := range []struct{ prior, timestamp, want string }{
		{"2026-09-08", "2026-09-08T19:58:59Z", "older"},
		{"2026-09-08", "2026-09-08T19:59:00Z", "older"},
		{"2026-09-08", "2026-09-08T19:59:01Z", "recent"},
		{"2026-09-08", normalizePublished("2026-09-08T19:59:00.001Z"), "recent"},
		{"2026-09-08", "2026-09-08 16:00:00", "recent"},
		{"2026-09-04", "2026-09-06T12:00:00Z", "recent"},
		{"2026-03-06", "2026-03-06T20:59:00Z", "older"},
		{"2026-03-06", "2026-03-09T13:30:00Z", "recent"},
		{"2026-11-02", "2026-11-02T20:59:01Z", "recent"},
		{"2026-09-08", "invalid", "unknown"},
		{"", "2026-09-09T12:00:00Z", "unknown"},
	} {
		if got := catalystPeriod(tc.timestamp, catalystCutoff(tc.prior)); got != tc.want {
			t.Errorf("%+v got %s", tc, got)
		}
	}
	news := []NewsItem{{Published: "2026-09-08T20:00:00Z"}}
	filings := []map[string]interface{}{{"filedAt": "2026-09-08T20:00:00Z"}, {"filedAt": nil}}
	annotateCatalysts(news, filings, catalystCutoff("2026-09-08"))
	if news[0].CatalystPeriod != filings[0]["catalyst_period"] || filings[1]["catalyst_period"] != "unknown" {
		t.Fatal("inconsistent annotation")
	}
}

func TestRTPRLive(t *testing.T) {
	if os.Getenv("RTPR_LIVE_TEST") != "1" {
		t.Skip("opt-in live API check")
	}
	env, err := godotenv.Read(".env")
	if err != nil {
		t.Fatal(err)
	}
	old := rtprAPIKey
	rtprAPIKey = env["RTPR_API_KEY"]
	defer func() { rtprAPIKey = old }()
	if rtprAPIKey == "" {
		t.Fatal("RTPR key not configured")
	}
	today := nyDateString(time.Now())
	items, err := queryRTPRNews(context.Background(), "AAPL", today, today)
	if err != nil || len(items) == 0 {
		t.Fatalf("live fetch count=%d error=%v", len(items), err)
	}
	for _, item := range items {
		if item.Title == "" || !strings.HasPrefix(item.URL, "/api/rtpr/article?") {
			t.Fatal("invalid live article")
		}
	}
	reader := httptest.NewRecorder()
	rtprArticleHandler(reader, httptest.NewRequest("GET", items[0].URL, nil))
	if reader.Code != http.StatusOK || !strings.Contains(reader.Body.String(), `target="_blank"`) || strings.Contains(reader.Body.String(), rtprAPIKey) {
		t.Fatalf("live article reader status=%d", reader.Code)
	}
	body, _ := json.Marshal(items[0])
	fmt.Printf("Live RTPR: %d articles; sample %s\n", len(items), body)
}

func TestDeduplicateNumericChangesAndAliasChains(t *testing.T) {
	for _, pair := range [][2]string{
		{"Company reports $1.5 million in quarterly revenue", "Company reports $15 million in quarterly revenue"},
		{"Company reports EPS of -$1.50 for the quarter", "Company reports EPS of $1.50 for the quarter"},
	} {
		got := deduplicateNews([]NewsItem{{Title: pair[0], URL: "https://a.example/1", Published: "2026-09-08T20:00:00Z"}, {Title: pair[1], URL: "https://b.example/2", Published: "2026-09-08T20:01:00Z"}})
		if len(got) != 2 {
			t.Fatalf("different figures collapsed: %v", pair)
		}
	}
	got := deduplicateNews([]NewsItem{
		{Title: "Company signs a supply agreement", URL: "https://a.example/1", Published: "2026-09-08T20:00:00Z"},
		{Title: "Company signs a new supply agreement", URL: "https://a.example/1", Published: "2026-09-08T20:01:00Z"},
		{Title: "Company signs a new supply agreement", URL: "https://b.example/2", Published: "2026-09-08T20:02:00Z"},
	})
	if len(got) != 1 {
		t.Fatalf("alias chain retained duplicates: %+v", got)
	}
}

func TestExtraHandlerHolidayWindowAndRTPRFailure(t *testing.T) {
	isolateRTPR(t, func(r *http.Request) (*http.Response, error) { return mockResponse(`{}`, 503), nil })
	previous := http.DefaultClient
	defer func() { http.DefaultClient = previous }()
	fmpDates := map[string]bool{}
	http.DefaultClient = &http.Client{Transport: rtprTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/aggs/ticker/SPY/") {
			if strings.Contains(r.URL.Path, "2026-09-04") {
				return mockResponse(`{"results":[{"t":1788530400000,"c":100}]}`, 200), nil
			}
			return mockResponse(`{"results":[]}`, 200), nil
		}
		if strings.Contains(r.URL.Path, "/benzinga/") {
			if r.URL.Query().Get("published.gte") == "2026-09-04T04:00:00Z" {
				return mockResponse(`{"results":[{"title":"Company announces a new supply contract","url":"https://wire.example/contract","published":"2026-09-04T19:58:00Z"}]}`, 200), nil
			}
			return mockResponse(`{"results":[]}`, 200), nil
		}
		if r.URL.Host == "financialmodelingprep.com" {
			day := r.URL.Query().Get("from")
			fmpDates[day] = true
			if day == "2026-09-06" {
				return mockResponse(`[{"title":"Company announces a new supply contract!","url":"https://second.example/contract","publishedDate":"2026-09-06 10:00:00","site":"Syndicated wire"},{"title":"Company names a new chief executive","url":"https://wire.example/ceo","publishedDate":"2026-09-06 11:00:00","site":"Wire"}]`, 200), nil
			}
			return mockResponse(`[]`, 200), nil
		}
		if r.URL.Host == "api.sec-api.io" {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "2026-09-04") {
				return mockResponse(`{"filings":[{"filedAt":"2026-09-04T16:01:00-04:00","formType":"8-K"}]}`, 200), nil
			}
			return mockResponse(`{"filings":[]}`, 200), nil
		}
		return nil, fmt.Errorf("unexpected request host: %s", r.URL.Host)
	})}
	response := httptest.NewRecorder()
	extraHandler(response, httptest.NewRequest("GET", "/api/extra?ticker=AAPL&date=2026-09-08&days=2&profile=false", nil))
	var got struct {
		News     []NewsItem               `json:"news"`
		Filings  []map[string]interface{} `json:"filings"`
		Prior    string                   `json:"prior_trading_date"`
		Warnings []string                 `json:"warnings"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || got.Prior != "2026-09-04" || len(fmpDates) != 5 || len(got.News) != 2 || len(got.Warnings) != 1 {
		t.Fatalf("bad combined response: %s dates=%v", response.Body.String(), fmpDates)
	}
	if got.News[0].CatalystPeriod != "recent" || got.News[1].CatalystPeriod != "older" || len(got.Filings) != 1 || got.Filings[0]["catalyst_period"] != "recent" {
		t.Fatalf("incorrect holiday cutoff: %s", response.Body.String())
	}
}
