package main

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const listingJSON = `{"kind":"Listing","data":{"children":[
 {"kind":"t3","data":{"id":"abc","name":"t3_abc","title":"Self post & <stuff>","author":"alice",
  "subreddit":"selfhosted","permalink":"/r/selfhosted/comments/abc/self_post/",
  "url":"https://www.reddit.com/r/selfhosted/comments/abc/self_post/","is_self":true,
  "selftext_html":"<!-- SC_OFF --><div class=\"md\"><p>Hello</p></div><!-- SC_ON -->",
  "thumbnail":"self","created_utc":1700000000,"edited":1700000100}},
 {"kind":"t3","data":{"id":"def","name":"t3_def","title":"Link post","author":"[deleted]",
  "subreddit":"selfhosted","permalink":"/r/selfhosted/comments/def/link_post/",
  "url":"https://example.com/?a=1&b=2","is_self":false,
  "thumbnail":"https://b.thumbs.redditmedia.com/x.jpg","created_utc":1700000200,"edited":false}}
]}}`

const commentJSON = `{"kind":"t1","data":{"id":"c1","name":"t1_c1","author":"bob","subreddit":"selfhosted",
  "permalink":"/r/selfhosted/comments/abc/self_post/c1/","body_html":"<div class=\"md\"><p>Nice</p></div>",
  "link_title":"Self post & <stuff>","created_utc":1700000300,"edited":false,"replies":""}}`

const subCommentsJSON = `{"kind":"Listing","data":{"children":[` + commentJSON + `]}}`

// A post's comment page: the post, then its comment tree (with a reply and a
// "more" placeholder).
const postCommentsJSON = `[
 {"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"abc","name":"t3_abc","title":"Self post","author":"alice",
  "subreddit":"selfhosted","permalink":"/r/selfhosted/comments/abc/self_post/","is_self":true,"created_utc":1700000000}}]}},
 {"kind":"Listing","data":{"children":[
  {"kind":"t1","data":{"id":"c1","name":"t1_c1","author":"bob","subreddit":"selfhosted","body_html":"<p>top</p>",
   "permalink":"/r/selfhosted/comments/abc/self_post/c1/","created_utc":1700000300,
   "replies":{"kind":"Listing","data":{"children":[
    {"kind":"t1","data":{"id":"c2","name":"t1_c2","author":"carol","subreddit":"selfhosted","body_html":"<p>reply</p>",
     "permalink":"/r/selfhosted/comments/abc/self_post/c2/","created_utc":1700000400,"replies":""}},
    {"kind":"more","data":{"count":3,"children":["c3","c4","c5"]}}]}}}},
  {"kind":"t1","data":{"id":"c6","name":"t1_c6","author":"dave","subreddit":"selfhosted","body_html":"<p>second</p>",
   "permalink":"/r/selfhosted/comments/abc/self_post/c6/","created_utc":1700000500,"replies":""}}
 ]}}
]`

const aboutJSON = `{"kind":"t5","data":{"title":"Selfhosted","public_description":"A place to share alternatives","community_icon":"https://styles.redditmedia.com/icon.png"}}`

type fakeReddit struct {
	listingCalls atomic.Int32
	aboutCalls   atomic.Int32
	failedCalls  atomic.Int32
	fail         atomic.Bool
	lastQuery    atomic.Value
}

