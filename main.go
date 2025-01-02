package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
)

func homePage(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(202)
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func requestLoggerMiddleware(logger *slog.Logger, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID, err := uuid.NewRandom()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("something went wrong"))
			logger.LogAttrs(r.Context(), slog.LevelError, "error while generating UUID",
				slog.Any("error", err),
			)
			return
		}

		logger := logger.With(
			slog.String("requestId", requestID.String()),
		)

		logger.LogAttrs(r.Context(), slog.LevelInfo, "incoming request",
			slog.String("method", r.Method),
			slog.String("url", r.URL.String()),
			slog.String("address", r.RemoteAddr),
		)

		lrw := &loggingResponseWriter{ResponseWriter: w}

		requestStartTime := time.Now()

		next(lrw, r)

		requestDuration := time.Since(requestStartTime)

		logger.LogAttrs(r.Context(), slog.LevelInfo, "sending response",
			slog.Int("status_code", lrw.statusCode),
			slog.Int64("duration_ms", requestDuration.Milliseconds()),
		)
	}
}

func main() {
	ctx := context.Background()

	loggerHandler := slog.NewJSONHandler(os.Stdout, nil)
	logger := slog.New(loggerHandler)

	router := http.NewServeMux()
	router.HandleFunc("GET /", requestLoggerMiddleware(logger, homePage))

	address := "127.0.0.1:8080"
	server := http.Server{
		Addr:     address,
		Handler:  router,
		ErrorLog: slog.NewLogLogger(loggerHandler, slog.LevelError),
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "server started", slog.String("address", address))

	err := server.ListenAndServe()

	logger.LogAttrs(ctx, slog.LevelError, "server stopped", slog.Any("error", err))
}
