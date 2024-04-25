package main

import (
    "io"
    "net/http"
    "net/http/httputil"
    "log"
    "crypto/sha1"
    "encoding/hex"
    "net/url"
    "time"
    "sync"
    "github.com/ltkh/montools/internal/monitor"
    "github.com/prometheus/client_golang/prometheus"
    "bytes"
)

var (
    reverseProxy *httputil.ReverseProxy
    reverseProxyOnce sync.Once
)

type Api struct {
    Upstream *Upstream
    Debug    bool
}

func apiNew(upstream *Upstream, debug bool) (*Api, error) {
    return &Api{ Upstream: upstream, Debug: debug }, nil
}

func initReverseProxy() {
    errorLogger := log.New(new(bytes.Buffer), "[error]", 0)

    reverseProxy = &httputil.ReverseProxy{
        Director: func(r *http.Request) {
            targetURL := r.Header.Get("proxy-target-url")
            target, err := url.Parse(targetURL)
            if err != nil {
                log.Printf("[error] unexpected error when parsing targetURL=%q: %s", targetURL, err)
                return
            }
            target.Path = target.Path+r.URL.Path
            target.RawQuery = r.URL.RawQuery
            r.URL = target
        },
        Transport: func() *http.Transport {
            tr := http.DefaultTransport.(*http.Transport).Clone()
            tr.DisableCompression = true
            tr.ForceAttemptHTTP2 = false
            tr.MaxIdleConnsPerHost = 100
            if tr.MaxIdleConns != 0 && tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
                tr.MaxIdleConns = tr.MaxIdleConnsPerHost
            }
            return tr
        }(),
        FlushInterval: time.Second,
        //ErrorLog: logger.StdErrorLogger(),
        //ErrorLog: log.New(new(bytes.Buffer), "", 0),
        ErrorLog: errorLogger,

    }
}

func getReverseProxy() *httputil.ReverseProxy {
    reverseProxyOnce.Do(initReverseProxy)
    return reverseProxy
}

func getStringHash(text string) string {
    h := sha1.New()
    io.WriteString(h, text)
    return hex.EncodeToString(h.Sum(nil))
}

func getPrefixURL(prefix []*URLPrefix) *URLPrefix {
    var urlPrefix *URLPrefix

    if len(prefix) == 0 {
        return urlPrefix
    }
    
    requests := 1000000

    for _, up := range prefix {
        if len(up.Health) != 0 {
            continue
        }

        if len(up.Requests) < requests {
            requests = len(up.Requests)
            urlPrefix = up
        }
    }

    return urlPrefix
}

func (api *Api) reverseProxy(w http.ResponseWriter, r *http.Request){
    monitor.RequestTotal.With(prometheus.Labels{"listen_addr": api.Upstream.ListenAddr}).Inc()

    for _, mapPath := range api.Upstream.MapPaths {
        if mapPath.RE.Match([]byte(r.URL.Path)) {

            if api.Debug {
                log.Printf("[debug] proxy match found Path - %v", r.URL.Path)
            }

            if len(api.Upstream.URLMap[mapPath.index].Users) > 0 {
                username, password, ok := r.BasicAuth()
                if !ok {
                    w.WriteHeader(401)
                    return
                }
                mPassword, ok := api.Upstream.URLMap[mapPath.index].MapUsers[username]
                if !ok {
                    w.WriteHeader(403)
                    return
                }
                if getStringHash(password) != mPassword {
                    w.WriteHeader(403)
                    return
                }
            }

            if urlPrefix := getPrefixURL(api.Upstream.URLMap[mapPath.index].URLPrefix); urlPrefix != nil {
                
                if api.Debug {
                    log.Printf("[debug] proxy match found URL - %v", urlPrefix.URL)
                }

                monitor.ProxyTotal.With(prometheus.Labels{"url_prefix": urlPrefix.URL}).Inc()
                if len(urlPrefix.Requests) < 1000000 {
                    urlPrefix.Requests <- 1
                }
                r.Header.Set("proxy-target-url", urlPrefix.URL)
                getReverseProxy().ServeHTTP(w, r)
                if len(urlPrefix.Requests) > 0 {
                    <- urlPrefix.Requests
                }
                return
            }

            w.WriteHeader(502)
            return
        }
    }

    w.WriteHeader(404)
    return
}

func (api *Api) healthCheck(w http.ResponseWriter, r *http.Request){
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")

    for _, urlMap := range api.Upstream.URLMap {
        for _, urlPrefix := range urlMap.URLPrefix {
            if len(urlPrefix.Health) == 0 {
                w.WriteHeader(200)
                w.Write([]byte("OK"))
                return
            }
        }
    }

    w.WriteHeader(502)
    return
}
