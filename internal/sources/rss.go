package sources

import (
	"context"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

// RSS parses RSS 2.0 and Atom documents into candidate deals.
type RSS struct{ base }

type rssDocument struct {
	XMLName xml.Name
	Title   string `xml:"title"`
	Channel struct {
		Title string    `xml:"title"`
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
	Items   []rssItem   `xml:"item"`
	Entries []atomEntry `xml:"entry"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	// Namespaced variants: Discourse/RSS emit content:encoded, some Chinese
	// feeds emit dc:date. A plain tag would not match them.
	Encoded      string   `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	EncodedLoose string   `xml:"encoded"`
	Date         string   `xml:"http://purl.org/dc/elements/1.1/ date"`
	DateLoose    string   `xml:"date"`
	Content      string   `xml:"content"`
	Guid         string   `xml:"guid"`
	PubDate      string   `xml:"pubDate"`
	Updated      string   `xml:"updated"`
	Categories   []string `xml:"category"`
	Subject      []string `xml:"subject"`
}

type atomEntry struct {
	Title      string         `xml:"title"`
	Links      []atomLink     `xml:"link"`
	Summary    string         `xml:"summary"`
	Content    string         `xml:"content"`
	Updated    string         `xml:"updated"`
	Published  string         `xml:"published"`
	ID         string         `xml:"id"`
	Categories []atomCategory `xml:"category"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

type atomCategory struct {
	Term string `xml:"term,attr"`
}

// parsedItem is the collector-neutral shape both feed dialects collapse to.
type parsedItem struct {
	Title      string
	Link       string
	Summary    string
	Published  time.Time
	Categories []string
	ID         string
}

// Fetch implements Source.
func (r *RSS) Fetch(ctx context.Context) ([]*model.Deal, error) {
	resp, err := r.fetch(ctx)
	if err != nil {
		return nil, err
	}
	items, err := r.items(decodeXMLUnsafeEntities(string(resp.Body)))
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", r.Cfg.Name, err)
	}
	var out []*model.Deal
	for _, it := range items {
		title := it.Title
		summary := truncate(it.Summary, 500)
		if title == "" {
			title = truncate(summary, 120)
		}
		if title == "" {
			continue
		}
		d := r.deal(title, it.Link, summary, it.Published)
		if d == nil {
			continue
		}
		for _, c := range it.Categories {
			if c = strings.TrimSpace(c); c != "" {
				d.Tags = append(d.Tags, c)
			}
		}
		if it.ID != "" {
			d.Meta["guid"] = truncate(it.ID, 200)
		}
		out = append(out, d)
	}
	return limit(out, r.Cfg.Limit), nil
}

// items decodes the feed, falling back to a tolerant block scanner so one
// malformed entity cannot cost us the whole feed.
func (r *RSS) items(body string) ([]parsedItem, error) {
	var doc rssDocument
	decodeErr := xml.Unmarshal([]byte(body), &doc)
	if decodeErr == nil {
		items := append([]rssItem{}, doc.Channel.Items...)
		items = append(items, doc.Items...)
		if len(items) == 0 && len(doc.Entries) == 0 {
			return nil, fmt.Errorf("feed contains no items")
		}
		out := make([]parsedItem, 0, len(items)+len(doc.Entries))
		for _, it := range items {
			out = append(out, parsedItem{
				Title:      cleanText(it.Title),
				Link:       strings.TrimSpace(stripCdata(it.Link)),
				Summary:    cleanText(firstNonEmpty(it.Description, it.Encoded, it.EncodedLoose, it.Content)),
				Published:  parseAnyTime(firstNonEmpty(it.PubDate, it.Updated, it.Date, it.DateLoose)),
				Categories: append(append([]string{}, it.Categories...), it.Subject...),
				ID:         strings.TrimSpace(stripCdata(it.Guid)),
			})
		}
		for _, e := range doc.Entries {
			var terms []string
			for _, c := range e.Categories {
				terms = append(terms, c.Term)
			}
			out = append(out, parsedItem{
				Title:      cleanText(e.Title),
				Link:       pickAtomLink(e.Links),
				Summary:    cleanText(firstNonEmpty(e.Summary, e.Content)),
				Published:  parseAnyTime(firstNonEmpty(e.Published, e.Updated)),
				Categories: terms,
				ID:         e.ID,
			})
		}
		return out, nil
	}
	if tolerant := tolerantRSS(body); len(tolerant) > 0 {
		return tolerant, nil
	}
	return nil, fmt.Errorf("malformed feed: %w", decodeErr)
}

var (
	itemBlockRe  = regexp.MustCompile(`(?is)<item[^>]*>(.*?)</item>`)
	entryBlockRe = regexp.MustCompile(`(?is)<entry[^>]*>(.*?)</entry>`)
)

func tolerantRSS(body string) []parsedItem {
	blocks := itemBlockRe.FindAllStringSubmatch(body, -1)
	if len(blocks) == 0 {
		blocks = entryBlockRe.FindAllStringSubmatch(body, -1)
	}
	var out []parsedItem
	for _, b := range blocks {
		blk := b[1]
		out = append(out, parsedItem{
			Title:     cleanText(subtag(blk, "title")),
			Link:      strings.TrimSpace(stripCdata(subtag(blk, "link"))),
			Summary:   cleanText(firstNonEmpty(subtag(blk, "description"), subtag(blk, "summary"), subtag(blk, "encoded"), subtag(blk, "content"))),
			Published: parseAnyTime(firstNonEmpty(subtag(blk, "pubDate"), subtag(blk, "updated"))),
		})
	}
	return out
}

var (
	subtagMu  sync.Mutex
	subtagREs = map[string]*regexp.Regexp{}
)

func subtagRE(name string) *regexp.Regexp {
	subtagMu.Lock()
	defer subtagMu.Unlock()
	if re, ok := subtagREs[name]; ok {
		return re
	}
	re := regexp.MustCompile(`(?is)<` + name + `(?:\s[^>]*)?>(.*?)</` + name + `>`)
	subtagREs[name] = re
	return re
}

func subtag(block, name string) string {
	m := subtagRE(name).FindStringSubmatch(block)
	if m == nil {
		return ""
	}
	return stripCdata(m[1])
}

func stripCdata(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<![CDATA[") {
		s = strings.TrimSuffix(strings.TrimPrefix(s, "<![CDATA["), "]]>")
	}
	return strings.TrimSpace(s)
}

func pickAtomLink(links []atomLink) string {
	if len(links) == 0 {
		return ""
	}
	for _, l := range links {
		if l.Rel == "" || l.Rel == "alternate" {
			return l.Href
		}
	}
	return links[0].Href
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
