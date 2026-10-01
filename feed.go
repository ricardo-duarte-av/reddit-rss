package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"html"
	"strings"
	"time"
)

const defaultIcon = "https://www.redditstatic.com/icon.png/"

// Feed is a rendered-format-agnostic view of one listing.
type Feed struct {
	Subreddit    string // as requested, e.g. "selfhosted" or "linux+golang"; empty for user feeds
	Title        string
	Subtitle     string
	Icon         string
	SelfURL      string // this feed on our server
	AlternateURL string // the listing on Reddit
	LinkBase     string // e.g. "https://www.reddit.com"
	Updated      time.Time
	Items        []Item
}

// --- Atom -------------------------------------------------------------------

type atomFeed struct {
	XMLName  xml.Name      `xml:"feed"`
	Xmlns    string        `xml:"xmlns,attr"`
	XmlnsMed string        `xml:"xmlns:media,attr"`
	Category *atomCategory `xml:"category"`
	Updated  string        `xml:"updated"`
	Icon     string        `xml:"icon"`
	ID       string        `xml:"id"`
	Links    []atomLink    `xml:"link"`
	Subtitle string        `xml:"subtitle,omitempty"`
	Title    string        `xml:"title"`
	Entries  []atomEntry   `xml:"entry"`
}

type atomCategory struct {
	Term  string `xml:"term,attr"`
	Label string `xml:"label,attr"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr,omitempty"`
	Href string `xml:"href,attr"`
	Type string `xml:"type,attr,omitempty"`
}

type atomAuthor struct {
	Name string `xml:"name"`
	URI  string `xml:"uri,omitempty"`
}

type atomContent struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

type mediaThumbnail struct {
	URL string `xml:"url,attr"`
}

type atomEntry struct {
	Author    atomAuthor      `xml:"author"`
	Category  atomCategory    `xml:"category"`
	Content   atomContent     `xml:"content"`
	ID        string          `xml:"id"`
	Thumbnail *mediaThumbnail `xml:"media:thumbnail"`
	Link      atomLink        `xml:"link"`
	Updated   string          `xml:"updated"`
	Published string          `xml:"published"`
	Title     string          `xml:"title"`
}

// RenderAtom renders the feed as Atom 1.0, closely following Reddit's own
// (Atom) ".rss" output.
func RenderAtom(f *Feed) ([]byte, error) {
	out := atomFeed{
		Xmlns:    "http://www.w3.org/2005/Atom",
		XmlnsMed: "http://search.yahoo.com/mrss/",
		Updated:  f.Updated.Format(time.RFC3339),
		Icon:     f.Icon,
		ID:       f.SelfURL,
		Links: []atomLink{
			{Rel: "self", Href: f.SelfURL, Type: "application/atom+xml"},
			{Rel: "alternate", Href: f.AlternateURL, Type: "text/html"},
		},
		Subtitle: f.Subtitle,
		Title:    f.Title,
	}

	if f.Subreddit != "" {
		cat := subredditCategory(f.Subreddit)
		out.Category = &cat
	}

	for i := range f.Items {
		it := &f.Items[i]
		out.Entries = append(out.Entries, atomEntry{
			Author:    atomAuthor{Name: "/u/" + it.Author, URI: userURL(f.LinkBase, it.Author)},
			Category:  subredditCategory(it.Subreddit),
			Content:   atomContent{Type: "html", Body: itemContent(f.LinkBase, it)},
			ID:        it.Name,
			Thumbnail: thumbnail(it),
			Link:      atomLink{Href: f.LinkBase + it.Permalink},
			Updated:   it.Updated().Format(time.RFC3339),
			Published: it.Created().Format(time.RFC3339),
			Title:     itemTitle(it),
		})
	}

	return marshalXML(out)
}

// --- RSS 2.0 ----------------------------------------------------------------

type rssDoc struct {
	XMLName  xml.Name   `xml:"rss"`
	Version  string     `xml:"version,attr"`
	XmlnsAt  string     `xml:"xmlns:atom,attr"`
	XmlnsDC  string     `xml:"xmlns:dc,attr"`
	XmlnsMed string     `xml:"xmlns:media,attr"`
	Channel  rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	AtomLink      atomLink  `xml:"atom:link"`
	LastBuildDate string    `xml:"lastBuildDate"`
	Image         *rssImage `xml:"image"`
	Items         []rssItem `xml:"item"`
}

