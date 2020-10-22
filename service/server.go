package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Pippolo84/rss-service/rss"
	"github.com/gorilla/mux"
	"github.com/mmcdole/gofeed"
)

const (
	// SrvReadTimeout is the http server read timeout
	SrvReadTimeout time.Duration = 5 * time.Second
	// SrvWriteTimeout is the http server write timeout
	SrvWriteTimeout time.Duration = 10 * time.Second
	// SrvIdleTimeout is the http server idle timeout
	SrvIdleTimeout time.Duration = 60 * time.Second

	// MaxRequestBodySize is the maximum size of a request body
	MaxRequestBodySize int64 = 1048576

	// RSSRequestTimeout is the timeout length for a request to an external RSS feed
	RSSRequestTimeout time.Duration = 5 * time.Second
)

// Service is the type of a rss service
type Service struct {
	server *http.Server
	router *mux.Router
	log    *log.Logger

	feeds map[string]string
	mu    sync.Mutex

	fetcher rss.Fetcher
}

// NewService builds a new Service ready to be run
func NewService(addr string) *Service {
	svc := &Service{
		server: &http.Server{
			Addr:         addr,
			ReadTimeout:  SrvReadTimeout,
			WriteTimeout: SrvWriteTimeout,
			IdleTimeout:  SrvIdleTimeout,
		},
		router:  mux.NewRouter(),
		log:     log.New(os.Stdout, "rss-service ", log.LstdFlags),
		feeds:   make(map[string]string),
		fetcher: rss.NewClient(),
	}
	svc.server.Handler = svc.router
	svc.router.HandleFunc("/feeds", svc.getFeeds).Schemes("http").Methods(http.MethodGet)
	svc.router.HandleFunc("/feed", svc.addFeed).Schemes("http").Methods(http.MethodPost)
	svc.router.HandleFunc("/items", svc.streamItems).Schemes("http").Methods(http.MethodGet)

	svc.server.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		dc := NewDeadlineController(c, svc.server)
		return context.WithValue(ctx, DeadlineControllerKey, dc)
	}

	return svc
}

// Run starts the service
// It takes a *sync.WaitGroup to signal when the init phase is finished and the server
// is about to call ListenAndServer
// It returns a channel where all the errors are forwarded
func (svc *Service) Run(wg *sync.WaitGroup) <-chan error {
	svcErrs := make(chan error)

	go func() {
		svc.log.Println("Run START")

		defer func() {
			close(svcErrs)
			svc.log.Println("Run STOPPED")
		}()

		wg.Done()
		if err := svc.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			svc.log.Printf("Run error: %v\n", err)
			svcErrs <- err
		}
	}()

	return svcErrs
}

// Shutdown shuts down the service
// It takes a context that it's passed to the underling HTTP server to control the shutdown process
// and a *sync.WaitGroup to signal when the shutdown process is ended
func (svc *Service) Shutdown(ctx context.Context, wg *sync.WaitGroup) error {
	defer wg.Done()
	return svc.server.Shutdown(ctx)
}

// DeadlineController holds a reference to the underlying TCP connection
// and a reference to the HTTP server serving the request
// see https://github.com/golang/go/issues/16100 for more information
type DeadlineController struct {
	c net.Conn
	s *http.Server
}

// ContextKey is the custom type of key to use with
type ContextKey string

// DeadlineControllerKey is the key for the context value of the deadline controller
const DeadlineControllerKey ContextKey = "deadline-controller"

// NewDeadlineController builds a new DeadlineController
func NewDeadlineController(c net.Conn, s *http.Server) *DeadlineController {
	return &DeadlineController{
		c: c,
		s: s,
	}
}

// ExtendWriteDeadline extends the current write deadline
func (dc *DeadlineController) ExtendWriteDeadline() error {
	return dc.c.SetWriteDeadline(time.Now().Add(dc.s.WriteTimeout))
}

// Feed holds information about a RSS feed
type Feed struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// News holds information about a single item from a RSS feed
type News struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

