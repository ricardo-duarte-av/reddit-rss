package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultAuthURL = "https://www.reddit.com/api/v1/access_token"
	defaultAPIURL  = "https://oauth.reddit.com"
)

// ErrNotFound is returned when the subreddit does not exist (or is banned).
var ErrNotFound = errors.New("subreddit not found")

// ErrForbidden is returned when the subreddit is private or quarantined.
var ErrForbidden = errors.New("subreddit is private or quarantined")

// RedditClient is a small read-only Reddit API client.
//
// It authenticates with OAuth2: the password grant when a username and
// password are configured (a "script" app), otherwise the client_credentials
// grant (application-only auth, no Reddit account needed).
type RedditClient struct {
	clientID     string
	clientSecret string
	username     string
	password     string
	userAgent    string

	authURL string
	apiURL  string
	http    *http.Client

	tokenMu     sync.Mutex
	token       string
	tokenExpiry time.Time

	// Upstream requests are serialised and spaced out so a burst of cache
	// misses cannot blow through Reddit's rate limit (100 requests/minute
	// per OAuth client).
	paceMu      sync.Mutex
	minInterval time.Duration
	nextAllowed time.Time
}

// Item is a post ("t3") or a comment ("t1"). Both kinds decode into the same
// struct; the fields that do not apply to a kind are left empty.
type Item struct {
	Kind       string  `json:"-"`
	ID         string  `json:"id"`
	Name       string  `json:"name"` // fullname, e.g. "t3_abc"
	Author     string  `json:"author"`
	Subreddit  string  `json:"subreddit"`
	Permalink  string  `json:"permalink"`
	CreatedUTC float64 `json:"created_utc"`
	Edited     any     `json:"edited"` // false, or a timestamp

	// Posts.
	Title        string `json:"title"`
	URL          string `json:"url"`
	SelftextHTML string `json:"selftext_html"`
	IsSelf       bool   `json:"is_self"`
	Thumbnail    string `json:"thumbnail"`

	// Comments.
	BodyHTML  string `json:"body_html"`
	LinkTitle string `json:"link_title"` // title of the post commented on

	Replies json.RawMessage `json:"replies"` // "" or a nested listing
}

// IsComment reports whether the item is a comment rather than a post.
func (it *Item) IsComment() bool {
	return it.Kind == "t1"
}

// Created returns the item's creation time.
func (it *Item) Created() time.Time {
	return unixFloat(it.CreatedUTC)
}

// Updated returns the time the item was last edited, or its creation time.
func (it *Item) Updated() time.Time {
	if ts, ok := it.Edited.(float64); ok && ts > 0 {
		return unixFloat(ts)
	}
	return it.Created()
}

// About is the subset of a subreddit's /about we put in feed headers.
type About struct {
	Title             string `json:"title"`
	PublicDescription string `json:"public_description"`
	IconImg           string `json:"icon_img"`
	CommunityIcon     string `json:"community_icon"`
}

// Icon returns the subreddit's icon URL, if it has one.
func (a *About) Icon() string {
	if a.CommunityIcon != "" {
		return a.CommunityIcon
	}
	return a.IconImg
}

// NewRedditClient builds a client against the real Reddit API.
func NewRedditClient(cfg *Config) *RedditClient {
	return &RedditClient{
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		username:     cfg.Username,
		password:     cfg.Password,
		userAgent:    cfg.UserAgent,
		authURL:      defaultAuthURL,
		apiURL:       defaultAPIURL,
		http:         &http.Client{Timeout: 20 * time.Second},
		minInterval:  cfg.MinRequestInterval,
	}
}

