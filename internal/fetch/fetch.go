// Package fetch is a polite HTTP client: per-host rate limit, robots.txt (RFC 9309) with
// retries, retries with backoff on transient statuses, bot-wall detection and HEAD checks.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/robots"
)

var retryStatuses = map[int]bool{408: true, 425: true, 429: true, 500: true, 502: true, 503: true, 504: true}

// RobotsDisallowed is returned when robots.txt forbids a URL. Unreachable is true when
// robots.txt could not be read (5xx, 429, network) rather than a real Disallow.
type RobotsDisallowed struct {
	URL, Reason string
	Unreachable bool
}

func (e *RobotsDisallowed) Error() string {
	return fmt.Sprintf("robots.txt disallows %s [%s]", e.URL, e.Reason)
}

// FetchError is an HTTP or network failure. Status is 0 for network errors.
type FetchError struct {
	URL     string
	Status  int
	Message string
	Wall    string
}

func (e *FetchError) Error() string {
	s := "error"
	if e.Status != 0 {
		s = strconv.Itoa(e.Status)
	}
	msg := s + " " + e.URL
	if e.Message != "" {
		msg += " (" + e.Message + ")"
	}
	return msg
}

var wallMarkers = []struct {
	name string
	rx   *regexp.Regexp
}{
	{"cloudflare_challenge", regexp.MustCompile(`(?i)attention required!? \| cloudflare|just a moment\.\.\.|cf-chl`)},
	{"akamai_block", regexp.MustCompile(`(?is)access denied.*reference #|errors\.edgesuite\.net`)},
	{"captcha", regexp.MustCompile(`(?i)captcha|are you a robot|verify you are human`)},
	{"incapsula", regexp.MustCompile(`(?i)incapsula|_incap_`)},
}

// DetectWall names the bot wall (WAF challenge or block page) a response shows, or "".
func DetectWall(resp *http.Response, body []byte) string {
	if strings.EqualFold(resp.Header.Get("cf-mitigated"), "challenge") {
		return "cloudflare_challenge"
	}
	if len(body) > 4096 {
		body = body[:4096]
	}
	for _, m := range wallMarkers {
		if m.rx.Match(body) {
			return m.name
		}
	}
	if (resp.StatusCode == 403 || resp.StatusCode == 429) && strings.Contains(strings.ToLower(resp.Header.Get("server")), "cloudflare") {
		return "cloudflare_block"
	}
	return ""
}

// HostLimiter spaces requests to the same host by at least 1/rate seconds.
type HostLimiter struct {
	interval time.Duration
	mu       sync.Mutex
	next     map[string]time.Time
	now      func() time.Time
	sleep    func(time.Duration)
}

func NewHostLimiter(rate float64) *HostLimiter {
	var iv time.Duration
	if rate > 0 {
		iv = time.Duration(float64(time.Second) / rate)
	}
	return &HostLimiter{interval: iv, next: map[string]time.Time{}, now: time.Now, sleep: time.Sleep}
}

func (l *HostLimiter) Wait(host string) {
	l.mu.Lock()
	now := l.now()
	due, ok := l.next[host]
	if !ok || due.Before(now) {
		due = now
	}
	l.next[host] = due.Add(l.interval)
	l.mu.Unlock()
	if d := due.Sub(now); d > 0 {
		l.sleep(d)
	}
}

// UrlCheck is the result of a HEAD (or one-byte range GET) check.
type UrlCheck struct {
	Status        int    `json:"status,omitempty"`
	Method        string `json:"method,omitempty"`
	ContentLength int64  `json:"content_length,omitempty"`
	HasLength     bool   `json:"-"` // false when the server sent no length
	LastModified  string `json:"last_modified,omitempty"`
	ETag          string `json:"etag,omitempty"`
	Error         string `json:"error,omitempty"`
}

type robotsEntry struct {
	once   sync.Once
	rules  *robots.Rules // nil when unreachable
	status string
}

type Client struct {
	S       config.Settings
	HTTP    *http.Client
	Limiter *HostLimiter
	Sleep   func(time.Duration)
	sem     chan struct{}
	mu      sync.Mutex
	robots  map[string]*robotsEntry
}

