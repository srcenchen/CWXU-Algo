package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-kratos/aegis/circuitbreaker"
	"github.com/go-kratos/aegis/circuitbreaker/sre"
	config "github.com/go-kratos/gateway/api/gateway/config/v1"
	"github.com/go-kratos/gateway/client"
	"github.com/go-kratos/gateway/middleware"
	"github.com/go-kratos/gateway/router"
	"github.com/go-kratos/gateway/router/mux"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/selector"
)

// _defaultMaxRequestBodyBytes 非流式端点请求体默认上限（16MB）。
// 非流式请求会被整体读入内存用于重试，必须设上限防止大请求打爆网关内存。
// 可用环境变量 PROXY_MAX_REQUEST_BODY_BYTES 覆盖全局默认；
// 单个端点可用 metadata.maxRequestBodyBytes 覆盖；大上传路由请直接配置 stream: true 走流式转发。
var _defaultMaxRequestBodyBytes = int64(16 << 20)

func init() {
	if v := os.Getenv("PROXY_MAX_REQUEST_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			panic("invalid PROXY_MAX_REQUEST_BODY_BYTES: " + v)
		}
		_defaultMaxRequestBodyBytes = n
	}
}

// endpointMaxBodyBytes 端点级请求体上限：metadata.maxRequestBodyBytes 优先，否则全局默认。
func endpointMaxBodyBytes(e *config.Endpoint) int64 {
	if e != nil && e.Metadata != nil {
		if raw, ok := e.Metadata["maxRequestBodyBytes"]; ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n > 0 {
				return n
			}
			log.Warnf("invalid maxRequestBodyBytes metadata on endpoint %s %s, fallback to default", e.Method, e.Path)
		}
	}
	return _defaultMaxRequestBodyBytes
}

// Option is proxy option.
type Option func(*Proxy)

// WithObservable set observable option.
func WithObservable(o Observable) Option {
	return func(p *Proxy) {
		p.observable = o
	}
}

// WithNotFoundHandler set not found handler option.
func WithNotFoundHandler(h http.Handler) Option {
	return func(p *Proxy) {
		p.notFoundHandler = h
	}
}

// WithMethodNotAllowedHandler set method not allowed handler option.
func WithMethodNotAllowedHandler(h http.Handler) Option {
	return func(p *Proxy) {
		p.methodNotAllowedHandler = h
	}
}

// WithAttemptTimeoutContext set attempt timeout context option.
func WithAttemptTimeoutContext(f AttemptTimeoutContext) Option {
	return func(p *Proxy) {
		p.prepareAttemptTimeoutContext = f
	}
}

// AttemptTimeoutContext is a function type that prepares a context with timeout for an HTTP request.
type AttemptTimeoutContext func(ctx context.Context, req *http.Request, timeout time.Duration) (context.Context, context.CancelFunc)

// Proxy is a gateway proxy.
type Proxy struct {
	router                       atomic.Value
	clientFactory                client.Factory
	middlewareFactory            middleware.FactoryV2
	observable                   Observable
	notFoundHandler              http.Handler
	methodNotAllowedHandler      http.Handler
	prepareAttemptTimeoutContext AttemptTimeoutContext
}

// New is new a gateway proxy.
func New(clientFactory client.Factory, middlewareFactory middleware.FactoryV2, opts ...Option) (*Proxy, error) {
	p := &Proxy{
		clientFactory:                clientFactory,
		middlewareFactory:            middlewareFactory,
		prepareAttemptTimeoutContext: defaultAttemptTimeoutContext,
		notFoundHandler:              http.HandlerFunc(notFoundHandler),
		methodNotAllowedHandler:      http.HandlerFunc(methodNotAllowedHandler),
	}
	for _, opt := range opts {
		opt(p)
	}
	// if no observer is provided, create a default one and discovery metrics
	if p.observable == nil {
		p.observable = NewObservable()
	}
	p.router.Store(mux.NewRouter(p.notFoundHandler, p.methodNotAllowedHandler))
	return p, nil
}

func (p *Proxy) buildMiddleware(ms []*config.Middleware, next http.RoundTripper) (http.RoundTripper, error) {
	for i := len(ms) - 1; i >= 0; i-- {
		m, err := p.middlewareFactory(ms[i])
		if err != nil {
			if errors.Is(err, middleware.ErrNotFound) {
				log.Errorf("Skip does not exist middleware: %s", ms[i].Name)
				continue
			}
			return nil, err
		}
		next = m.Process(next)
	}
	return next, nil
}

