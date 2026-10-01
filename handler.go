package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	formatRSS  = "rss"
	formatAtom = "atom"

	defaultLimit = 25
	maxLimit     = 100
)

// Kinds of feed.
const (
	kindSubreddit    = "subreddit"     // /r/{sub}/{sort}
	kindSubComments  = "sub-comments"  // /r/{sub}/comments
	kindPostComments = "post-comments" // /r/{sub}/comments/{id}
	kindUser         = "user"          // /user/{name}/{where}
)

var (
	// One or more subreddit names joined by "+", e.g. "linux+golang".
	subredditRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]{1,20}(\+[A-Za-z0-9][A-Za-z0-9_]{1,20})*$`)
	usernameRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{3,20}$`)
	postIDRe    = regexp.MustCompile(`^[a-z0-9]{1,13}$`)

	subredditSorts   = map[string]bool{"hot": true, "new": true, "top": true, "controversial": true, "rising": true}
	userSorts        = map[string]bool{"hot": true, "new": true, "top": true, "controversial": true}
	commentSorts     = map[string]bool{"confidence": true, "top": true, "new": true, "controversial": true, "old": true, "qa": true}
	userWheres       = map[string]bool{"overview": true, "submitted": true, "comments": true}
	validTimeframes  = map[string]bool{"hour": true, "day": true, "week": true, "month": true, "year": true, "all": true}
	errNotAFeedRoute = errors.New("Not found. Try /r/{subreddit}/.rss, /r/{subreddit}/{hot|new|top|controversial|rising}/.rss, " +
		"/r/{subreddit}/comments/.rss, /r/{subreddit}/comments/{post id}/.rss or /user/{name}/.rss (or .atom).")
)

// Server serves Reddit-compatible feed URLs.
type Server struct {
	cfg      *Config
	reddit   *RedditClient
	listings *Cache[[]Item]
	abouts   *Cache[*About]
}

// NewServer wires the HTTP handlers to a Reddit client and its caches.
func NewServer(cfg *Config, client *RedditClient) *Server {
	return &Server{
		cfg:      cfg,
		reddit:   client,
		listings: NewCache[[]Item](cfg.CacheTTL, cfg.CacheTTL, 6*time.Hour),
		abouts:   NewCache[*About](24*time.Hour, cfg.CacheTTL, 7*24*time.Hour),
	}
}

// Handler returns the server's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /r/", s.handleFeed)
	mux.HandleFunc("GET /user/", s.handleFeed)
	mux.HandleFunc("GET /u/", s.handleFeed)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /{$}", s.handleIndex)
	return mux
}

// feedRequest is a parsed feed URL.
type feedRequest struct {
	Kind      string
	Format    string
	Subreddit string // subreddit feeds
	Sort      string
	PostID    string // post comment feeds
	User      string // user feeds
	Where     string // user feeds: overview, submitted or comments

	// The Reddit API call that backs the feed.
	APIPath  string
	APIQuery url.Values
}

// parseFeedPath understands the same shapes Reddit does, each with ".rss" or
// ".atom", with or without a slash before the extension:
//
//	/r/{sub}/.rss                      hot posts
//	/r/{sub}/{sort}/.rss               hot, new, top, controversial, rising
//	/r/{sub}/comments/.rss             newest comments in the subreddit
//	/r/{sub}/comments/{id}/[slug/].rss a post and its comments
//	/user/{name}/.rss                  posts and comments by a user (also /u/)
//	/user/{name}/{where}/.rss          overview, submitted or comments
func parseFeedPath(path string) (*feedRequest, bool) {
	req := &feedRequest{}
	switch {
	case strings.HasSuffix(path, ".rss"):
		req.Format = formatRSS
	case strings.HasSuffix(path, ".atom"):
		req.Format = formatAtom
	default:
		return nil, false
	}
	path = strings.Trim(strings.TrimSuffix(path, "."+req.Format), "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return nil, false
	}

	switch parts[0] {
	case "r":
		req.Subreddit = parts[1]
		if !subredditRe.MatchString(req.Subreddit) {
			return nil, false
		}
		rest := parts[2:]
		switch {
		case len(rest) == 0:
			req.Kind, req.Sort = kindSubreddit, "hot"
		case len(rest) == 1 && strings.EqualFold(rest[0], "comments"):
			req.Kind = kindSubComments
		case len(rest) == 1 && subredditSorts[strings.ToLower(rest[0])]:
			req.Kind, req.Sort = kindSubreddit, strings.ToLower(rest[0])
		case (len(rest) == 2 || len(rest) == 3) && rest[0] == "comments" && postIDRe.MatchString(rest[1]):
			// A trailing title slug is accepted and ignored, as on Reddit.
			req.Kind, req.PostID = kindPostComments, rest[1]
		default:
			return nil, false
		}

	case "user", "u":
		req.Kind, req.User, req.Where = kindUser, parts[1], "overview"
		if !usernameRe.MatchString(req.User) {
			return nil, false
		}
		switch len(parts) {
		case 2:
		case 3:
			req.Where = strings.ToLower(parts[2])
			if !userWheres[req.Where] {
				return nil, false
			}
		default:
			return nil, false
		}

	default:
		return nil, false
	}
	return req, true
}

