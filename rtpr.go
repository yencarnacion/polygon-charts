package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

var rtprClient = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
	// Never forward credentials to an article redirect (including another host).
	return http.ErrUseLastResponse
}}

type rtprArticleRef struct {
	Ticker    string `json:"ticker"`
	Published string `json:"article_published_at"`
	URL       string `json:"article_url"`
}
type rtprFeed struct {
	Articles  []rtprArticleRef `json:"articles"`
	NextToken string           `json:"next_token"`
}

func rtprGet(ctx context.Context, endpoint string, feed bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("RTPR invalid request")
	}
	if feed {
		req.Header.Set("Authorization", "Bearer "+rtprAPIKey)
	} else {
		req.Header.Set("X-API-Key", rtprAPIKey)
	}
	resp, err := rtprClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("RTPR request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("RTPR status %d", resp.StatusCode)
	}
	const limit = 8 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(raw) > limit {
		return nil, fmt.Errorf("RTPR unreadable or oversized response")
	}
	return raw, nil
}

func rtprArticleID(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "rtpr.io" || u.User != nil || !strings.HasPrefix(u.Path, "/a/") {
		return "", false
	}
	id := strings.TrimPrefix(u.Path, "/a/")
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00\r\n") {
		return "", false
	}
	return id, true
}