func (p *Proxy) buildEndpoint(buildCtx *client.BuildContext, e *config.Endpoint, ms []*config.Middleware) (_ http.Handler, _ io.Closer, retError error) {
	client, err := p.clientFactory(buildCtx, e)
	if err != nil {
		return nil, nil, err
	}
	tripper := http.RoundTripper(client)
	closer := io.Closer(client)
	defer closeOnError(closer, &retError)

	if e.Stream {
		tripper = builtinStreamTripper(tripper)
	}
	tripper, err = p.buildMiddleware(e.Middlewares, tripper)
	if err != nil {
		return nil, nil, err
	}
	tripper, err = p.buildMiddleware(ms, tripper)
	if err != nil {
		return nil, nil, err
	}
	retryStrategy, err := prepareRetryStrategy(e)
	if err != nil {
		return nil, nil, err
	}
	maxBodyBytes := endpointMaxBodyBytes(e)
	observer := p.observable.Observe(e)
	markSuccessStat, markFailedStat, markBreakerStat := splitRetryMetricsHandler(observer)
	retryBreaker := sre.NewBreaker(sre.WithSuccess(0.8), sre.WithRequest(10))
	markSuccess := func(w http.ResponseWriter, req *http.Request, i int) {
		markSuccessStat(w, req, i)
		if i > 0 {
			retryBreaker.MarkSuccess()
		}
	}
	markFailed := func(w http.ResponseWriter, req *http.Request, i int, err error) {
		markFailedStat(w, req, i, err)
		if i > 0 {
			retryBreaker.MarkFailed()
		}
	}
	markBreaker := func(w http.ResponseWriter, req *http.Request, i int) {
		markBreakerStat(w, req, i)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		startTime := time.Now()
		setXFFHeader(req)

		reqOpts := middleware.NewRequestOptions(e)
		ctx := middleware.NewRequestContext(req.Context(), reqOpts)
		ctx, cancel := context.WithTimeout(ctx, retryStrategy.timeout)
		defer cancel()
		defer func() {
			observer.HandleLatency(req, time.Since(startTime))
		}()

		proxyStream := func() {
			reqOpts.LastAttempt = true
			streamCtx := &middleware.MetaStreamContext{}
			defer streamCtx.DoOnFinish()
			middleware.InitMetaStreamContext(reqOpts, streamCtx)
			wrapStreamRequestBody(req, streamCtx)
			defer req.Body.Close()
			reverseProxy := &httputil.ReverseProxy{
				Rewrite: func(proxyRequest *httputil.ProxyRequest) {},
				ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
					reqOpts.DoneFunc(ctx, selector.DoneInfo{Err: err})
					markFailed(w, req, 0, err)
					writeError(w, req, e, err, observer)
				},
				ModifyResponse: func(resp *http.Response) error {
					defer streamCtx.DoOnResponse()
					reqOpts.DoneFunc(ctx, selector.DoneInfo{ReplyMD: getReplyMD(e, resp)})
					markSuccess(w, req, 0)
					observer.HandleRequest(req, w.Header(), resp.StatusCode, nil)
					return nil
				},
				Transport:     tripper,
				FlushInterval: -1,
			}
			reverseProxy.ServeHTTP(w, req.Clone(ctx))
		}
		if e.Stream {
			proxyStream()
			return
		}

		// 非流式请求整体读入内存用于重试：按端点上限截断，超限直接 413
		body, err := io.ReadAll(io.LimitReader(req.Body, maxBodyBytes+1))
		if err != nil {
			writeError(w, req, e, err, observer)
			return
		}
		if int64(len(body)) > maxBodyBytes {
			log.Warnf("request body exceeds limit %d bytes on endpoint: [%s] %s %s", maxBodyBytes, e.Protocol, e.Method, e.Path)
			observer.HandleRequest(req, w.Header(), http.StatusRequestEntityTooLarge, nil)
			http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
			return
		}
		observer.HandleReceivedBytes(req, int64(len(body)))
		req.GetBody = func() (io.ReadCloser, error) {
			reader := bytes.NewReader(body)
			return io.NopCloser(reader), nil
		}

		var resp *http.Response
		// 每次 attempt 结束时显式 cancel 上一次的 context，避免重试循环内 defer 堆积；
		// 最后一次成功 attempt 的 cancel 延迟到响应体拷贝完成（handler 返回）后执行。
		var attemptCancel context.CancelFunc
		defer func() {
			if attemptCancel != nil {
				attemptCancel()
			}
		}()
		for i := 0; i < retryStrategy.attempts; i++ {
			if i > 0 {
				if !retryFeature.Enabled() {
					break
				}
				if err := retryBreaker.Allow(); err != nil {
					if errors.Is(err, circuitbreaker.ErrNotAllowed) {
						markBreaker(w, req, i)
					} else {
						markFailed(w, req, i, err)
					}
					break
				}
			}

			if (i + 1) >= retryStrategy.attempts {
				reqOpts.LastAttempt = true
			}
			// canceled or deadline exceeded
			if err = ctx.Err(); err != nil {
				markFailed(w, req, i, err)
				break
			}
			if attemptCancel != nil {
				attemptCancel()
			}
			tryCtx, cancel := p.prepareAttemptTimeoutContext(ctx, req, retryStrategy.perTryTimeout)
			attemptCancel = cancel
			reader := bytes.NewReader(body)
			req.Body = io.NopCloser(reader)
			resp, err = tripper.RoundTrip(req.Clone(tryCtx))
			if err != nil {
				markFailed(w, req, i, err)
				log.Errorf("Attempt at [%d/%d], failed to handle request: %s: %+v", i+1, retryStrategy.attempts, req.URL.String(), err)
				continue
			}
			if !judgeRetryRequired(retryStrategy.conditions, resp) {
				reqOpts.LastAttempt = true
				markSuccess(w, req, i)
				break
			}
			markFailed(w, req, i, errors.New("assertion failed"))
			resp.Body.Close()
			// continue the retry loop
		}
		if err != nil {
			writeError(w, req, e, err, observer)
			return
		}

		headers := w.Header()
		for k, v := range resp.Header {
			headers[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		// flush any non grpc-status headers immediately for HTTP/2 GRPC requests.
		// otherwise, the http2 server will send `content-length: 0` in error response,
		// which will cause many reverse proxy into unexpected state.
		if reqOpts.Endpoint.Protocol == config.Protocol_GRPC &&
			req.ProtoMajor == 2 &&
			resp.Header.Get("grpc-status") == "" {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		doCopyBody := func() (bool, error) {
			if resp.Body == nil {
				return true, nil
			}
			defer resp.Body.Close()

			copyFunc := io.Copy
			if isNoBufferingResponse(resp) {
				copyFunc = copyNoBuffering(w)
			}
			sent, err := copyFunc(w, resp.Body)
			if err != nil {
				observer.HandleSentBytes(req, sent)
				reqOpts.DoneFunc(ctx, selector.DoneInfo{Err: err})
				log.Errorf("Failed to copy backend response body to client: [%s] %s %s %d %+v\n", e.Protocol, e.Method, e.Path, sent, err)
				return false, err
			}
			observer.HandleSentBytes(req, sent)
			reqOpts.DoneFunc(ctx, selector.DoneInfo{ReplyMD: getReplyMD(e, resp)})
			// see https://pkg.go.dev/net/http#example-ResponseWriter-Trailers
			for k, v := range resp.Trailer {
				headers[http.TrailerPrefix+k] = v
			}
			return true, nil
		}
		_, err = doCopyBody()
		observer.HandleRequest(req, headers, resp.StatusCode, err)
	}), closer, nil
}

// Update updates service endpoint.
func (p *Proxy) Update(buildContext *client.BuildContext, c *config.Gateway) (retError error) {
	router := mux.NewRouter(http.HandlerFunc(notFoundHandler), http.HandlerFunc(methodNotAllowedHandler))
	for _, e := range c.Endpoints {
		handler, closer, err := p.buildEndpoint(buildContext, e, c.Middlewares)
		if err != nil {
			return err
		}
		defer closeOnError(closer, &retError)
		if err = router.Handle(e.Path, e.Method, e.Host, handler, closer); err != nil {
			return err
		}
		log.Infof("build endpoint: [%s] %s %s", e.Protocol, e.Method, e.Path)
	}
	old := p.router.Swap(router)
	tryCloseRouter(old)
	return nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	defer func() {
		if err := recover(); err != nil {
			if err == http.ErrAbortHandler {
				return
			}
			w.WriteHeader(http.StatusBadGateway)
			buf := make([]byte, 64<<10) //nolint:gomnd
			n := runtime.Stack(buf, false)
			log.Errorf("panic recovered: %+v\n%s", err, buf[:n])
			fmt.Fprintf(os.Stderr, "panic recovered: %+v\n%s\n", err, buf[:n])
		}
	}()
	p.router.Load().(router.Router).ServeHTTP(w, req)
}

// DebugHandler implemented debug handler.
func (p *Proxy) DebugHandler() http.Handler {
	debugMux := http.NewServeMux()
	debugMux.HandleFunc("/debug/proxy/router/inspect", func(rw http.ResponseWriter, r *http.Request) {
		router, ok := p.router.Load().(router.Router)
		if !ok {
			return
		}
		inspect := mux.InspectMuxRouter(router)
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(inspect)
	})
	return debugMux
}

func getReplyMD(ep *config.Endpoint, resp *http.Response) selector.ReplyMD {
	if ep.Protocol == config.Protocol_GRPC {
		return resp.Trailer
	}
	return resp.Header
}

func closeOnError(closer io.Closer, err *error) {
	if *err == nil {
		return
	}
	closer.Close()
}

func tryCloseRouter(in interface{}) {
	if in == nil {
		return
	}
	r, ok := in.(router.Router)
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		r.SyncClose(ctx)
	}()
}
func splitRetryMetricsHandler(observer Observer) (
	func(http.ResponseWriter, *http.Request, int), func(http.ResponseWriter, *http.Request, int, error), func(http.ResponseWriter, *http.Request, int)) {
	// success marks a successful retry attempt
	success := func(w http.ResponseWriter, req *http.Request, i int) {
		if i <= 0 {
			return
		}
		observer.HandleRetry(req, w.Header(), "true")
	}
	failed := func(w http.ResponseWriter, req *http.Request, i int, err error) {
		if i <= 0 {
			return
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		observer.HandleRetry(req, w.Header(), "false")
	}
	breaker := func(w http.ResponseWriter, req *http.Request, i int) {
		if i <= 0 {
			return
		}
		observer.HandleRetry(req, w.Header(), "breaker")
	}
	return success, failed, breaker
}

func isWebSocketRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Connection"), "Upgrade") &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func wrapStreamRequestBody(req *http.Request, ctxValue *middleware.MetaStreamContext) {
	if req.Body == nil {
		return
	}
	switch req.ProtoMajor {
	case 1:
		// the websocket request body does not need to be wrapped, all data will be received in response body
		if isWebSocketRequest(req) {
			return
		}
		req.Body = middleware.WrapReadCloserBody(req.Body, middleware.TagRequest, ctxValue)
		return
	case 2:
		req.Body = middleware.WrapReadCloserBody(req.Body, middleware.TagRequest, ctxValue)
	}
}