func New(s config.Settings) *Client {
	n := s.Concurrency
	if n < 1 {
		n = 1
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Bodies can stream for hours, so only the wait for response headers is bounded.
	tr.ResponseHeaderTimeout = time.Duration(s.TimeoutSeconds * float64(time.Second))
	return &Client{
		S:       s,
		HTTP:    &http.Client{Transport: tr},
		Limiter: NewHostLimiter(s.RequestsPerSecondHost),
		Sleep:   time.Sleep,
		sem:     make(chan struct{}, n),
		robots:  map[string]*robotsEntry{},
	}
}

func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

func (c *Client) backoff(attempt int) time.Duration {
	base := c.S.BackoffBaseSeconds * float64(int(1)<<attempt)
	d := base + rand.Float64()*base/4
	if d > c.S.BackoffMaxSeconds {
		d = c.S.BackoffMaxSeconds
	}
	return time.Duration(d * float64(time.Second))
}

func (c *Client) robotsFor(u *url.URL) *robotsEntry {
	o := origin(u)
	c.mu.Lock()
	e, ok := c.robots[o]
	if !ok {
		e = &robotsEntry{}
		c.robots[o] = e
	}
	c.mu.Unlock()
	e.once.Do(func() { e.rules, e.status = c.loadRobots(u) })
	return e
}

// RobotsStatus returns the robots.txt outcome recorded for the URL's origin.
func (c *Client) RobotsStatus(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.robots[origin(u)]; ok {
		return e.status
	}
	return ""
}

// loadRobots fetches robots.txt with retries. Only a persistent 5xx/429/network failure disallows.
func (c *Client) loadRobots(u *url.URL) (*robots.Rules, string) {
	o := origin(u)
	problem := ""
	for attempt := 0; attempt <= c.S.MaxRetries; attempt++ {
		c.Limiter.Wait(u.Host)
		resp, err := c.do(context.Background(), "GET", o+"/robots.txt", nil)
		if err != nil {
			problem = "robots.txt unreachable (" + errName(err) + ")"
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode < 500 && resp.StatusCode != 429 {
				status := fmt.Sprintf("robots.txt %d", resp.StatusCode)
				if resp.StatusCode >= 400 {
					return &robots.Rules{AllowAll: true}, status
				}
				return robots.Parse(string(body)), status
			}
			problem = fmt.Sprintf("robots.txt unavailable (HTTP %d)", resp.StatusCode)
		}
		if attempt < c.S.MaxRetries {
			c.Sleep(c.backoff(attempt))
		}
	}
	return nil, problem
}

func errName(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "timeout"
		}
		return fmt.Sprintf("%T", ue.Err)
	}
	return fmt.Sprintf("%T", err)
}

func (c *Client) do(ctx context.Context, method, rawURL string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.S.UserAgent())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if c.S.TimeoutSeconds > 0 && method != "GET" {
		ctx2, cancel := context.WithTimeout(ctx, time.Duration(c.S.TimeoutSeconds*float64(time.Second)))
		req = req.WithContext(ctx2)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			cancel()
			return nil, err
		}
		resp.Body = cancelOnClose{resp.Body, cancel}
		return resp, nil
	}
	return c.HTTP.Do(req)
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelOnClose) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

// Allowed reports whether robots.txt lets us fetch rawURL.
func (c *Client) Allowed(rawURL string) (bool, *robotsEntry, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false, nil, err
	}
	e := c.robotsFor(u)
	if e.rules == nil {
		return false, e, nil
	}
	return e.rules.CanFetch(c.S.UserAgent(), rawURL), e, nil
}

