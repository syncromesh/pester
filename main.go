package pester

// pester provides additional resiliency over the standard http client methods by
// allowing you to control concurrency, retries, and a backoff strategy.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	// "github.com/sbowman/glog"
)

// SendErrorCode is the code for logging send errors via the LogWriter
const SendErrorCode = 602

// RFC3339Ms is the time format with milliseconds.
const RFC3339Ms = "2006-01-02T15:04:05.000Z07:00"

// LogStringTimeFormat produces timestamp strings like hh:mm:ss.uuuuuu
const LogStringTimeFormat = `15:04:05.999999`

// ExternalLogger is a function that is called when LogRetries is true.
// Same signature as glog.Errorf(format string, args ...interface{}))
type ExternalLogger func(format string, args ...interface{})

// Client wraps the http client and exposes all the functionality of the http.Client.
// Additionally, Client provides pester specific values for handling resiliency.
type Client struct {
	// wrap it to provide access to http built ins
	hc http.Client

	Transport     http.RoundTripper
	CheckRedirect func(req *http.Request, via []*http.Request) error
	Jar           http.CookieJar
	Timeout       time.Duration

	// pester specific
	Concurrency int
	MaxRetries  int
	Backoff     BackoffStrategy
	KeepLog     bool

	// A logger provided externally - timestamp may not be what is expected
	Logger func(string)

	// LogWriter is used to send a pre-formatted log message to (e.g. STDOUT/ERR)
	LogWriter io.Writer

	// LogRetries enables logging of retries when they happen
	LogRetries bool

	// Verbosity of debug messages; 0 is no debug (info only), 3 is most verbose.
	Verbosity int
	// Threshhold - Minimum log level to output to console (info, warn, error, or fatal)
	Threshold string

	// LogTimeFormat is used to format the time when LogRetries is true
	LogTimeFormat string

	SuccessReqNum   int
	SuccessRetryNum int

	sync.Mutex
	ErrLog []ErrEntry
}

// ErrEntry is used to provide the LogString() data and is populated
// each time an error happens if KeepLog is set
type ErrEntry struct {
	Time    time.Time
	Method  string
	URL     string
	Verb    string
	Request int
	Retry   int
	Err     error
}

// result simplifies the channel communication for concurrent request handling
type result struct {
	resp  *http.Response
	err   error
	req   int
	retry int
}

// params represents all the params needed to run http client calls and pester errors
type params struct {
	method   string
	verb     string
	req      *http.Request
	url      string
	bodyType string
	body     io.Reader
	data     url.Values
}

// New constructs a new DefaultClient with sensible default values
func New() *Client {
	return &Client{
		Concurrency:   DefaultClient.Concurrency,
		MaxRetries:    DefaultClient.MaxRetries,
		Backoff:       DefaultClient.Backoff,
		ErrLog:        DefaultClient.ErrLog,
		LogRetries:    true,
		Verbosity:     2,
		Threshold:     "info",
		LogTimeFormat: LogStringTimeFormat,
	}
}

// BackoffStrategy is used to determine how long a retry request should wait until attempted
type BackoffStrategy func(retry int) time.Duration

// DefaultClient provides sensible defaults
var DefaultClient = &Client{Concurrency: 1, MaxRetries: 3, Backoff: DefaultBackoff, ErrLog: []ErrEntry{}}

// DefaultBackoff always returns 1 second
func DefaultBackoff(_ int) time.Duration {
	return 1 * time.Second
}

// ExponentialBackoff returns ever increasing backoffs by a power of 2
func ExponentialBackoff(i int) time.Duration {
	return time.Duration(math.Pow(2, float64(i))) * time.Second
}

// ExponentialJitterBackoff returns ever increasing backoffs by a power of 2
// with +/- 0-33% to prevent sychronized reuqests.
func ExponentialJitterBackoff(i int) time.Duration {
	return jitter(int(math.Pow(2, float64(i))))
}

// LinearBackoff returns increasing durations, each a second longer than the last
func LinearBackoff(i int) time.Duration {
	return time.Duration(i) * time.Second
}