func wrapStreamResponseBody(resp *http.Response, ctxValue *middleware.MetaStreamContext) {
	if resp.Body == nil {
		return
	}
	switch resp.ProtoMajor {
	case 1:
		// websocket
		rwc, ok := resp.Body.(io.ReadWriteCloser)
		if ok {
			resp.Body = middleware.WrapReadWriteCloserBody(rwc, ctxValue)
			return
		}
		// common http1.x response body
		resp.Body = middleware.WrapReadCloserBody(resp.Body, middleware.TagResponse, ctxValue)
	case 2:
		resp.Body = middleware.WrapReadCloserBody(resp.Body, middleware.TagResponse, ctxValue)
	}
}

func builtinStreamTripper(tripper http.RoundTripper) http.RoundTripper {
	return middleware.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		reqOpts, ok := middleware.FromRequestContext(req.Context())
		if !ok {
			return tripper.RoundTrip(req)
		}
		streamCtx, ok := middleware.GetMetaStreamContext(reqOpts)
		if !ok {
			return tripper.RoundTrip(req)
		}
		streamCtx.Request = req
		resp, err := tripper.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		streamCtx.Response = resp
		wrapStreamResponseBody(resp, streamCtx)
		return resp, nil
	})
}

func setXFFHeader(req *http.Request) {
	// see https://github.com/golang/go/blob/master/src/net/http/httputil/reverseproxy.go
	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		prior, ok := req.Header["X-Forwarded-For"]
		omit := ok && prior == nil // Issue 38079: nil now means don't populate the header
		if len(prior) > 0 {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		if !omit {
			req.Header.Set("X-Forwarded-For", clientIP)
		}
	}
}
