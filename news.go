package main

import (
	"context"
	"html"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Canonical URLs remove transport/tracking differences, retaining meaningful queries.
func canonicalNewsURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	u.Scheme = "https"
	u.Host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/")
	q := u.Query()
	for key := range q {
		k := strings.ToLower(key)
		if strings.HasPrefix(k, "utm_") || k == "fbclid" || k == "gclid" || k == "mc_cid" || k == "mc_eid" {
			q.Del(key)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func normalizedHeadline(s string) string {
	s = strings.ToLower(html.UnescapeString(s))
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		// Retain decimal points and signed amounts: $1.5 and $15 are different news.
		numericNext := i+1 < len(runes) && (unicode.IsNumber(runes[i+1]) || runes[i+1] == '$')
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '%' ||
			(r == '.' && i > 0 && unicode.IsNumber(runes[i-1]) && numericNext) ||
			((r == '-' || r == '+' || r == '−') && numericNext) {
			if r == '−' {
				r = '-'
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func deduplicateNews(items []NewsItem) []NewsItem {
	// Keep the earliest known publication so syndication cannot turn old news into a new catalyst.
	items = append([]NewsItem(nil), items...)
	sort.SliceStable(items, func(i, j int) bool {
		a, b := parsePublished(items[i].Published), parsePublished(items[j].Published)
		if a.IsZero() != b.IsZero() {
			return !a.IsZero()
		}
		return isNewsNewer(items[j], items[i])
	})
	// Union aliases as well as retained rows, so URL matches and headline matches compose.
	parents := make([]int, len(items))
	urls, headlines := make([]string, len(items)), make([]string, len(items))
	times := make([]time.Time, len(items))
	var root func(int) int
	root = func(i int) int {
		if parents[i] != i {
			parents[i] = root(parents[i])
		}
		return parents[i]
	}
	for i, item := range items {
		parents[i] = i
		urls[i], headlines[i], times[i] = canonicalNewsURL(item.URL), normalizedHeadline(item.Title), parsePublished(item.Published)
		for j := 0; j < i; j++ {
			sameURL := urls[i] != "" && urls[i] == urls[j]
			sameTitle := len([]rune(headlines[i])) >= 12 && headlines[i] == headlines[j] &&
				!times[i].IsZero() && !times[j].IsZero() && times[i].Sub(times[j]).Abs() <= 48*time.Hour
			if sameURL || sameTitle {
				a, b := root(i), root(j)
				if a < b {
					parents[b] = a
				} else {
					parents[a] = b
				}
			}
		}
	}
	out := []NewsItem{}
	for i, item := range items {
		if root(i) == i {
			out = append(out, item)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return isNewsNewer(out[i], out[j]) })
	return out
}

func catalystCutoff(prior string) time.Time {
	d, err := time.ParseInLocation("2006-01-02", prior, marketNewsLocation)
	if err != nil {
		return time.Time{}
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 15, 59, 0, 0, marketNewsLocation)
}

func catalystPeriod(published string, cutoff time.Time) string {
	t := parsePublished(published)
	if cutoff.IsZero() || t.IsZero() {
		return "unknown"
	}
	if t.After(cutoff) {
		return "recent"
	}
	return "older"
}

func annotateCatalysts(news []NewsItem, filings []map[string]interface{}, cutoff time.Time) {
	label := ""
	if !cutoff.IsZero() {
		label = cutoff.Format(time.RFC3339)
	}
	for i := range news {
		news[i].CatalystPeriod = catalystPeriod(news[i].Published, cutoff)
		news[i].CatalystCutoff = label
	}
	for _, filing := range filings {
		filing["catalyst_period"] = catalystPeriod(firstNonEmptyString(filing, "filedAt"), cutoff)
		filing["catalyst_cutoff"] = label
	}
}

// All views use the same sources, market calendar, deduplication and cutoff.
func collectCatalysts(ctx context.Context, ticker, date string, days int) ([]NewsItem, []map[string]interface{}, string, []string) {
	prior, _ := getPriorTradingDate("SPY", date)
	from := date
	if days == 2 && prior != "" {
		from = prior
	}
	var news []NewsItem
	var filings []map[string]interface{}
	var rtprNews []NewsItem
	var rtprErr error
	var secIncomplete bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); rtprNews, rtprErr = queryRTPRNews(ctx, ticker, from, date) }()
	for day := from; day <= date; day = nextDay(day) {
		massive, _ := queryMassiveBenzingaNews(ticker, day)
		news = appendMassiveNewsItems(news, massive)
		fmp, _ := queryFMPNews(ticker, day)
		for _, f := range fmp {
			title, link, published := firstNonEmptyString(f, "title"), firstNonEmptyString(f, "url"), firstNonEmptyString(f, "publishedDate")
			if title != "" && link != "" && published != "" {
				news = append(news, NewsItem{Title: title, URL: link, Source: firstNonEmptyString(f, "site"), Published: normalizePublished(published)})
			}
		}
		sec, secErr := querySECFilingsContext(ctx, ticker, day)
		secIncomplete = secIncomplete || secErr != nil
		filings = append(filings, sec...)
	}
	wg.Wait()
	news = deduplicateNews(filterTradeableNews(append(news, rtprNews...)))
	sort.SliceStable(filings, func(i, j int) bool {
		return parsePublished(firstNonEmptyString(filings[i], "filedAt")).After(parsePublished(firstNonEmptyString(filings[j], "filedAt")))
	})
	annotateCatalysts(news, filings, catalystCutoff(prior))
	warnings := []string{}
	if secIncomplete {
		warning := "SEC filing coverage is incomplete or unavailable; cached filings may be stale."
		if secProvider == "edgar" && secUserAgent == "" {
			warning = "SEC filings unavailable: set SEC_USER_AGENT to your app name and contact email for free EDGAR access."
		}
		warnings = append(warnings, warning)
	}
	if prior == "" {
		warnings = append(warnings, "Previous trading day unavailable; catalyst cutoff could not be determined.")
	}
	if rtprErr != nil {
		warnings = append(warnings, "RTPR coverage is incomplete or unavailable; other news sources remain available.")
	}
	return news, filings, prior, warnings
}
