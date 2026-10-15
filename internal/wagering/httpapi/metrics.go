package httpapi

import (
	"net/http"
	"strings"
	"time"
)

type responseStatus struct {
	http.ResponseWriter
	status int
}

func (w *responseStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseStatus) WriteHeader(status int) {
	if w.status != 0 {
		return
	}

	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseStatus) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}

	return w.ResponseWriter.Write(data)
}

func (a *API) instrument(pattern string, next http.HandlerFunc) http.HandlerFunc {
	_, route, _ := strings.Cut(pattern, " ")

	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		response := &responseStatus{ResponseWriter: w}

		defer func() {
			status := response.status
			if status == 0 {
				status = http.StatusOK
			}

			a.metrics.ObserveHTTP(route, r.Method, status, time.Since(start))
		}()

		next(response, r)
	}
}