// LinearJitterBackoff returns increasing durations, each a second longer than the last
// with +/- 0-33% to prevent sychronized reuqests.
func LinearJitterBackoff(i int) time.Duration {
	return jitter(i)
}

// jitter keeps the +/- 0-33% logic in one place
func jitter(i int) time.Duration {
	ms := i * 1000

	maxJitter := ms / 3

	rand.Seed(time.Now().Unix())
	jitter := rand.Intn(maxJitter + 1)

	if rand.Intn(2) == 1 {
		ms = ms + jitter
	} else {
		ms = ms - jitter
	}

	// a jitter of 0 messes up the time.Tick chan
	if ms <= 0 {
		ms = 1
	}

	return time.Duration(ms) * time.Millisecond
}

// pester provides all the logic of retries, concurrency, backoff, and logging
func (c *Client) pester(p params) (*http.Response, error) {
	resultCh := make(chan result)
	finishCh := make(chan struct{})

	// GET calls should be idempotent and can make use
	// of concurrency. Other verbs can mutate and should not
	// make use of the concurrency feature
	concurrency := c.Concurrency
	if p.verb != "GET" {
		concurrency = 1
	}

	// re-create the http client so we can leverage the std lib
	httpClient := http.Client{
		Transport:     c.hc.Transport,
		CheckRedirect: c.hc.CheckRedirect,
		Jar:           c.hc.Jar,
		Timeout:       c.hc.Timeout,
	}

	// if we have a request body, we need to save it for later
	var originalRequestBody []byte
	var originalBody []byte
	var err error
	if p.req != nil && p.req.Body != nil {
		originalRequestBody, err = ioutil.ReadAll(p.req.Body)
		if err != nil {
			return &http.Response{}, errors.New("error reading request body")
		}
		p.req.Body.Close()
	}
	if p.body != nil {
		originalBody, err = ioutil.ReadAll(p.body)
		if err != nil {
			return &http.Response{}, errors.New("error reading body")
		}
	}

	for req := 0; req < concurrency; req++ {
		go func(n int, p params) {
			resp := &http.Response{}
			var err error

			for i := 0; i < c.MaxRetries; i++ {
				select {
				case <-finishCh:
					return
				default:
				}
				// rehydrate the body (it is drained each read)
				if len(originalRequestBody) > 0 {
					p.req.Body = ioutil.NopCloser(bytes.NewBuffer(originalRequestBody))
				}
				if len(originalBody) > 0 {
					p.body = bytes.NewBuffer(originalBody)
				}
				// route the calls
				switch p.method {
				case "Do":
					resp, err = httpClient.Do(p.req)
				case "Get":
					resp, err = httpClient.Get(p.url)
				case "Head":
					resp, err = httpClient.Head(p.url)
				case "Post":
					resp, err = httpClient.Post(p.url, p.bodyType, p.body)
				case "PostForm":
					resp, err = httpClient.PostForm(p.url, p.data)
				}

				// 200 and 300 level errors are considered success and we are done
				if err == nil && resp.StatusCode < 400 {
					resultCh <- result{resp: resp, err: err, req: n, retry: i}
					return
				}

				if err == nil {
					err = fmt.Errorf("%v", resp.Status)
				}

				c.log(ErrEntry{
					Time:    time.Now(),
					Method:  p.method,
					Verb:    p.verb,
					URL:     p.url,
					Request: n,
					Retry:   i,
					Err:     err,
				})

				// prevent a 0 from causing the tick to block, pass additional microsecond
				<-time.Tick(c.Backoff(i) + 1*time.Microsecond)
			}
			resultCh <- result{resp: resp, err: err}
		}(req, p)
	}

	for {
		select {
		case res := <-resultCh:
			close(finishCh)
			c.SuccessReqNum = res.req
			c.SuccessRetryNum = res.retry
			return res.resp, res.err
		}
	}
}

// LogString provides a string representation of the errors the client has seen
func (c *Client) LogString() string {
	var res string
	for _, e := range c.ErrLog {
		res += fmt.Sprintf("%d %s [%s] to %s request-%d retry-%d error: %s\n",
			e.Time.Unix(), e.Method, e.Verb, e.URL, e.Request, e.Retry, e.Err)
	}
	return res
}

