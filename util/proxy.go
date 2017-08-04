package util

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"encoding/base64"
)

func NewSingleHostProxy(target *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := func(req *http.Request) {
		proxy.Director(req)
		req.URL.Host = target.Host
		req.URL.Scheme = target.Scheme
		targetUser := target.User
		if targetUser != nil {
			username := targetUser.Username()
			password, _ := targetUser.Password()
			auth := username + ":" + password
			enc := base64.StdEncoding.EncodeToString([]byte(auth))
			req.Header.Add("Authorization", "Basic " + enc)
		}
	}
	return &httputil.ReverseProxy{Director: director}
}
