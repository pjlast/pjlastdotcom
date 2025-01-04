package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/google/uuid"
)

type loggerCreator struct {
	baseLogger *slog.Logger
}

func (lc *loggerCreator) RequestLoggerFromContext(ctx context.Context) *slog.Logger {
	requestID := ctx.Value(requestIDKey).(string)
	return lc.baseLogger.With(slog.String("request_id", requestID))
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

const requestIDKey = iota

func generateRequestID() (string, error) {
	requestID, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}

	return requestID.String(), nil
}

// homeHandler handles both the home page as well as any
// requests that don't exist.
func homeHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || r.Method != http.MethodGet {
		http.NotFoundHandler().ServeHTTP(w, r)
		return
	}

	home().Render(r.Context(), w)
}

func requestLoggerMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID, err := generateRequestID()
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("something went wrong"))
				logger.LogAttrs(r.Context(), slog.LevelError, "error while generating UUID",
					slog.Any("error", err),
				)
				return
			}

			ctxWithReqID := context.WithValue(r.Context(), requestIDKey, requestID)
			r = r.WithContext(ctxWithReqID)

			logger := logger.With(
				slog.String("request_id", requestID),
			)

			logger.LogAttrs(r.Context(), slog.LevelInfo, "incoming request",
				slog.String("method", r.Method),
				slog.String("url", r.URL.String()),
				slog.String("address", r.RemoteAddr),
			)

			lrw := &loggingResponseWriter{ResponseWriter: w}

			requestStartTime := time.Now()

			handler.ServeHTTP(lrw, r)

			if lrw.statusCode == 0 {
				lrw.statusCode = http.StatusOK
			}

			requestDuration := time.Since(requestStartTime)

			logger.LogAttrs(r.Context(), slog.LevelInfo, "sending response",
				slog.Int("status_code", lrw.statusCode),
				slog.Int64("duration_ms", requestDuration.Milliseconds()),
			)
		})
	}
}

func run(ctx context.Context, stdout, stderr io.Writer) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	loggerHandler := slog.NewJSONHandler(stdout, nil)
	logger := slog.New(loggerHandler)

	router := http.NewServeMux()
	router.Handle("GET /css/", http.StripPrefix("/css/", http.FileServer(http.Dir("css"))))
	router.HandleFunc("/", homeHandler)

	address := net.JoinHostPort("127.0.0.1", "8080")
	httpServer := http.Server{
		Addr:     address,
		Handler:  requestLoggerMiddleware(logger)(router),
		ErrorLog: slog.NewLogLogger(loggerHandler, slog.LevelError),
	}

	go func() {
		logger.LogAttrs(ctx, slog.LevelInfo, "server started", slog.String("address", address))

		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "error listening and serving: %s\n", err)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		<-ctx.Done()
		shutdownCtx := context.Background()
		shutdownCtx, cancel := context.WithTimeout(shutdownCtx, 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "error shutting down http server: %s\n", err)
		}
	}()
	wg.Wait()

	return nil
}

func main() {
	ctx := context.Background()

	if err := run(ctx, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}
