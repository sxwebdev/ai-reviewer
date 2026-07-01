package server

import (
	"net"
	"net/http"
)

// auth enforces localhost-only access and a per-launch session token. The token
// arrives once via ?token=, is stored in a cookie, then stripped from the URL.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			http.Error(w, "forbidden: local access only", http.StatusForbidden)
			return
		}
		if c, err := r.Cookie("ai_session"); err == nil && c.Value == s.token {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Query().Get("token") == s.token {
			http.SetCookie(w, &http.Cookie{
				Name: "ai_session", Value: s.token, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			q := r.URL.Query()
			q.Del("token")
			r.URL.RawQuery = q.Encode()
			http.Redirect(w, r, r.URL.String(), http.StatusSeeOther)
			return
		}
		http.Error(w, "unauthorized: open the URL printed in the terminal (it contains a session token)", http.StatusUnauthorized)
	})
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