type rssImage struct {
	URL   string `xml:"url"`
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

type rssGUID struct {
	IsPermaLink bool   `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type rssItem struct {
	Title       string          `xml:"title"`
	Link        string          `xml:"link"`
	GUID        rssGUID         `xml:"guid"`
	PubDate     string          `xml:"pubDate"`
	Creator     string          `xml:"dc:creator"`
	Category    string          `xml:"category"`
	Comments    string          `xml:"comments"`
	Description string          `xml:"description"`
	Thumbnail   *mediaThumbnail `xml:"media:thumbnail"`
}

// RenderRSS renders the feed as RSS 2.0.
func RenderRSS(f *Feed) ([]byte, error) {
	description := f.Subtitle
	if description == "" {
		description = f.Title
	}

	out := rssDoc{
		Version:  "2.0",
		XmlnsAt:  "http://www.w3.org/2005/Atom",
		XmlnsDC:  "http://purl.org/dc/elements/1.1/",
		XmlnsMed: "http://search.yahoo.com/mrss/",
		Channel: rssChannel{
			Title:         f.Title,
			Link:          f.AlternateURL,
			Description:   description,
			AtomLink:      atomLink{Rel: "self", Href: f.SelfURL, Type: "application/rss+xml"},
			LastBuildDate: f.Updated.Format(time.RFC1123Z),
		},
	}
	if f.Icon != "" {
		out.Channel.Image = &rssImage{URL: f.Icon, Title: f.Title, Link: f.AlternateURL}
	}

	for i := range f.Items {
		it := &f.Items[i]
		permalink := f.LinkBase + it.Permalink
		out.Channel.Items = append(out.Channel.Items, rssItem{
			Title:       itemTitle(it),
			Link:        permalink,
			GUID:        rssGUID{Value: it.Name},
			PubDate:     it.Created().Format(time.RFC1123Z),
			Creator:     "/u/" + it.Author,
			Category:    "r/" + it.Subreddit,
			Comments:    permalink,
			Description: itemContent(f.LinkBase, it),
			Thumbnail:   thumbnail(it),
		})
	}

	return marshalXML(out)
}

// --- shared helpers ---------------------------------------------------------

func marshalXML(v any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func subredditCategory(sub string) atomCategory {
	return atomCategory{Term: sub, Label: "r/" + sub}
}

func userURL(linkBase, author string) string {
	if author == "" || author == "[deleted]" {
		return ""
	}
	return linkBase + "/user/" + author
}

func thumbnail(it *Item) *mediaThumbnail {
	// Reddit uses placeholders like "self", "default", "nsfw" and "spoiler".
	if strings.HasPrefix(it.Thumbnail, "http") {
		return &mediaThumbnail{URL: it.Thumbnail}
	}
	return nil
}

// itemTitle is the post's title, or "/u/x on <post title>" for a comment, as
// in Reddit's feeds.
func itemTitle(it *Item) string {
	if it.IsComment() {
		return "/u/" + it.Author + " on " + it.LinkTitle
	}
	return it.Title
}

// itemContent is the HTML body of an entry: the comment's text, or the post
// rendered by postContent.
func itemContent(linkBase string, it *Item) string {
	if it.IsComment() {
		return it.BodyHTML
	}
	return postContent(linkBase, it)
}

// postContent builds the HTML body of an entry the same way Reddit does: the
// self text (or a thumbnail for link posts), then "submitted by /u/x [link]
// [comments]".
func postContent(linkBase string, p *Item) string {
	esc := html.EscapeString
	permalink := linkBase + p.Permalink

	var author string
	if u := userURL(linkBase, p.Author); u != "" {
		author = fmt.Sprintf(`<a href="%s"> /u/%s </a>`, esc(u), esc(p.Author))
	} else {
		author = "/u/" + esc(p.Author)
	}
	footer := fmt.Sprintf(
		`&#32; submitted by &#32; %s <br/> <span><a href="%s">[link]</a></span> &#32; <span><a href="%s">[comments]</a></span>`,
		author, esc(p.URL), esc(permalink),
	)

	if thumb := thumbnail(p); thumb != nil && !p.IsSelf {
		return fmt.Sprintf(
			`<table> <tr><td> <a href="%s"> <img src="%s" alt="%s" title="%s" /> </a> </td><td> %s </td></tr></table>`,
			esc(permalink), esc(thumb.URL), esc(p.Title), esc(p.Title), footer,
		)
	}
	if p.SelftextHTML != "" {
		return p.SelftextHTML + " " + footer
	}
	return footer
}