func retryAfter(resp *http.Response) (time.Duration, bool) {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// Send runs a request with the robots check, the rate limit and retries. The caller closes the body.
func (c *Client) Send(ctx context.Context, method, rawURL string, headers map[string]string) (*http.Response, error) {
	ok, e, err := c.Allowed(rawURL)
	if err != nil {
		return nil, &FetchError{URL: rawURL, Message: err.Error()}
	}
	if !ok {
		return nil, &RobotsDisallowed{URL: rawURL, Reason: e.status, Unreachable: e.rules == nil}
	}
	u, _ := url.Parse(rawURL)
	var lastErr error
	for attempt := 0; attempt <= c.S.MaxRetries; attempt++ {
		c.Limiter.Wait(u.Host)
		resp, err := c.do(ctx, method, rawURL, headers)
		if err != nil {
			lastErr = err
			if attempt == c.S.MaxRetries || ctx.Err() != nil {
				break
			}
			c.Sleep(c.backoff(attempt))
			continue
		}
		if retryStatuses[resp.StatusCode] && attempt < c.S.MaxRetries {
			wait, ok := retryAfter(resp)
			resp.Body.Close()
			if !ok {
				wait = c.backoff(attempt)
			}
			c.Sleep(wait)
			continue
		}
		return resp, nil
	}
	return nil, &FetchError{URL: rawURL, Message: fmt.Sprintf("%s: %v", errName(lastErr), lastErr)}
}

// Get returns a streaming response for a 2xx/3xx GET. Errors carry the detected bot wall.
func (c *Client) Get(ctx context.Context, rawURL string) (*http.Response, error) {
	c.sem <- struct{}{}
	resp, err := c.Send(ctx, "GET", rawURL, nil)
	if err != nil {
		<-c.sem
		return nil, err
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		<-c.sem
		return nil, &FetchError{URL: rawURL, Status: resp.StatusCode, Wall: DetectWall(resp, body)}
	}
	resp.Body = releaseOnClose{resp.Body, c.sem, &sync.Once{}}
	return resp, nil
}

type releaseOnClose struct {
	io.ReadCloser
	sem  chan struct{}
	once *sync.Once
}

func (b releaseOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { <-b.sem })
	return err
}

// GetText fetches a small text resource.
func (c *Client) GetText(ctx context.Context, rawURL string, limit int64) (string, error) {
	resp, err := c.Get(ctx, rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return string(b), err
}

// HeadOrRange records size and freshness without downloading the body. Pre-signed links are
// often signed for GET only, so 403/405/501 on HEAD falls back to one GET for the first byte.
func (c *Client) HeadOrRange(ctx context.Context, rawURL string) UrlCheck {
	c.sem <- struct{}{}
	defer func() { <-c.sem }()
	resp, err := c.Send(ctx, "HEAD", rawURL, nil)
	if err != nil {
		return checkFromErr(err)
	}
	resp.Body.Close()
	method := "HEAD"
	if resp.StatusCode == 403 || resp.StatusCode == 405 || resp.StatusCode == 501 {
		rng, err := c.Send(ctx, "GET", rawURL, map[string]string{"Range": "bytes=0-0"})
		if err != nil {
			return checkFromErr(err)
		}
		rng.Body.Close()
		resp, method = rng, "RANGE_GET"
	}
	return checkFrom(resp, method)
}

func checkFromErr(err error) UrlCheck {
	var rd *RobotsDisallowed
	if errors.As(err, &rd) {
		return UrlCheck{Error: "robots_disallowed"}
	}
	var fe *FetchError
	if errors.As(err, &fe) {
		return UrlCheck{Status: fe.Status, Error: fe.Error()}
	}
	return UrlCheck{Error: err.Error()}
}

func checkFrom(resp *http.Response, method string) UrlCheck {
	var length int64 = -1
	cr := resp.Header.Get("Content-Range")
	if resp.StatusCode == 206 && strings.Contains(cr, "/") {
		if n, err := strconv.ParseInt(cr[strings.LastIndex(cr, "/")+1:], 10, 64); err == nil {
			length = n
		}
	} else if n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil {
		length = n
	}
	uc := UrlCheck{Status: resp.StatusCode, Method: method, LastModified: resp.Header.Get("Last-Modified"), ETag: resp.Header.Get("ETag")}
	if length >= 0 {
		uc.ContentLength, uc.HasLength = length, true
	}
	if resp.StatusCode >= 400 {
		uc.Error = fmt.Sprintf("http_%d", resp.StatusCode)
	}
	return uc
}
