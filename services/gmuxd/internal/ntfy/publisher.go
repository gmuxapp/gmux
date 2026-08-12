// Package ntfy publishes privacy-safe, best-effort notifications to ntfy.
package ntfy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultConcurrency = 4
	maxResponseBytes   = 64 << 10
	busyLogInterval    = time.Minute
)

// Kind identifies the attention event without carrying session content.
type Kind string

const (
	KindFinished  Kind = "finished"
	KindUnread    Kind = "unread"
	KindCoalesced Kind = "coalesced"
)

// Message contains only the privacy-safe facts used by the ntfy formatter.
type Message struct {
	Kind      Kind
	SessionID string
	Adapter   string
	Count     int
}

// Config is the validated ntfy publisher configuration.
type Config struct {
	ServerURL string
	Topic     string
	Token     string
	Username  string
	Password  string
	Priority  int
	Tags      []string
	ClickURL  string
	Timeout   time.Duration
	Hostname  string
}

// Option is a test seam for Publisher construction.
type Option func(*options)

type options struct {
	transport   http.RoundTripper
	logf        func(string, ...any)
	concurrency int
}

// WithTransport replaces the HTTP transport. It is intended for tests.
func WithTransport(transport http.RoundTripper) Option {
	return func(opts *options) { opts.transport = transport }
}

// WithLogger replaces the package logger. It is intended for tests.
func WithLogger(logf func(string, ...any)) Option {
	return func(opts *options) { opts.logf = logf }
}

// WithConcurrency replaces the in-flight request limit. It is intended for tests.
func WithConcurrency(limit int) Option {
	return func(opts *options) { opts.concurrency = limit }
}

// Publisher makes at most one asynchronous HTTP attempt per accepted message.
// It has no waiting queue, retries, or persistent state.
type Publisher struct {
	endpoint *url.URL
	config   Config
	client   *http.Client
	logf     func(string, ...any)
	sem      chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup

	lastBusyLog atomic.Int64
}

// New constructs a publisher without making a network request.
func New(cfg Config, optionsList ...Option) (*Publisher, error) {
	server, err := url.Parse(cfg.ServerURL)
	if err != nil || server.Scheme == "" || server.Host == "" {
		return nil, errors.New("ntfy: invalid server URL")
	}
	endpoint := *server
	endpoint.Path = "/" + cfg.Topic
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""

	opts := options{logf: log.Printf, concurrency: defaultConcurrency}
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		opts.transport = transport.Clone()
	} else {
		opts.transport = http.DefaultTransport
	}
	for _, option := range optionsList {
		option(&opts)
	}
	if opts.concurrency < 1 {
		return nil, errors.New("ntfy: concurrency must be positive")
	}
	if opts.logf == nil {
		opts.logf = func(string, ...any) {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Publisher{
		endpoint: &endpoint,
		config:   cfg,
		client: &http.Client{
			Transport: opts.transport,
			Timeout:   cfg.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logf:   opts.logf,
		sem:    make(chan struct{}, opts.concurrency),
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// Notify starts one best-effort publish attempt without waiting for a slot or
// for HTTP. It returns false when the publisher is closed or all slots are busy.
func (p *Publisher) Notify(message Message) bool {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	select {
	case p.sem <- struct{}{}:
		p.wg.Add(1)
		p.mu.Unlock()
	case <-p.ctx.Done():
		p.mu.Unlock()
		return false
	default:
		p.mu.Unlock()
		p.logBusy()
		return false
	}

	go func() {
		defer func() {
			<-p.sem
			p.wg.Done()
		}()
		if class := p.publish(p.ctx, message); class != "" {
			p.logf("ntfy: publish failed class=%s", class)
		}
	}()
	return true
}

// Close cancels in-flight requests and waits for their goroutines. Future
// notifications are dropped.
func (p *Publisher) Close() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		p.cancel()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func (p *Publisher) publish(ctx context.Context, message Message) string {
	title, body := formatMessage(p.config.Hostname, message)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint.String(), bytes.NewBufferString(body))
	if err != nil {
		return "request"
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", title)
	req.Header.Set("Priority", strconv.Itoa(p.config.Priority))
	if len(p.config.Tags) > 0 {
		req.Header.Set("Tags", strings.Join(p.config.Tags, ","))
	}
	if p.config.ClickURL != "" {
		req.Header.Set("Click", p.config.ClickURL)
	}
	if p.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.config.Token)
	} else if p.config.Username != "" {
		req.SetBasicAuth(p.config.Username, p.config.Password)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "canceled"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "timeout"
		}
		return "network"
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ""
	}
	return "http_" + strconv.Itoa(resp.StatusCode)
}

func (p *Publisher) logBusy() {
	now := time.Now().UnixNano()
	last := p.lastBusyLog.Load()
	if last != 0 && time.Duration(now-last) < busyLogInterval {
		return
	}
	if p.lastBusyLog.CompareAndSwap(last, now) {
		p.logf("ntfy: publish dropped class=busy")
	}
}

func formatMessage(host string, message Message) (string, string) {
	host = cleanText(host)
	if host == "" {
		host = "this host"
	}
	if message.Kind == KindCoalesced {
		count := message.Count
		if count < 1 {
			count = 1
		}
		return "gmux", truncateUTF8(fmt.Sprintf("Host %s · %d sessions need attention.", host, count), 512)
	}
	adapter := cleanText(message.Adapter)
	if adapter == "" {
		adapter = "gmux"
	}
	sessionID := cleanText(message.SessionID)
	title := "gmux: new output"
	if message.Kind == KindFinished {
		title = "gmux: task finished"
	}
	body := fmt.Sprintf("Host %s · %s session %s needs attention.", host, adapter, sessionID)
	return truncateUTF8(title, 128), truncateUTF8(body, 512)
}

func cleanText(value string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value))
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