// log has been hacked
func (c *Client) log(e ErrEntry) {
	if c.LogRetries { //&& c.Logger != nil {

		if c.Logger != nil {
			// Calling the external logger works except the timestamp is "zero" time
			msg := fmt.Sprintf("%s %s [%s] to %s request-%d retry-%d error: %v",
				e.Time.UTC().Format(c.LogTimeFormat), e.Method, e.Verb, e.URL, e.Request, e.Retry, e.Err)
			c.Logger(msg)
		}

		if c.LogWriter != nil {
			// Error code, time, file:line, message
			format := "I %04d %s [%s:%d] %s\n" // Infof format

			// Time, file:line, message
			// format := "D      %s [%s:%d] %s\n" // Debugf format

			_, file, line, ok := runtime.Caller(1)

			t := e.Time.UTC().Format(RFC3339Ms)

			lwmsg := fmt.Sprintf("%s %s [%s] to %s request-%d retry-%d error: %v",
				e.Time.UTC().Format(c.LogTimeFormat), e.Method, e.Verb, e.URL, e.Request, e.Retry, e.Err)

			var logmsg string
			if ok {
				dir := filepath.Dir(file)
				dir = filepath.Base(dir)
				file = dir + string(os.PathSeparator) + filepath.Base(file)
				// 602 is a SendError
				logmsg = fmt.Sprintf(format, SendErrorCode, t, file, line, lwmsg)
			} else {
				logmsg = fmt.Sprintf(format, SendErrorCode, t, "n/a", 0, lwmsg)
			}

			c.LogWriter.Write([]byte(logmsg))
		}
	}
	if c.KeepLog {
		c.Lock()
		c.ErrLog = append(c.ErrLog, e)
		c.Unlock()
	}
}

// Do provides the same functionality as http.Client.Do
func (c *Client) Do(req *http.Request) (resp *http.Response, err error) {
	return c.pester(params{method: "Do", req: req, verb: req.Method, url: req.URL.String()})
}

// Get provides the same functionality as http.Client.Get
func (c *Client) Get(url string) (resp *http.Response, err error) {
	return c.pester(params{method: "Get", url: url, verb: "GET"})
}

// Head provides the same functionality as http.Client.Head
func (c *Client) Head(url string) (resp *http.Response, err error) {
	return c.pester(params{method: "Head", url: url, verb: "HEAD"})
}

// Post provides the same functionality as http.Client.Post
func (c *Client) Post(url string, bodyType string, body io.Reader) (resp *http.Response, err error) {
	return c.pester(params{method: "Post", url: url, bodyType: bodyType, body: body, verb: "POST"})
}

// PostForm provides the same functionality as http.Client.PostForm
func (c *Client) PostForm(url string, data url.Values) (resp *http.Response, err error) {
	return c.pester(params{method: "PostForm", url: url, data: data, verb: "POST"})
}

////////////////////////////////////////
// Provide self-constructing variants //
//                                    //
// NOTE: These will NOT  log retries  //
////////////////////////////////////////

// Do provides the same functionality as http.Client.Do and creates its own constructor
func Do(req *http.Request) (resp *http.Response, err error) {
	c := New()
	return c.Do(req)
}

// Get provides the same functionality as http.Client.Get and creates its own constructor
func Get(url string) (resp *http.Response, err error) {
	c := New()
	return c.Get(url)
}

// Head provides the same functionality as http.Client.Head and creates its own constructor
func Head(url string) (resp *http.Response, err error) {
	c := New()
	return c.Head(url)
}

// Post provides the same functionality as http.Client.Post and creates its own constructor
func Post(url string, bodyType string, body io.Reader) (resp *http.Response, err error) {
	c := New()
	return c.Post(url, bodyType, body)
}

// PostForm provides the same functionality as http.Client.PostForm and creates its own constructor
func PostForm(url string, data url.Values) (resp *http.Response, err error) {
	c := New()
	return c.PostForm(url, data)
}
