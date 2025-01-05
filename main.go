package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/a-h/templ"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/activeterm"
	"github.com/charmbracelet/wish/bubbletea"
	"github.com/charmbracelet/wish/logging"
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

// homeAndNotFoundHandler handles both the home page as well as any
// requests that don't exist.
func homeAndNotFoundHandler(w http.ResponseWriter, r *http.Request) {
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

func runSSH(ctx context.Context) error {
	s, err := wish.NewServer(
		wish.WithAddress("0.0.0.0:23423"),
		wish.WithHostKeyPath("/app/.ssh/id_ed25519"),
		wish.WithMiddleware(
			bubbletea.Middleware(teaHandler),
			activeterm.Middleware(),
			logging.Middleware(),
		),
	)
	if err != nil {
		return err
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	log.Info("starting SSH server", "host", "0.0.0.0", "port", "23423")

	go func() {
		if err := s.ListenAndServe(); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			log.Error("could not start server", "error", err)
			done <- nil
		}
	}()

	<-done
	log.Info("stopping SSH server")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer func() { cancel() }()
	if err := s.Shutdown(ctx); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
		log.Error("could not stop server", "error", err)
	}

	return nil
}

func teaHandler(s ssh.Session) (tea.Model, []tea.ProgramOption) {
	pty, _, _ := s.Pty()

	renderer := bubbletea.MakeRenderer(s)
	txtStyle := renderer.NewStyle().Foreground(lipgloss.Color("10"))
	quitStyle := renderer.NewStyle().Foreground(lipgloss.Color("8"))

	bg := "light"
	if renderer.HasDarkBackground() {
		bg = "dark"
	}

	m := model{
		term:      pty.Term,
		profile:   renderer.ColorProfile().Name(),
		width:     pty.Window.Width,
		height:    pty.Window.Height,
		bg:        bg,
		txtStyle:  txtStyle,
		quitStyle: quitStyle,
	}

	return m, []tea.ProgramOption{tea.WithAltScreen()}
}

type model struct {
	term      string
	profile   string
	width     int
	height    int
	bg        string
	txtStyle  lipgloss.Style
	quitStyle lipgloss.Style
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m model) View() string {
	s := fmt.Sprintf("Your term is %s\nYour window size is %dx%d\nBackground: %s\nColor Profile: %s", m.term, m.width, m.height, m.bg, m.profile)
	return m.txtStyle.Render(s) + "\n\n" + m.quitStyle.Render("Press 'q' to quit\n")
}

func run(ctx context.Context, stdout, stderr io.Writer) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	loggerHandler := slog.NewJSONHandler(stdout, nil)
	logger := slog.New(loggerHandler)

	router := http.NewServeMux()
	router.Handle("GET /css/", http.StripPrefix("/css/", http.FileServer(http.Dir("css"))))
	router.Handle("GET /images/", http.StripPrefix("/images/", http.FileServer(http.Dir("images"))))
	router.Handle("GET /js/", http.StripPrefix("/js/", http.FileServer(http.Dir("js"))))
	router.Handle("GET /writing", templ.Handler(writing()))
	router.Handle("GET /writing/hidden-simplicity", templ.Handler(hiddenSimplicity()))
	router.Handle("GET /writing/afrikaans-yugioh-1", templ.Handler(afrikaansYugioh1()))
	router.Handle("GET /work-history", templ.Handler(workHistory()))
	router.HandleFunc("/", homeAndNotFoundHandler)

	address := net.JoinHostPort("0.0.0.0", "8080")
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

	go func() {
		runSSH(ctx)
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