// parseFeedRequest parses the URL and its query string, and works out the
// Reddit API call behind it. A non-nil error is a message for a 4xx reply.
func parseFeedRequest(u *url.URL) (*feedRequest, int, error) {
	req, ok := parseFeedPath(u.Path)
	if !ok {
		return nil, http.StatusNotFound, errNotAFeedRoute
	}

	q := u.Query()
	api := url.Values{}

	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, http.StatusBadRequest, errors.New("invalid limit")
		}
		limit = min(n, maxLimit)
	}
	api.Set("limit", strconv.Itoa(limit))

	// User feeds and comment threads take their sort from the query string.
	if v := strings.ToLower(q.Get("sort")); v != "" {
		switch {
		case req.Kind == kindUser && userSorts[v], req.Kind == kindPostComments && commentSorts[v]:
			req.Sort = v
			api.Set("sort", v)
		case req.Kind == kindUser || req.Kind == kindPostComments:
			return nil, http.StatusBadRequest, errors.New("invalid sort")
		}
	}

	if req.Sort == "top" || req.Sort == "controversial" {
		if t := strings.ToLower(q.Get("t")); t != "" {
			if !validTimeframes[t] {
				return nil, http.StatusBadRequest, errors.New("invalid t (expected hour, day, week, month, year or all)")
			}
			api.Set("t", t)
		}
	}

	switch req.Kind {
	case kindSubreddit:
		req.APIPath = "/r/" + req.Subreddit + "/" + req.Sort
	case kindSubComments:
		req.APIPath = "/r/" + req.Subreddit + "/comments"
	case kindPostComments:
		req.APIPath = "/r/" + req.Subreddit + "/comments/" + req.PostID
	case kindUser:
		req.APIPath = "/user/" + req.User + "/" + req.Where
	}
	req.APIQuery = api
	return req, 0, nil
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	req, status, err := parseFeedRequest(r.URL)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	feed, fetched, expires, err := s.buildFeed(r, req)
	if err != nil {
		s.writeError(w, req, err)
		return
	}

	var body []byte
	var contentType string
	if req.Format == formatAtom {
		body, err = RenderAtom(feed)
		contentType = "application/atom+xml; charset=UTF-8"
	} else {
		body, err = RenderRSS(feed)
		contentType = "application/rss+xml; charset=UTF-8"
	}
	if err != nil {
		log.Printf("Rendering %s feed for %s failed: %v\n", req.Format, req.APIPath, err)
		http.Error(w, "failed to render feed", http.StatusInternalServerError)
		return
	}

	h := fnv.New64a()
	h.Write(body)
	maxAge := max(int(time.Until(expires).Seconds()), 0)

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", fmt.Sprintf(`"%x"`, h.Sum64()))
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	http.ServeContent(w, r, "", fetched, bytes.NewReader(body))
}

