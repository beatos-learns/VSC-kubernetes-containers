package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// The application handlers: the four server-side auth endpoints upstream
// implemented as Next route handlers (login/logout/me/signup, proxying to the
// backend with the httpOnly jwt cookie) and the static bundle with the
// upstream middleware's redirects.

func newAppHandler(cfg *config, log *logger, m *metrics) http.Handler {
	client := &http.Client{Timeout: cfg.apiTimeout, Transport: &instrumentedTransport{m: m, next: http.DefaultTransport}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) { handleLogin(cfg, client, w, r) })
	mux.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) { handleLogout(cfg, w, r) })
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) { handleMe(cfg, client, w, r) })
	mux.HandleFunc("/api/signup", func(w http.ResponseWriter, r *http.Request) { handleSignup(cfg, client, w, r) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { handleStatic(cfg, w, r) })
	return instrument(cfg, log, m, mux)
}

func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func hasJwtCookie(r *http.Request) bool {
	cookie, err := r.Cookie("jwt")
	return err == nil && cookie.Value != ""
}

func setJwtCookie(cfg *config, w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     "jwt",
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cfg.cookieSecure,
	})
}

// backendPost issues a JSON POST to the backend within the request's context
// (so the request id travels along and the client metrics see the route).
func backendPost(cfg *config, client *http.Client, r *http.Request, route string, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, cfg.apiURL+route, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return client.Do(request)
}

func handleLogin(cfg *config, client *http.Client, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method not allowed"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid body"})
		return
	}
	response, err := backendPost(cfg, client, r, "/users/login", body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Server error"})
		return
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Invalid credentials"})
		return
	}
	token := strings.TrimPrefix(response.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "No token"})
		return
	}
	setJwtCookie(cfg, w, token, cfg.cookieMaxAge)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func handleLogout(cfg *config, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method not allowed"})
		return
	}
	setJwtCookie(cfg, w, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func handleMe(cfg *config, client *http.Client, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method not allowed"})
		return
	}
	cookie, err := r.Cookie("jwt")
	if err != nil || cookie.Value == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return
	}
	request, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, cfg.apiURL+"/users/me", nil)
	request.Header.Set("Authorization", "Bearer "+cookie.Value)
	response, err := client.Do(request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Failed to fetch user"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, response.Body)
		writeJSON(w, response.StatusCode, map[string]any{"error": "Failed to fetch user"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, response.Body)
}

func handleSignup(cfg *config, client *http.Client, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method not allowed"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid body"})
		return
	}
	response, err := backendPost(cfg, client, r, "/users/register", body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Server error"})
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

// handleStatic serves the exported bundle and reproduces the upstream Next
// middleware: "/" redirects to /dashboard, /dashboard* requires the jwt
// cookie, /login bounces authenticated users back to the dashboard.
func handleStatic(cfg *config, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method not allowed"})
		return
	}
	clean := path.Clean("/" + r.URL.Path)
	authed := hasJwtCookie(r)
	switch {
	case clean == "/":
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	case clean == "/login" && authed:
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	case (clean == "/dashboard" || strings.HasPrefix(clean, "/dashboard/")) && !authed:
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	full, ok := resolveStatic(cfg.staticDir, clean)
	if !ok {
		notFound := filepath.Join(cfg.staticDir, "404.html")
		if content, readErr := os.ReadFile(notFound); readErr == nil {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(content)
			return
		}
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(clean, "/_next/static/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else if strings.HasSuffix(full, ".html") || strings.HasSuffix(full, ".txt") {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeFile(w, r, full)
}