func newTestServer(t *testing.T) (*fakeReddit, *httptest.Server) {
	t.Helper()
	fake := &fakeReddit{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/access_token":
			if id, secret, _ := r.BasicAuth(); id != "id" || secret != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, `{"access_token":"tok","expires_in":3600}`)
			return
		case r.Header.Get("Authorization") != "bearer tok":
			w.WriteHeader(http.StatusUnauthorized)
			return
		case fake.fail.Load():
			fake.failedCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/r/selfhosted/new":
			fake.listingCalls.Add(1)
			if r.URL.Query().Get("raw_json") != "1" {
				t.Errorf("raw_json not set: %s", r.URL)
			}
			io.WriteString(w, listingJSON)
		case r.URL.Path == "/r/selfhosted/comments":
			io.WriteString(w, subCommentsJSON)
		case r.URL.Path == "/r/selfhosted/comments/abc":
			fake.lastQuery.Store(r.URL.RawQuery)
			io.WriteString(w, postCommentsJSON)
		case r.URL.Path == "/user/alice/overview":
			fake.lastQuery.Store(r.URL.RawQuery)
			io.WriteString(w, strings.Replace(listingJSON, `"children":[`, `"children":[`+commentJSON+`,`, 1))
		case r.URL.Path == "/r/selfhosted/about":
			fake.aboutCalls.Add(1)
			io.WriteString(w, aboutJSON)
		case r.URL.Path == "/r/private/hot":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	cfg := &Config{
		ClientID: "id", ClientSecret: "secret", UserAgent: "test",
		LinkBase: "https://www.reddit.com", CacheTTL: 15 * time.Minute,
	}
	client := NewRedditClient(cfg)
	client.authURL = upstream.URL + "/api/v1/access_token"
	client.apiURL = upstream.URL

	srv := httptest.NewServer(NewServer(cfg, client).Handler())
	t.Cleanup(srv.Close)
	return fake, srv
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestParseFeedPath(t *testing.T) {
	tests := []struct {
		path, sub, sort, format string
		ok                      bool
	}{
		{"/r/selfhosted/.rss", "selfhosted", "hot", "rss", true},
		{"/r/selfhosted.rss", "selfhosted", "hot", "rss", true},
		{"/r/selfhosted/new/.rss", "selfhosted", "new", "rss", true},
		{"/r/selfhosted/new.atom", "selfhosted", "new", "atom", true},
		{"/r/selfhosted/controversial/.atom", "selfhosted", "controversial", "atom", true},
		{"/r/linux+golang/top/.rss", "linux+golang", "top", "rss", true},
		{"/r/selfhosted/comments/.rss", "selfhosted", "", "rss", true},
		{"/r/selfhosted/comments/abc123/.rss", "selfhosted", "", "rss", true},
		{"/r/selfhosted/comments/abc123/some_title/.atom", "selfhosted", "", "atom", true},
		{"/user/spez/.rss", "", "", "rss", true},
		{"/u/spez/submitted.rss", "", "", "rss", true},
		{"/user/spez/comments/.atom", "", "", "atom", true},
		{"/user/spez/upvoted/.rss", "", "", "", false},
		{"/user/x/.rss", "", "", "", false},
		{"/r/selfhosted/comments/ABC!/.rss", "", "", "", false},
		{"/r/selfhosted/", "", "", "", false},
		{"/r/selfhosted/bogus/.rss", "", "", "", false},
		{"/r/../etc/.rss", "", "", "", false},
		{"/r/a/.rss", "", "", "", false},
		{"/r/selfhosted/new/extra/.rss", "", "", "", false},
	}
	for _, tt := range tests {
		req, ok := parseFeedPath(tt.path)
		if ok != tt.ok {
			t.Errorf("%s: ok = %v, want %v", tt.path, ok, tt.ok)
			continue
		}
		if ok && (req.Subreddit != tt.sub || req.Sort != tt.sort || req.Format != tt.format) {
			t.Errorf("%s: got %+v", tt.path, req)
		}
	}
}

func TestRSSFeed(t *testing.T) {
	fake, srv := newTestServer(t)

	resp, body := get(t, srv.URL+"/r/selfhosted/new/.rss")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/rss+xml") {
		t.Errorf("content type %q", ct)
	}

	var doc struct {
		Channel struct {
			Title string `xml:"title"`
			Items []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				GUID        string `xml:"guid"`
				Description string `xml:"description"`
				Creator     string `xml:"http://purl.org/dc/elements/1.1/ creator"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("invalid XML: %v\n%s", err, body)
	}
	if doc.Channel.Title != "Selfhosted (new)" {
		t.Errorf("title %q", doc.Channel.Title)
	}
	if len(doc.Channel.Items) != 2 {
		t.Fatalf("got %d items", len(doc.Channel.Items))
	}
	first, second := doc.Channel.Items[0], doc.Channel.Items[1]
	if first.Title != "Self post & <stuff>" || first.GUID != "t3_abc" || first.Creator != "/u/alice" {
		t.Errorf("first item %+v", first)
	}
	if first.Link != "https://www.reddit.com/r/selfhosted/comments/abc/self_post/" {
		t.Errorf("link %q", first.Link)
	}
	if !strings.Contains(first.Description, `<div class="md"><p>Hello</p></div>`) {
		t.Errorf("self text missing: %q", first.Description)
	}
	if !strings.Contains(second.Description, `<img src="https://b.thumbs.redditmedia.com/x.jpg"`) ||
		!strings.Contains(second.Description, `href="https://example.com/?a=1&amp;b=2">[link]`) {
		t.Errorf("link post content: %q", second.Description)
	}

	// Same listing in Atom, and repeat requests, must be served from cache.
	get(t, srv.URL+"/r/selfhosted/new/.atom")
	get(t, srv.URL+"/r/SelfHosted/new.rss")
	if n := fake.listingCalls.Load(); n != 1 {
		t.Errorf("listing fetched %d times, want 1", n)
	}
	if n := fake.aboutCalls.Load(); n != 1 {
		t.Errorf("about fetched %d times, want 1", n)
	}
}

func TestAtomFeed(t *testing.T) {
	_, srv := newTestServer(t)

	resp, body := get(t, srv.URL+"/r/selfhosted/new/.atom")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/atom+xml") {
		t.Errorf("content type %q", ct)
	}

	var feed struct {
		XMLName  xml.Name `xml:"http://www.w3.org/2005/Atom feed"`
		Title    string   `xml:"title"`
		Subtitle string   `xml:"subtitle"`
		Icon     string   `xml:"icon"`
		Entries  []struct {
			ID        string `xml:"id"`
			Updated   string `xml:"updated"`
			Published string `xml:"published"`
			Author    struct {
				Name string `xml:"name"`
				URI  string `xml:"uri"`
			} `xml:"author"`
			Thumbnail struct {
				URL string `xml:"url,attr"`
			} `xml:"http://search.yahoo.com/mrss/ thumbnail"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal([]byte(body), &feed); err != nil {
		t.Fatalf("invalid XML: %v\n%s", err, body)
	}
	if feed.Subtitle != "A place to share alternatives" || feed.Icon != "https://styles.redditmedia.com/icon.png" {
		t.Errorf("feed header %+v", feed)
	}
	if len(feed.Entries) != 2 {
		t.Fatalf("got %d entries", len(feed.Entries))
	}
	e0, e1 := feed.Entries[0], feed.Entries[1]
	if e0.ID != "t3_abc" || e0.Published != "2023-11-14T22:13:20Z" || e0.Updated != "2023-11-14T22:15:00Z" {
		t.Errorf("entry 0 %+v", e0)
	}
	if e0.Author.URI != "https://www.reddit.com/user/alice" || e1.Author.URI != "" {
		t.Errorf("author URIs %q %q", e0.Author.URI, e1.Author.URI)
	}
	if e1.Thumbnail.URL != "https://b.thumbs.redditmedia.com/x.jpg" {
		t.Errorf("thumbnail %q", e1.Thumbnail.URL)
	}
}

func TestErrors(t *testing.T) {
	_, srv := newTestServer(t)

	for path, want := range map[string]int{
		"/r/doesnotexist/.rss":            http.StatusNotFound,
		"/r/private/.rss":                 http.StatusForbidden,
		"/r/selfhosted/":                  http.StatusNotFound,
		"/r/selfhosted/new/.rss?limit=x":  http.StatusBadRequest,
		"/r/selfhosted/top/.rss?t=decade": http.StatusBadRequest,
	} {
		if resp, body := get(t, srv.URL+path); resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d (%s)", path, resp.StatusCode, want, body)
		}
	}
}

func TestConditionalGet(t *testing.T) {
	_, srv := newTestServer(t)

	resp, _ := get(t, srv.URL+"/r/selfhosted/new/.rss")
	etag := resp.Header.Get("ETag")
	if etag == "" || resp.Header.Get("Last-Modified") == "" {
		t.Fatal("missing ETag / Last-Modified")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/r/selfhosted/new/.rss", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("status %d, want 304", resp2.StatusCode)
	}
}

func TestCacheServesStaleOnError(t *testing.T) {
	c := NewCache[string](time.Millisecond, time.Millisecond, time.Hour)

	v, _, _, err := c.Get("k", func() (string, error) { return "good", nil })
	if err != nil || v != "good" {
		t.Fatalf("got %q, %v", v, err)
	}
	time.Sleep(5 * time.Millisecond)

	v, _, expires, err := c.Get("k", func() (string, error) { return "", errors.New("boom") })
	if err != nil || v != "good" {
		t.Fatalf("expected stale value, got %q, %v", v, err)
	}
	if time.Until(expires) < 30*time.Second {
		t.Errorf("stale value should back off before retrying, expires in %s", time.Until(expires))
	}
}

func TestCacheCoalescesConcurrentMisses(t *testing.T) {
	c := NewCache[int](time.Minute, time.Minute, time.Hour)
	var calls atomic.Int32
	release := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Get("k", func() (int, error) {
				calls.Add(1)
				<-release
				return 42, nil
			})
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("fetch called %d times, want 1", n)
	}
}