// buildFeed fetches (or reuses) the listing and the subreddit's details.
func (s *Server) buildFeed(r *http.Request, req *feedRequest) (*Feed, time.Time, time.Time, error) {
	// Subreddit and user names are case-insensitive on Reddit.
	key := strings.ToLower(req.APIPath) + "?" + req.APIQuery.Encode()

	items, fetched, expires, err := s.listings.Get(key, func() ([]Item, error) {
		// Detached from the request: other requests may be waiting on this.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		log.Printf("Fetching %s?%s from Reddit.\n", req.APIPath, req.APIQuery.Encode())
		return s.reddit.Listing(ctx, req.APIPath, req.APIQuery)
	})
	if err != nil {
		return nil, time.Time{}, time.Time{}, err
	}

	feed := &Feed{
		Subreddit: req.Subreddit,
		Icon:      defaultIcon,
		SelfURL:   s.baseURL(r) + r.URL.RequestURI(),
		LinkBase:  s.cfg.LinkBase,
		Updated:   fetched,
		Items:     items,
	}

	switch req.Kind {
	case kindSubreddit, kindSubComments:
		feed.Title = "r/" + req.Subreddit
		feed.AlternateURL = s.cfg.LinkBase + "/r/" + req.Subreddit + "/"
		err := s.addSubredditDetails(feed, req.Subreddit)
		// Reddit answers some unknown subreddit names with an empty listing
		// instead of a 404; their /about does fail, though.
		if len(items) == 0 && (errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden)) {
			return nil, time.Time{}, time.Time{}, err
		}
		switch {
		case req.Kind == kindSubComments:
			feed.Title += " (comments)"
			feed.AlternateURL += "comments/"
		case req.Sort != "hot":
			feed.Title += " (" + req.Sort + ")"
			feed.AlternateURL += req.Sort + "/"
		}

	case kindPostComments:
		feed.AlternateURL = s.cfg.LinkBase + "/r/" + req.Subreddit + "/comments/" + req.PostID + "/"
		feed.Title = "Comments on post " + req.PostID
		if len(items) > 0 && !items[0].IsComment() {
			feed.Title = items[0].Title
			feed.AlternateURL = s.cfg.LinkBase + items[0].Permalink
		}

	case kindUser:
		// No subreddit category: a user's items come from many subreddits.
		feed.Subreddit = ""
		feed.AlternateURL = s.cfg.LinkBase + "/user/" + req.User + "/"
		switch req.Where {
		case "overview":
			feed.Title = "overview for " + req.User
		case "submitted":
			feed.Title = "submitted by " + req.User
			feed.AlternateURL += "submitted/"
		case "comments":
			feed.Title = "comments by " + req.User
			feed.AlternateURL += "comments/"
		}
	}

	return feed, fetched, expires, nil
}

// addSubredditDetails fills in the subreddit's title, description and icon.
// Failing to get them is not fatal; the error is returned for the caller to
// judge.
func (s *Server) addSubredditDetails(feed *Feed, subreddit string) error {
	sub := strings.ToLower(subreddit)
	// Multireddits ("a+b") have no single /about to describe them.
	if strings.Contains(sub, "+") {
		return nil
	}

	about, _, _, err := s.abouts.Get(sub, func() (*About, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return s.reddit.About(ctx, subreddit)
	})
	if err != nil {
		// The listing worked, so a feed without the extras is still useful.
		log.Printf("Could not fetch details of r/%s: %v\n", subreddit, err)
		return err
	}
	if about.Title != "" {
		feed.Title = about.Title
	}
	feed.Subtitle = about.PublicDescription
	if icon := about.Icon(); icon != "" {
		feed.Icon = icon
	}
	return nil
}

func (s *Server) writeError(w http.ResponseWriter, req *feedRequest, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found on Reddit", http.StatusNotFound)
	case errors.Is(err, ErrForbidden):
		http.Error(w, "private, quarantined, banned or suspended", http.StatusForbidden)
	case errors.Is(err, ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		log.Printf("Fetching %s failed: %v\n", req.APIPath, err)
		http.Error(w, "could not fetch from Reddit", http.StatusBadGateway)
	}
}

// baseURL is the public URL of this server, used for self links.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return s.cfg.BaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "http" || proto == "https" {
		scheme = proto
	}
	return scheme + "://" + r.Host
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, `reddit-rss: Reddit-style RSS/Atom feeds, served from the Reddit API.

  %[1]s/r/selfhosted/.rss                       hot posts, RSS 2.0
  %[1]s/r/selfhosted/.atom                      hot posts, Atom
  %[1]s/r/selfhosted/new/.rss                   new posts
  %[1]s/r/selfhosted/top/.rss?t=week            top posts (t: hour, day, week, month, year, all)
  %[1]s/r/selfhosted/controversial/.rss         controversial posts
  %[1]s/r/selfhosted/rising/.rss                rising posts
  %[1]s/r/linux+golang/new/.rss                 several subreddits at once
  %[1]s/r/selfhosted/comments/.rss              newest comments in a subreddit
  %[1]s/r/selfhosted/comments/{post id}/.rss    a post and its comments (?sort=new, top, old, ...)
  %[1]s/user/spez/.rss                          a user's posts and comments (?sort=new, top, ...)
  %[1]s/user/spez/submitted/.rss                a user's posts
  %[1]s/user/spez/comments/.rss                 a user's comments

Optional: ?limit=1..100 (default 25). Feeds are cached for %[2]s.
`, base, s.cfg.CacheTTL)
}