func (svc *Service) getFeeds(w http.ResponseWriter, r *http.Request) {
	svc.log.Println("GET /feeds")
	defer svc.log.Println("GET /feeds DONE")

	svc.mu.Lock()
	defer svc.mu.Unlock()

	feeds := make([]Feed, len(svc.feeds))
	i := 0
	for name, url := range svc.feeds {
		feeds[i] = Feed{name, url}
		i++
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(feeds); err != nil {
		_ = json.NewEncoder(w).Encode(struct {
			Code int    `json:"code"`
			Text string `json:"text"`
		}{
			Code: http.StatusInternalServerError,
			Text: http.StatusText(http.StatusInternalServerError),
		})
	}
}

func (svc *Service) addFeed(w http.ResponseWriter, r *http.Request) {
	svc.log.Println("POST /feed")
	defer svc.log.Println("POST /feed DONE")

	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)
		return
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxRequestBodySize))
	dec.DisallowUnknownFields()

	var feed Feed
	if err := dec.Decode(&feed); err != nil {
		var syntaxError *json.SyntaxError
		var unmarshalTypeError *json.UnmarshalTypeError

		switch {
		case errors.As(err, &syntaxError):
			http.Error(
				w,
				fmt.Sprintf("Badly-formed JSON (at position %d)", syntaxError.Offset),
				http.StatusBadRequest,
			)
		case errors.As(err, &unmarshalTypeError):
			http.Error(
				w,
				fmt.Sprintf("Invalid value for the %q field (at position %d)", unmarshalTypeError.Field, unmarshalTypeError.Offset),
				http.StatusBadRequest,
			)
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			http.Error(
				w,
				fmt.Sprintf("Request body contains unknown field %s", strings.TrimPrefix(err.Error(), "json: unknown field ")),
				http.StatusBadRequest,
			)
		case errors.Is(err, io.EOF):
			http.Error(w, "Request body must not be empty", http.StatusBadRequest)
		case err.Error() == "http: request body too large":
			http.Error(
				w,
				fmt.Sprintf("Request body must not be larger than %d bytes", MaxRequestBodySize),
				http.StatusRequestEntityTooLarge,
			)
		default:
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}

		return
	}

	// Call decode again to be sure that just a single JSON object is in the request body
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "Request body must only contain a single JSON object", http.StatusBadRequest)
		return
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()

	svc.feeds[feed.Name] = feed.URL

	w.WriteHeader(http.StatusAccepted)
}

func (svc *Service) streamItems(w http.ResponseWriter, r *http.Request) {
	svc.log.Println("GET /items")
	defer svc.log.Println("GET /items DONE")

	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	value := r.Context().Value(DeadlineControllerKey)
	dlController, ok := value.(*DeadlineController)
	if !ok {
		svc.log.Println("deadline controller casting error")
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	svc.mu.Lock()

	urls := make([]string, len(svc.feeds))
	i := 0
	for _, url := range svc.feeds {
		urls[i] = url
		i++
	}

	svc.mu.Unlock()

	// set the headers or event streaming
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	if len(urls) == 0 {
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	enc := json.NewEncoder(w)

	for range ticker.C {
		for _, url := range urls {
			select {
			case <-r.Context().Done():
				return
			default:
				// extend the write timeout of the underlying TCP connection
				if err := dlController.ExtendWriteDeadline(); err != nil {
					svc.log.Printf("deadline error: %v\n", err)
					return
				}

				items, err := svc.fetchItems(r.Context(), dlController, url)
				if errors.Is(err, context.Canceled) {
					return
				}
				if err != nil {
					svc.log.Printf("fetch error: %v\n", err)
					continue
				}

				news := make([]News, len(items))
				for i, item := range items {
					news[i] = News{
						Title:       item.Title,
						Description: item.Description,
						Content:     item.Content,
					}
				}

				if err := enc.Encode(news); err != nil {
					svc.log.Printf("encode error: %v\n", err)
					continue
				}

				f.Flush()
			}
		}
	}
}

func (svc *Service) fetchItems(ctx context.Context, controller *DeadlineController, url string) ([]*gofeed.Item, error) {
	reqCtx, reqCancel := context.WithTimeout(ctx, RSSRequestTimeout)
	defer reqCancel()

	items, err := svc.fetcher.FetchWithContext(reqCtx, url)
	if err != nil {
		return nil, err
	}

	return items, nil
}