func TestUpstreamFailureIsCachedBriefly(t *testing.T) {
	fake, srv := newTestServer(t)
	fake.fail.Store(true)

	for i := 0; i < 3; i++ {
		if resp, _ := get(t, srv.URL+"/r/selfhosted/new/.rss"); resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status %d, want 502", resp.StatusCode)
		}
	}
	if n := fake.failedCalls.Load(); n != 1 {
		t.Errorf("reddit hit %d times during an outage, want 1", n)
	}
}

func TestParseFeedRequestAPICall(t *testing.T) {
	tests := map[string]string{
		"/r/selfhosted/.rss":                             "/r/selfhosted/hot?limit=25",
		"/r/selfhosted/top/.rss?t=week&limit=500":        "/r/selfhosted/top?limit=100&t=week",
		"/r/selfhosted/new/.rss?t=week":                  "/r/selfhosted/new?limit=25",
		"/r/selfhosted/comments/.rss":                    "/r/selfhosted/comments?limit=25",
		"/r/selfhosted/comments/abc/title/.rss?sort=new": "/r/selfhosted/comments/abc?limit=25&sort=new",
		"/u/spez/.rss":                                   "/user/spez/overview?limit=25",
		"/user/spez/submitted/.rss?sort=top&t=all":       "/user/spez/submitted?limit=25&sort=top&t=all",
	}
	for in, want := range tests {
		u, _ := url.Parse(in)
		req, _, err := parseFeedRequest(u)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := req.APIPath + "?" + req.APIQuery.Encode(); got != want {
			t.Errorf("%s: API call %q, want %q", in, got, want)
		}
	}

	for _, in := range []string{"/user/spez/.rss?sort=rising", "/r/a_b/comments/abc/.rss?sort=hot"} {
		u, _ := url.Parse(in)
		if _, status, err := parseFeedRequest(u); err == nil || status != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d %v", in, status, err)
		}
	}
}