type listing struct {
	Kind string `json:"kind"`
	Data struct {
		Children []struct {
			Kind string `json:"kind"`
			Data Item   `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

// Listing fetches any listing endpoint (e.g. "/r/selfhosted/new",
// "/r/selfhosted/comments", "/user/spez/overview") and returns its posts and
// comments in order.
//
// A post's comment page ("/r/{sub}/comments/{id}") answers with two
// listings, the post and its comment tree; it comes back as the post followed
// by every loaded comment, depth first.
func (c *RedditClient) Listing(ctx context.Context, path string, q url.Values) ([]Item, error) {
	q.Set("raw_json", "1")

	var raw json.RawMessage
	if err := c.get(ctx, path, q, &raw); err != nil {
		return nil, err
	}

	var listings []listing
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(raw, &listings); err != nil {
			return nil, fmt.Errorf("decoding reddit response for %s: %w", path, err)
		}
	} else {
		var l listing
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, fmt.Errorf("decoding reddit response for %s: %w", path, err)
		}
		listings = []listing{l}
	}

	var items []Item
	for _, l := range listings {
		// An unknown subreddit or user is sometimes answered with a search
		// redirect or a non-listing body rather than a 404.
		if l.Kind != "Listing" {
			return nil, ErrNotFound
		}
		items = appendListing(items, &l)
	}

	// Comments under a post do not carry the post's title; fill it in.
	if len(items) > 0 && !items[0].IsComment() {
		for i := 1; i < len(items); i++ {
			if items[i].IsComment() && items[i].LinkTitle == "" {
				items[i].LinkTitle = items[0].Title
			}
		}
	}
	return items, nil
}

// appendListing flattens a listing, and any reply trees inside it, into items.
func appendListing(items []Item, l *listing) []Item {
	for _, child := range l.Data.Children {
		if child.Kind != "t1" && child.Kind != "t3" {
			continue // "more" placeholders and the like
		}
		item := child.Data
		item.Kind = child.Kind
		replies := item.Replies
		item.Replies = nil
		items = append(items, item)

		var nested listing
		if len(replies) > 0 && replies[0] == '{' && json.Unmarshal(replies, &nested) == nil {
			items = appendListing(items, &nested)
		}
	}
	return items
}

// About fetches a subreddit's title, description and icon.
func (c *RedditClient) About(ctx context.Context, subreddit string) (*About, error) {
	var about struct {
		Kind string `json:"kind"`
		Data About  `json:"data"`
	}
	if err := c.get(ctx, "/r/"+subreddit+"/about", url.Values{"raw_json": {"1"}}, &about); err != nil {
		return nil, err
	}
	if about.Kind != "t5" {
		return nil, ErrNotFound
	}
	return &about.Data, nil
}

func (c *RedditClient) get(ctx context.Context, path string, q url.Values, out any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	if err := c.pace(ctx); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reddit request failed: %w", err)
	}
	defer resp.Body.Close()

	c.respectRateLimit(resp.Header)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusForbidden:
		return ErrForbidden
	case resp.StatusCode == http.StatusUnauthorized:
		// The token was revoked or expired early; fetch a new one next time.
		c.tokenMu.Lock()
		c.token = ""
		c.tokenMu.Unlock()
		return fmt.Errorf("reddit returned 401 for %s", path)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// e.g. a redirect to /subreddits/search for unknown subreddits.
		return ErrNotFound
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("reddit returned %s for %s: %s", resp.Status, path, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding reddit response for %s: %w", path, err)
	}
	return nil
}

// accessToken returns a cached OAuth token, fetching a new one when needed.
func (c *RedditClient) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	form := url.Values{}
	if c.username != "" && c.password != "" {
		form.Set("grant_type", "password")
		form.Set("username", c.username)
		form.Set("password", c.password)
	} else {
		form.Set("grant_type", "client_credentials")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("reddit token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("reddit token request returned %s (check REDDIT_CLIENT_ID/SECRET): %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       any    `json:"error"` // a string, or sometimes a status code
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decoding reddit token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("reddit token request failed: %v", tok.Error)
	}

	c.token = tok.AccessToken
	// Renew a minute early so a request never goes out with a dying token.
	c.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second - time.Minute)
	log.Printf("Obtained Reddit access token (expires in %ds).\n", tok.ExpiresIn)
	return c.token, nil
}

// maxPaceWait is how long a request will queue for an upstream slot before
// giving up, so callers can fall back to stale data instead of hanging.
const maxPaceWait = 15 * time.Second

// ErrRateLimited is returned when no upstream slot is available soon enough.
var ErrRateLimited = errors.New("reddit rate limit reached, try again later")

// pace blocks until the next upstream request is allowed.
func (c *RedditClient) pace(ctx context.Context) error {
	c.paceMu.Lock()
	now := time.Now()
	wait := c.nextAllowed.Sub(now)
	if wait < 0 {
		wait = 0
	}
	if wait > maxPaceWait {
		c.paceMu.Unlock()
		return ErrRateLimited
	}
	c.nextAllowed = now.Add(wait + c.minInterval)
	c.paceMu.Unlock()

	if wait == 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// respectRateLimit pushes the next allowed request past the reset window when
// Reddit says we have used up our quota.
func (c *RedditClient) respectRateLimit(h http.Header) {
	remaining, err1 := strconv.ParseFloat(h.Get("X-Ratelimit-Remaining"), 64)
	reset, err2 := strconv.ParseFloat(h.Get("X-Ratelimit-Reset"), 64)
	if err1 != nil || err2 != nil || remaining >= 1 {
		return
	}

	until := time.Now().Add(time.Duration(reset * float64(time.Second)))
	log.Printf("Reddit rate limit exhausted, holding upstream requests for %.0fs.\n", reset)

	c.paceMu.Lock()
	if until.After(c.nextAllowed) {
		c.nextAllowed = until
	}
	c.paceMu.Unlock()
}

func unixFloat(ts float64) time.Time {
	return time.Unix(int64(ts), 0).UTC()
}
