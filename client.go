// The retryablehttp package provides a familiar HTTP client interface with
// automatic retries and exponential backoff. It is a thin wrapper over the
// standard net/http client library and exposes nearly the same public API.
// This makes retryablehttp very easy to drop into existing programs.
//
// retryablehttp performs automatic retries under certain conditions. Mainly, if
// an error is returned by the client (connection errors etc), or if a 500-range
// response is received, then a retry is invoked. Otherwise, the response is
// returned and left to the caller to interpret.
//
// Requests which take a request body should provide a non-nil function
// parameter. The best choice is to provide either a function satisfying
// ReaderFunc which provides multiple io.Readers in an efficient manner, a
// *bytes.Buffer (the underlying raw byte slice will be used) or a raw byte
// slice. As it is a reference type, and we will wrap it as needed by readers,
// we can efficiently re-use the request body without needing to copy it. If an
// io.Reader (such as a *bytes.Reader) is provided, the full body will be read
// prior to the first request, and will be efficiently re-used for any retries.
// ReadSeeker can be used, but some users have observed occasional data races
// between the net/http library and the Seek functionality of some
// implementations of ReadSeeker, so should be avoided if possible.
package retryablehttp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/go-cleanhttp"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/lalamove/nui/ntracing"

	"github.com/lalamove/nui/nlogger"
)

var (
	// Default retry configuration
	defaultRetryWaitMin = 1 * time.Second
	defaultRetryWaitMax = 30 * time.Second
	defaultRetryMax     = 4

	// We need to consume response bodies to maintain http connections, but
	// limit the size we consume to respReadLimit.
	respReadLimit = int64(4096)
)

// ReaderFunc is the type of function that can be given natively to NewRequest
type ReaderFunc func() (io.Reader, error)

// LenReader is an interface implemented by many in-memory io.Reader's. Used
// for automatically sending the right Content-Length header when possible.
type LenReader interface {
	Len() int
}

// Request wraps the metadata needed to create HTTP requests.
type Request struct {
	// body is a seekable reader over the request body payload. This is
	// used to rewind the request data in between retries.
	body ReaderFunc

	// Embed an HTTP request directly. This makes a *Request act exactly
	// like an *http.Request so that all meta methods are supported.
	*http.Request
}

// WithContext returns wrapped Request with a shallow copy of underlying *http.Request
// with its context changed to ctx. The provided ctx must be non-nil.
func (r *Request) WithContext(ctx context.Context) *Request {
	r.Request = r.Request.WithContext(ctx)
	return r
}

