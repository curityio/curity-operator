package health

import (
	"net/http"

	"github.com/sirupsen/logrus"
)

func Start(addr string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	logrus.Infof("health endpoints listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		logrus.Fatalf("health server failed: %v", err)
	}
}