func rtprHeadline(raw []byte) (string, string, error) {
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	var h1, pageTitle, source string
	var content func(*html.Node) string
	content = func(n *html.Node) string {
		if n.Type == html.TextNode {
			return n.Data
		}
		var s strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			s.WriteString(content(c))
		}
		return s.String()
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "h1" && h1 == "" {
				h1 = content(n)
			}
			if n.Data == "title" {
				pageTitle = content(n)
			}
			for _, a := range n.Attr {
				if a.Key == "class" && strings.Contains(" "+a.Val+" ", " meta-source ") {
					source = content(n)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if h1 == "" {
		h1 = strings.TrimSuffix(pageTitle, " — RTPR")
	}
	h1 = strings.Join(strings.Fields(h1), " ")
	if h1 == "" {
		return "", "", fmt.Errorf("RTPR missing headline")
	}
	source = strings.Join(strings.Fields(source), " ")
	if source == "" {
		source = "RTPR"
	} else {
		source += " · RTPR"
	}
	return h1, source, nil
}

func resolveRTPRArticle(ctx context.Context, ref rtprArticleRef) (NewsItem, error) {
	id, ok := rtprArticleID(ref.URL)
	if !ok {
		return NewsItem{}, fmt.Errorf("RTPR invalid article URL")
	}
	path := jsonCachePath("rtpr_headline", fmt.Sprintf("%x", sha256.Sum256([]byte(id))))
	if raw, _, fresh := readJSONCache(path, 24*time.Hour, time.Now()); fresh {
		var item NewsItem
		if json.Unmarshal(raw, &item) == nil && item.Title != "" {
			item.Published = normalizePublished(ref.Published)
			return item, nil
		}
	}
	raw, err := rtprGet(ctx, ref.URL, false)
	if err != nil {
		return NewsItem{}, err
	}
	title, source, err := rtprHeadline(raw)
	if err != nil {
		return NewsItem{}, err
	}
	item := NewsItem{Title: title, Source: source, URL: "/api/rtpr/article?id=" + url.QueryEscape(id), Published: normalizePublished(ref.Published)}
	encoded, _ := json.Marshal(item)
	writeJSONCache(path, encoded, false, time.Now())
	return item, nil
}

func queryRTPRNews(ctx context.Context, ticker, from, to string) ([]NewsItem, error) {
	if rtprAPIKey == "" || to < "2026-05-01" {
		return nil, nil
	}
	now := time.Now()
	path := jsonCachePath("rtpr_news", ticker, from, to)
	cached, hasCache, fresh := readJSONCache(path, time.Minute, now)
	var stale []NewsItem
	if hasCache && json.Unmarshal(cached, &stale) == nil && fresh {
		return stale, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	items, err := fetchRTPRNews(ctx, ticker, from, to)
	if err != nil {
		return deduplicateNews(append(items, stale...)), err
	}
	raw, _ := json.Marshal(items)
	writeJSONCache(path, raw, dateScopedDataIsComplete(to, now), now)
	return items, nil
}

func fetchRTPRNews(ctx context.Context, ticker, from, to string) ([]NewsItem, error) {
	start, err := time.ParseInLocation("2006-01-02", from, marketNewsLocation)
	if err != nil {
		return nil, err
	}
	end, err := time.ParseInLocation("2006-01-02", to, marketNewsLocation)
	if err != nil {
		return nil, err
	}
	end = end.AddDate(0, 0, 1)
	refs := []rtprArticleRef{}
	seenURLs, seenTokens := map[string]bool{}, map[string]bool{}
	token := ""
	var fetchErr error
	for page := 0; ; page++ {
		if page >= 100 {
			fetchErr = fmt.Errorf("RTPR pagination limit reached")
			break
		}
		params := url.Values{"limit": {"500"}}
		if token != "" {
			params.Set("next_token", token)
		}
		raw, err := rtprGet(ctx, "https://api.rtpr.io/feed/articles/"+url.PathEscape(ticker)+"?"+params.Encode(), true)
		if err != nil {
			fetchErr = err
			break
		}
		var feed rtprFeed
		if err = json.Unmarshal(raw, &feed); err != nil {
			fetchErr = fmt.Errorf("RTPR invalid feed")
			break
		}
		reachedStart := false
		for _, ref := range feed.Articles {
			t := parsePublished(ref.Published)
			if t.IsZero() {
				fetchErr = fmt.Errorf("RTPR invalid publication time")
				continue
			}
			if t.Before(start) {
				reachedStart = true
				continue
			}
			if !t.Before(end) || seenURLs[canonicalNewsURL(ref.URL)] {
				continue
			}
			if ref.Ticker != "" && !strings.EqualFold(ref.Ticker, ticker) {
				continue
			}
			seenURLs[canonicalNewsURL(ref.URL)] = true
			refs = append(refs, ref)
		}
		if reachedStart || feed.NextToken == "" {
			break
		}
		if seenTokens[feed.NextToken] {
			fetchErr = fmt.Errorf("RTPR repeated cursor")
			break
		}
		token = feed.NextToken
		seenTokens[token] = true
	}
	items := make([]NewsItem, 0, len(refs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	jobs := make(chan rtprArticleRef)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ref := range jobs {
				item, err := resolveRTPRArticle(ctx, ref)
				mu.Lock()
				if err != nil {
					fetchErr = err
				} else {
					items = append(items, item)
				}
				mu.Unlock()
			}
		}()
	}
	for _, ref := range refs {
		jobs <- ref
	}
	close(jobs)
	wg.Wait()
	return items, fetchErr
}

// Only this allowlisted article fragment is passed to template.HTML. Remote scripts,
// styles, embeds, images, event handlers and arbitrary attributes are never rendered.
func rtprArticleContent(raw []byte, baseURL string) (template.HTML, bool, error) {
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return "", false, err
	}
	var body *html.Node
	formatted := false
	var find func(*html.Node)
	find = func(n *html.Node) {
		for _, a := range n.Attr {
			if a.Key == "class" && strings.Contains(" "+a.Val+" ", " article-body ") {
				body = n
				formatted = strings.Contains(" "+a.Val+" ", " is-html ")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if body == nil {
		return "", false, fmt.Errorf("RTPR missing article body")
	}
	base, _ := url.Parse(baseURL)
	allowed := " div span p br a strong em b i u ul ol li table thead tbody tfoot tr th td caption colgroup col blockquote h2 h3 h4 h5 h6 pre code hr sup sub "
	blocked := " script style iframe object embed form input button textarea select noscript link meta svg math img "
	var clean func(*html.Node, *html.Node)
	clean = func(n, parent *html.Node) {
		if n.Type == html.TextNode {
			parent.AppendChild(&html.Node{Type: html.TextNode, Data: n.Data})
			return
		}
		if n.Type != html.ElementNode || strings.Contains(blocked, " "+n.Data+" ") {
			return
		}
		dst := parent
		if strings.Contains(allowed, " "+n.Data+" ") {
			dst = &html.Node{Type: html.ElementNode, Data: n.Data}
			for _, a := range n.Attr {
				if n.Data == "a" && a.Key == "href" {
					u, e := url.Parse(strings.TrimSpace(a.Val))
					if e == nil {
						u = base.ResolveReference(u)
						if (u.Scheme == "https" || u.Scheme == "http" || u.Scheme == "mailto") && u.User == nil {
							dst.Attr = append(dst.Attr, html.Attribute{Key: "href", Val: u.String()})
						}
					}
				}
				if (n.Data == "td" || n.Data == "th") && (a.Key == "colspan" || a.Key == "rowspan") {
					dst.Attr = append(dst.Attr, html.Attribute{Key: a.Key, Val: a.Val})
				}
			}
			if n.Data == "a" {
				dst.Attr = append(dst.Attr, html.Attribute{Key: "target", Val: "_blank"}, html.Attribute{Key: "rel", Val: "noopener noreferrer"})
			}
			parent.AppendChild(dst)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			clean(c, dst)
		}
	}
	safe := &html.Node{Type: html.ElementNode, Data: "div"}
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		clean(c, safe)
	}
	var out bytes.Buffer
	if err := html.Render(&out, safe); err != nil {
		return "", false, err
	}
	return template.HTML(out.String()), formatted, nil
}

var rtprReader = template.Must(template.New("article").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>{{.Title}} — RTPR</title><style>body{background:#111c2d;color:#e5edf9;max-width:1000px;margin:2rem auto;padding:0 1rem;font:16px/1.6 system-ui}.article{white-space:pre-wrap;overflow-wrap:anywhere}.article.formatted{white-space:normal}a{color:#79c9ff}table{border-collapse:collapse;display:block;overflow:auto;max-width:100%}td,th{padding:.4rem;border:1px solid #52657b}h1{line-height:1.2}</style><a href="{{.URL}}" target="_blank" rel="noopener noreferrer">Open on RTPR ↗</a><h1>{{.Title}}</h1><div class="article{{if .Formatted}} formatted{{end}}">{{.Body}}</div></html>`))

func rtprArticleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if rtprAPIKey == "" {
		http.Error(w, "RTPR is not configured", 404)
		return
	}
	endpoint := "https://rtpr.io/a/" + url.PathEscape(r.URL.Query().Get("id"))
	if _, ok := rtprArticleID(endpoint); !ok {
		http.Error(w, "invalid article", 400)
		return
	}
	raw, err := rtprGet(r.Context(), endpoint, false)
	if err != nil {
		http.Error(w, "RTPR article unavailable; try again later", 502)
		return
	}
	body, formatted, err := rtprArticleContent(raw, endpoint)
	if err != nil {
		http.Error(w, "RTPR article content unavailable", 502)
		return
	}
	title, _, _ := rtprHeadline(raw)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	_ = rtprReader.Execute(w, struct {
		URL, Title string
		Body       template.HTML
		Formatted  bool
	}{endpoint, title, body, formatted})
}