// BodyBytes allows accessing the request body. It is an analogue to
// http.Request's Body variable, but it returns a copy of the underlying data
// rather than consuming it.
//
// This function is not thread-safe; do not call it at the same time as another
// call, or at the same time this request is being used with Client.Do.
func (r *Request) BodyBytes() ([]byte, error) {
	if r.body == nil {
		return nil, nil
	}
	body, err := r.body()
	if err != nil {
		return nil, err
	}
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(body)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// NewRequest creates a new wrapped request.
func NewRequest(method, url string, rawBody interface{}) (*Request, error) {
	var err error
	var body ReaderFunc
	var contentLength int64

	if rawBody != nil {
		switch rawBody.(type) {
		// If they gave us a function already, great! Use it.
		case ReaderFunc:
			body = rawBody.(ReaderFunc)
			tmp, err := body()
			if err != nil {
				return nil, err
			}
			if lr, ok := tmp.(LenReader); ok {
				contentLength = int64(lr.Len())
			}
			if c, ok := tmp.(io.Closer); ok {
				c.Close()
			}

		case func() (io.Reader, error):
			body = rawBody.(func() (io.Reader, error))
			tmp, err := body()
			if err != nil {
				return nil, err
			}
			if lr, ok := tmp.(LenReader); ok {
				contentLength = int64(lr.Len())
			}
			if c, ok := tmp.(io.Closer); ok {
				c.Close()
			}

		// If a regular byte slice, we can read it over and over via new
		// readers
		case []byte:
			buf := rawBody.([]byte)
			body = func() (io.Reader, error) {
				return bytes.NewReader(buf), nil
			}
			contentLength = int64(len(buf))

		// If a bytes.Buffer we can read the underlying byte slice over and
		// over
		case *bytes.Buffer:
			buf := rawBody.(*bytes.Buffer)
			body = func() (io.Reader, error) {
				return bytes.NewReader(buf.Bytes()), nil
			}
			contentLength = int64(buf.Len())

		// We prioritize *bytes.Reader here because we don't really want to
		// deal with it seeking so want it to match here instead of the
		// io.ReadSeeker case.
		case *bytes.Reader:
			buf, err := ioutil.ReadAll(rawBody.(*bytes.Reader))
			if err != nil {
				return nil, err
			}
			body = func() (io.Reader, error) {
				return bytes.NewReader(buf), nil
			}
			contentLength = int64(len(buf))

		// Compat case
		case io.ReadSeeker:
			raw := rawBody.(io.ReadSeeker)
			body = func() (io.Reader, error) {
				raw.Seek(0, 0)
				return ioutil.NopCloser(raw), nil
			}
			if lr, ok := raw.(LenReader); ok {
				contentLength = int64(lr.Len())
			}

		// Read all in so we can reset
		case io.Reader:
			buf, err := ioutil.ReadAll(rawBody.(io.Reader))
			if err != nil {
				return nil, err
			}
			body = func() (io.Reader, error) {
				return bytes.NewReader(buf), nil
			}
			contentLength = int64(len(buf))

		default:
			return nil, fmt.Errorf("cannot handle type %T", rawBody)
		}
	}

	httpReq, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	httpReq.ContentLength = contentLength

	return &Request{body, httpReq}, nil
}

// Logger interface allows to use other loggers than
// standard log.Logger.
type Logger = nlogger.Structured

// RequestModifier provides a function type which modifiers a giving
// request object, returning a new request object to be used.
type RequestModifier func(*Request) *Request

// RequestLogHook allows a function to run before each retry. The HTTP
// request which will be made, and the retry number (0 for the initial
// request) are available to users. The internal logger is exposed to
// consumers.
type RequestLogHook func(Logger, *http.Request, int)

// ResponseLogHook is like RequestLogHook, but allows running a function
// on each HTTP response. This function will be invoked at the end of
// every HTTP request executed, regardless of whether a subsequent retry
// needs to be performed or not. If the response body is read or closed
// from this method, this will affect the response returned from Do().
type ResponseLogHook func(Logger, *http.Response)

// CheckRetry specifies a policy for handling retries. It is called
// following each request with the response and error values returned by
// the http.Client. If CheckRetry returns false, the Client stops retrying
// and returns the response to the caller. If CheckRetry returns an error,
// that error value is returned in lieu of the error from the request. The
// Client will close any response body when retrying, but if the retry is
// aborted it is up to the CheckResponse callback to properly close any
// response body before returning.
type CheckRetry func(ctx context.Context, resp *http.Response, err error) (bool, error)

// Backoff specifies a policy for how long to wait between retries.
// It is called after a failing request to determine the amount of time
// that should pass before trying again.
type Backoff func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration

// ErrorHandler is called if retries are expired, containing the last status
// from the http library. If not specified, default behavior for the library is
// to close the body and return an error indicating how many tries were
// attempted. If overriding this, be sure to close the body if needed.
type ErrorHandler func(resp *http.Response, err error, numTries int) (*http.Response, error)

// Config is to be used to instantiate giving Client.
type Config struct {
	Metrics      bool          // Flag to enable metrics.
	RetryMax     int           // Maximum number of retries
	RetryWaitMin time.Duration // Minimum time to wait in retries
	RetryWaitMax time.Duration // Maximum time to wait in retries
	Logger       Logger        // Customer logger instance to be used.

	// HttpClient is the internal HTTP client.
	HttpClient *http.Client

	// RequestModifier allows a user-supplied function to be called
	// to modify a request object.
	RequestModifier RequestModifier

	// RequestLogHook allows a user-supplied function to be called
	// before each retry.
	RequestLogHook RequestLogHook

	// ResponseLogHook allows a user-supplied function to be called
	// with the response from each HTTP request executed.
	ResponseLogHook ResponseLogHook

	// CheckRetry specifies the policy for handling retries, and is called
	// after each request. The default policy is DefaultRetryPolicy.
	CheckRetry CheckRetry

	// Backoff specifies the policy for how long to wait between retries
	Backoff Backoff

	// ErrorHandler specifies the custom error handler to use, if any
	ErrorHandler ErrorHandler
}

func (c *Config) init() error {
	if c.Logger == nil {
		c.Logger = nlogger.New(os.Stderr, "[HTTP CLIENT]")
	}
	if c.HttpClient == nil {
		c.HttpClient = cleanhttp.DefaultClient()
	}
	if c.RetryMax <= 0 {
		c.RetryWaitMin = defaultRetryWaitMin
	}
	if c.RetryWaitMax <= 0 {
		c.RetryWaitMax = defaultRetryWaitMax
	}
	if c.CheckRetry == nil {
		c.CheckRetry = DefaultRetryPolicy
	}
	if c.Backoff == nil {
		c.Backoff = DefaultBackoff
	}
	if c.RetryMax <= 0 {
		c.RetryMax = defaultRetryMax
	}
	return nil
}

// Client is used to make HTTP requests. It adds additional functionality
// like automatic retries to tolerate minor outages.
type Client struct {
	*Config

	// metrics is the internal metrics generated to be used for
	// metric collection when enabled.
	metrics *retryHttpMetrics
}

// New creates a new Client with default settings.
func New(c *Config) (*Client, error) {
	var err error
	if err = c.init(); err != nil {
		return nil, err
	}

	var metrics *retryHttpMetrics
	if c.Metrics {
		metrics, err = initMetrics()
		if err != nil {
			return nil, err
		}
	}

	return &Client{
		Config:  c,
		metrics: metrics,
	}, nil
}

// DefaultRetryPolicy provides a default callback for Client.CheckRetry, which
// will retry on connection errors and server errors.
func DefaultRetryPolicy(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// do not retry on context.Canceled or context.DeadlineExceeded
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	if err != nil {
		return true, err
	}
	// Check the response code. We retry on 500-range responses to allow
	// the server time to recover, as 500's are typically not permanent
	// errors and may relate to outages on the server side. This will catch
	// invalid response codes as well, like 0 and 999.
	if resp.StatusCode == 0 || (resp.StatusCode >= 500 && resp.StatusCode != 501) {
		return true, nil
	}

	return false, nil
}

// DefaultBackoff provides a default callback for Client.Backoff which
// will perform exponential backoff based on the attempt number and limited
// by the provided minimum and maximum durations.
func DefaultBackoff(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
	mult := math.Pow(2, float64(attemptNum)) * float64(min)
	sleep := time.Duration(mult)
	if float64(sleep) != mult || sleep > max {
		sleep = max
	}
	return sleep
}

// LinearJitterBackoff provides a callback for Client.Backoff which will
// perform linear backoff based on the attempt number and with jitter to
// prevent a thundering herd.
//
// min and max here are *not* absolute values. The number to be multipled by
// the attempt number will be chosen at random from between them, thus they are
// bounding the jitter.
//
// For instance:
// * To get strictly linear backoff of one second increasing each retry, set
// both to one second (1s, 2s, 3s, 4s, ...)
// * To get a small amount of jitter centered around one second increasing each
// retry, set to around one second, such as a min of 800ms and max of 1200ms
// (892ms, 2102ms, 2945ms, 4312ms, ...)
// * To get extreme jitter, set to a very wide spread, such as a min of 100ms
// and a max of 20s (15382ms, 292ms, 51321ms, 35234ms, ...)
func LinearJitterBackoff(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
	// attemptNum always starts at zero but we want to start at 1 for multiplication
	attemptNum++

	if max <= min {
		// Unclear what to do here, or they are the same, so return min *
		// attemptNum
		return min * time.Duration(attemptNum)
	}

	// Seed rand; doing this every time is fine
	rand := rand.New(rand.NewSource(int64(time.Now().Nanosecond())))

	// Pick a random number that lies somewhere between the min and max and
	// multiply by the attemptNum. attemptNum starts at zero so we always
	// increment here. We first get a random percentage, then apply that to the
	// difference between min and max, and add to min.
	jitter := rand.Float64() * float64(max-min)
	jitterMin := int64(jitter) + int64(min)
	return time.Duration(jitterMin * int64(attemptNum))
}

// PassthroughErrorHandler is an ErrorHandler that directly passes through the
// values from the net/http library for the final request. The body is not
// closed.
func PassthroughErrorHandler(resp *http.Response, err error, _ int) (*http.Response, error) {
	return resp, err
}

// Do wraps calling an HTTP method with retries.
func (c *Client) Do(req *Request) (*http.Response, error) {
	if c.metrics != nil {
		c.metrics.doTotal.Inc()
		var timer = prometheus.NewTimer(c.metrics.doDuration)
		defer timer.ObserveDuration()
	}

	// If modifier is provided then modify request.
	if c.RequestModifier != nil {
		req = c.RequestModifier(req)
	}

	var ctx = req.Context()
	if span, ok := ntracing.NewChildSpanFromContext(ctx, "HttpClient.Do"); ok {
		defer span.Finish()

		ctx = context.WithValue(ctx, ntracing.SpanKey, span)
		req.WithContext(ctx)
	}

	c.Logger.DebugWithFields("Sending request for method", func(entry nlogger.Entry) {
		entry.String("method", req.Method)
		entry.String("url", req.URL.String())
	})

	var resp *http.Response
	var err error

	var retryTimer *prometheus.Timer
	for i := 0; ; i++ {
		if c.metrics != nil && i > 0 {
			retryTimer = prometheus.NewTimer(c.metrics.doRetryDuration)
			c.metrics.doRetries.Inc()
		}

		var code int // HTTP response code

		// Always rewind the request body when non-nil.
		if req.body != nil {
			body, err := req.body()
			if err != nil {
				if retryTimer != nil {
					retryTimer.ObserveDuration()
					retryTimer = nil
				}

				if c.metrics != nil {
					c.metrics.doFailure.Inc()
					if i > 0 {
						c.metrics.doRetriesFailure.Inc()
					}
				}
				return resp, err
			}
			if c, ok := body.(io.ReadCloser); ok {
				req.Request.Body = c
			} else {
				req.Request.Body = ioutil.NopCloser(body)
			}
		}

		if c.RequestLogHook != nil {
			c.RequestLogHook(c.Logger, req.Request, i)
		}

		// Attempt the request
		resp, err = c.HttpClient.Do(req.Request)
		if resp != nil {
			code = resp.StatusCode
		}

		// Check if we should continue with retries.
		checkOK, checkErr := c.CheckRetry(req.Request.Context(), resp, err)

		if retryTimer != nil {
			retryTimer.ObserveDuration()
			retryTimer = nil
		}

		if err != nil {
			if c.metrics != nil && i > 0 {
				c.metrics.doRetriesFailure.Inc()
			}

			c.Logger.ErrorWithFields(err.Error(), func(entry nlogger.Entry) {
				entry.String("method", req.Method)
				entry.String("url", req.URL.String())
			})
		} else {
			// Call this here to maintain the behavior of logging all requests,
			// even if CheckRetry signals to stop.
			if c.ResponseLogHook != nil {
				// Call the response logger function if provided.
				c.ResponseLogHook(c.Logger, resp)
			}
		}

		// Now decide if we should continue.
		if !checkOK {
			if checkErr != nil {
				err = checkErr
			}

			if c.metrics != nil {
				if err != nil {
					c.metrics.doFailure.Inc()
				} else {
					c.metrics.doSuccess.Inc()
				}
			}
			return resp, err
		}

		// We do this before drainBody beause there's no need for the I/O if
		// we're breaking out
		remain := c.RetryMax - i
		if remain <= 0 {
			if c.metrics != nil && err != nil {
				c.metrics.doFailure.Inc()
			}
			break
		}

		// We're going to retry, consume any response to reuse the connection.
		if err == nil && resp != nil {
			c.drainBody(resp.Body)
		}

		wait := c.Backoff(c.RetryWaitMin, c.RetryWaitMax, i, resp)
		desc := fmt.Sprintf("%s %s", req.Method, req.URL)
		if code > 0 {
			desc = fmt.Sprintf("%s (status: %d)", desc, code)
		}

		c.Logger.DebugWithFields("retrying http request", func(entry nlogger.Entry) {
			entry.Int("remain", remain)
			entry.String("desc", desc)
			entry.String("method", req.Method)
			entry.String("wait", wait.String())
			entry.String("url", req.URL.String())
		})

		time.Sleep(wait)
	}

	if c.ErrorHandler != nil {
		return c.ErrorHandler(resp, err, c.RetryMax+1)
	}

	// By default, we close the response body and return an error without
	// returning the response
	if resp != nil {
		resp.Body.Close()
	}

	if c.metrics != nil {
		c.metrics.doFailure.Inc()
	}
	return nil, fmt.Errorf("%s %s giving up after %d attempts",
		req.Method, req.URL, c.RetryMax+1)
}

// Try to read the response body so we can reuse this connection.
func (c *Client) drainBody(body io.ReadCloser) {
	defer body.Close()
	_, err := io.Copy(ioutil.Discard, io.LimitReader(body, respReadLimit))
	if err != nil {
		if c.Logger != nil {
			c.Logger.Error(err.Error())
		}
	}
}

// Get is a convenience helper for doing simple GET requests.
func (c *Client) Get(url string) (*http.Response, error) {
	req, err := NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// Head is a convenience method for doing simple HEAD requests.
func (c *Client) Head(url string) (*http.Response, error) {
	req, err := NewRequest("HEAD", url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// Post is a convenience method for doing simple POST requests.
func (c *Client) Post(url, bodyType string, body interface{}) (*http.Response, error) {
	req, err := NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", bodyType)
	return c.Do(req)
}

// PostForm is a convenience method for doing simple POST operations using
// pre-filled url.Values form data.
func (c *Client) PostForm(url string, data url.Values) (*http.Response, error) {
	return c.Post(url, "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
}