type rssItemT struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	Description string `xml:"description"`
}

func parseRSS(t *testing.T, body string) (title string, items []rssItemT) {
	t.Helper()
	var doc struct {
		Channel struct {
			Title string     `xml:"title"`
			Items []rssItemT `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("invalid XML: %v\n%s", err, body)
	}
	return doc.Channel.Title, doc.Channel.Items
}

func TestSubredditCommentsFeed(t *testing.T) {
	_, srv := newTestServer(t)

	resp, body := get(t, srv.URL+"/r/selfhosted/comments/.rss")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	title, items := parseRSS(t, body)
	if title != "Selfhosted (comments)" || len(items) != 1 {
		t.Fatalf("title %q, %d items", title, len(items))
	}
	it := items[0]
	if it.Title != "/u/bob on Self post & <stuff>" || it.GUID != "t1_c1" ||
		it.Link != "https://www.reddit.com/r/selfhosted/comments/abc/self_post/c1/" ||
		it.Description != `<div class="md"><p>Nice</p></div>` {
		t.Errorf("comment item %+v", it)
	}
}

func TestPostCommentsFeed(t *testing.T) {
	fake, srv := newTestServer(t)

	resp, body := get(t, srv.URL+"/r/selfhosted/comments/abc/self_post/.rss?sort=new")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if q, _ := fake.lastQuery.Load().(string); !strings.Contains(q, "sort=new") {
		t.Errorf("sort not passed to Reddit: %q", q)
	}
	title, items := parseRSS(t, body)
	if title != "Self post" {
		t.Errorf("title %q", title)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.GUID+" "+it.Title)
	}
	want := []string{
		"t3_abc Self post",
		"t1_c1 /u/bob on Self post",
		"t1_c2 /u/carol on Self post",
		"t1_c6 /u/dave on Self post",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestUserFeed(t *testing.T) {
	_, srv := newTestServer(t)

	resp, body := get(t, srv.URL+"/u/alice/.atom")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var feed struct {
		Title    string    `xml:"title"`
		Category *struct{} `xml:"category"`
		Entries  []struct {
			ID    string `xml:"id"`
			Title string `xml:"title"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal([]byte(body), &feed); err != nil {
		t.Fatalf("invalid XML: %v\n%s", err, body)
	}
	if feed.Title != "overview for alice" || feed.Category != nil {
		t.Errorf("feed header: title %q, category %v", feed.Title, feed.Category)
	}
	if len(feed.Entries) != 3 || feed.Entries[0].ID != "t1_c1" || feed.Entries[1].ID != "t3_abc" {
		t.Errorf("entries %+v", feed.Entries)
	}

	if resp, _ := get(t, srv.URL+"/user/nobody/.rss"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown user: status %d, want 404", resp.StatusCode)
	}
}
