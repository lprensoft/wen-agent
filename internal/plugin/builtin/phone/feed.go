package phone

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// 订阅源只认 RSS 2.0、RSS 1.0（RDF）与 Atom，都用标准库的 encoding/xml 解析，不加依赖。

const (
	fetchTimeout  = 20 * time.Second
	maxFeedBytes  = 4 << 20 // 一份订阅源的大小上限
	maxPerFeed    = 20      // 每个源只看最前面这么多条：手机上往下划也就划这么远
	maxTitleRunes = 80
	maxSumRunes   = 400
)

// entry 是从订阅源里读出来的一条。
type entry struct {
	ID        string
	Title     string
	Link      string
	Summary   string
	Published time.Time
}

// feedDoc 同时容纳三种格式：RSS 2.0 的条目在 channel 下，RSS 1.0 的条目与 channel
// 平级，Atom 叫 entry。不先嗅探格式，一次解码把认得的字段都接住。
type feedDoc struct {
	XMLName  xml.Name
	Title    string     `xml:"title"` // Atom
	Channel  rssChannel `xml:"channel"`
	RDFItems []rssItem  `xml:"item"` // RSS 1.0
	Entries  []atomItem `xml:"entry"`
}

type rssChannel struct {
	Title string    `xml:"title"`
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	DCDate      string `xml:"http://purl.org/dc/elements/1.1/ date"`
}

type atomItem struct {
	Title     string     `xml:"title"`
	ID        string     `xml:"id"`
	Links     []atomLink `xml:"link"`
	Summary   string     `xml:"summary"`
	Content   string     `xml:"content"`
	Published string     `xml:"published"`
	Updated   string     `xml:"updated"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

var errNotFeed = errors.New("not an RSS or Atom feed")

// fetchFeed 取一个订阅源，返回源的名字与最前面的若干条。
func fetchFeed(ctx context.Context, client *http.Client, url string) (string, []entry, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("User-Agent", "wen-agent/1.0")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml;q=0.9, */*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return "", nil, err
	}
	if len(raw) > maxFeedBytes {
		return "", nil, fmt.Errorf("feed larger than %d MB", maxFeedBytes>>20)
	}
	return parseFeed(raw)
}

// parseFeed 解析一份订阅源。编码按 XML 声明里的 encoding 转（国内不少源是 GBK）。
func parseFeed(raw []byte) (string, []entry, error) {
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	// 不严格、认 HTML 实体：不少源里混着 &nbsp; 之类。不开 AutoClose——HTML 里 <link> 是
	// 自闭合的，开了它 RSS 的 <link>地址</link> 会被拆坏
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	dec.CharsetReader = func(label string, in io.Reader) (io.Reader, error) {
		enc, err := htmlindex.Get(label)
		if err != nil {
			return nil, err
		}
		return enc.NewDecoder().Reader(in), nil
	}
	var doc feedDoc
	if err := dec.Decode(&doc); err != nil {
		return "", nil, err
	}

	var name string
	var out []entry
	switch strings.ToLower(doc.XMLName.Local) {
	case "rss", "rdf":
		name = doc.Channel.Title
		items := doc.Channel.Items
		if len(items) == 0 {
			items = doc.RDFItems
		}
		for _, it := range items {
			e := entry{
				Title:     it.Title,
				Link:      strings.TrimSpace(it.Link),
				Summary:   it.Description,
				Published: parseTime(firstNonEmpty(it.PubDate, it.DCDate)),
			}
			e.ID = firstNonEmpty(strings.TrimSpace(it.GUID), e.Link)
			out = append(out, e)
		}
	case "feed":
		name = doc.Title
		for _, it := range doc.Entries {
			e := entry{
				Title:     it.Title,
				Link:      atomHref(it.Links),
				Summary:   firstNonEmpty(it.Summary, it.Content),
				Published: parseTime(firstNonEmpty(it.Published, it.Updated)),
			}
			e.ID = firstNonEmpty(strings.TrimSpace(it.ID), e.Link)
			out = append(out, e)
		}
	default:
		return "", nil, errNotFeed
	}

	name = cleanLine(name, maxTitleRunes)
	var kept []entry
	for _, e := range out {
		e.Title = cleanLine(e.Title, maxTitleRunes)
		if e.Title == "" {
			continue
		}
		e.Summary = cleanText(e.Summary, maxSumRunes)
		if e.ID == "" {
			e.ID = e.Title
		}
		e.ID = hashID(e.ID)
		kept = append(kept, e)
		if len(kept) == maxPerFeed {
			break
		}
	}
	return name, kept, nil
}

func atomHref(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "" || l.Rel == "alternate" {
			return strings.TrimSpace(l.Href)
		}
	}
	if len(links) > 0 {
		return strings.TrimSpace(links[0].Href)
	}
	return ""
}

// hashID 把条目标识压成定长：有的源拿整段长链接当 guid，原样存进去重表会无谓地胀大。
func hashID(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:10])
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

var timeLayouts = []string{
	time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.RFC3339, time.RFC3339Nano,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", "2 Jan 2006 15:04:05 -0700",
	"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02",
}

// parseTime 认常见的几种时间写法，认不出返回零值（由刷到的时刻代替）。
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

var tagRE = regexp.MustCompile(`(?s)<[^>]*>`)

// cleanText 把摘要里的 HTML 去掉、实体还原、空白收拢，按字数截断。
func cleanText(s string, max int) string {
	s = tagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return cleanLine(s, max)
}

// cleanLine 把外网来的一段文字压成一行：控制字符与换行换成空格，方括号换成圆括号——
// 注入里的块名用的正是方括号，一条标题写成「[系统] ……」就能冒充一个块。
func cleanLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '[':
			return '('
		case r == ']':
			return ')'
		case unicode.IsControl(r), r == utf8.RuneError:
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
