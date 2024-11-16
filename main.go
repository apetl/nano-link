package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type URLService struct {
	db     *sql.DB
	mu     sync.RWMutex
	logger *log.Logger
}

type Config struct {
	Port         string
	DBPath       string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

var (
	ErrURLRequired   = errors.New("url is required")
	ErrInvalidURL    = errors.New("invalid url format")
	ErrURLNotFound   = errors.New("url not found")
	ErrDatabaseError = errors.New("database error")
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func NewConfig() *Config {
	return &Config{
		Port:         getEnv("PORT", "8080"),
		DBPath:       getEnv("DB_PATH", "./urls.db"),
		ReadTimeout:  time.Second * 15,
		WriteTimeout: time.Second * 15,
	}
}

func NewURLService(db *sql.DB, logger *log.Logger) *URLService {
	return &URLService{
		db:     db,
		logger: logger,
	}
}

func (s *URLService) initDB() error {
	dropTableSQL := `DROP TABLE IF EXISTS urls;`
	_, err := s.db.Exec(dropTableSQL)
	if err != nil {
		return err
	}

	createTableSQL := `CREATE TABLE urls (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        long_url TEXT NOT NULL,
        short_url TEXT NOT NULL UNIQUE,
        created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
        access_count INTEGER DEFAULT 0,
        last_accessed DATETIME
    );`
	_, err = s.db.Exec(createTableSQL)
	return err
}

func (s *URLService) shortenURL(longURL string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var existingShortURL string
	err := s.db.QueryRow("SELECT short_url FROM urls WHERE long_url = ?", longURL).Scan(&existingShortURL)
	if err == nil {
		return existingShortURL, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("%w: %v", ErrDatabaseError, err)
	}

	shortURL := generateShortURL(longURL)

	var exists bool
	err = s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM urls WHERE short_url = ?)", shortURL).Scan(&exists)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDatabaseError, err)
	}

	if exists {
		shortURL = shortURL + generateRandomString(2)
	}

	_, err = s.db.Exec(`
        INSERT INTO urls (long_url, short_url, created_at) 
        VALUES (?, ?, datetime('now'))`,
		longURL, shortURL)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDatabaseError, err)
	}

	return shortURL, nil
}

func (s *URLService) getLongURL(shortURL string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDatabaseError, err)
	}
	defer tx.Rollback()

	var longURL string
	err = tx.QueryRow(`SELECT long_url FROM urls WHERE short_url = ?`, shortURL).Scan(&longURL)
	if err == sql.ErrNoRows {
		return "", ErrURLNotFound
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDatabaseError, err)
	}

	_, err = tx.Exec(`
        UPDATE urls 
        SET access_count = access_count + 1,
            last_accessed = datetime('now')
        WHERE short_url = ?`, shortURL)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDatabaseError, err)
	}

	if err = tx.Commit(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrDatabaseError, err)
	}

	return longURL, nil
}

func (s *URLService) deleteURL(identifier string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `DELETE FROM urls WHERE short_url = ? OR 
              short_url = (SELECT short_url FROM urls WHERE long_url LIKE ?)`

	result, err := s.db.Exec(query, identifier, "%"+identifier+"%")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDatabaseError, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDatabaseError, err)
	}
	if affected == 0 {
		return ErrURLNotFound
	}

	return nil
}

func main() {
	logger := log.New(os.Stdout, "url-shortener: ", log.LstdFlags|log.Lshortfile)
	config := NewConfig()

	db, err := sql.Open("sqlite3", config.DBPath)
	if err != nil {
		logger.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	if err = db.Ping(); err != nil {
		logger.Fatalf("failed to connect to database: %v", err)
	}

	_, err = db.Exec("PRAGMA foreign_keys = ON")
	if err != nil {
		logger.Fatalf("failed to enable foreign keys: %v", err)
	}
	_, err = db.Exec("PRAGMA journal_mode = WAL")
	if err != nil {
		logger.Fatalf("failed to enable WAL mode: %v", err)
	}

	service := NewURLService(db, logger)
	if err := service.initDB(); err != nil {
		logger.Fatalf("failed to initialize database: %v", err)
	}

	server := &http.Server{
		Addr:         ":" + config.Port,
		Handler:      setupRoutes(service),
		ReadTimeout:  config.ReadTimeout,
		WriteTimeout: config.WriteTimeout,
	}

	go func() {
		logger.Printf("server starting on port %s", config.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Println("server shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Fatalf("server forced to shutdown: %v", err)
	}

	logger.Println("server exited properly")
}

func setupRoutes(service *URLService) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/shorten", handleShorten(service))
	mux.HandleFunc("/delete/", handleDelete(service))
	mux.HandleFunc("/view/", handleViewLongURL(service)) // Changed this line
	mux.HandleFunc("/", handleRoot(service))

	return logging(recovery(mux))
}

func handleViewLongURL(s *URLService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Extract the short URL from the path
		shortURL := strings.TrimPrefix(r.URL.Path, "/view/")
		shortURL = strings.TrimSpace(shortURL)

		if shortURL == "" {
			http.Error(w, "Short URL is required", http.StatusBadRequest)
			return
		}

		// Remove any URL scheme and host if present
		if strings.Contains(shortURL, "://") {
			u, err := url.Parse(shortURL)
			if err == nil {
				shortURL = strings.TrimPrefix(u.Path, "/")
			}
		}

		longURL, err := s.getLongURL(shortURL)
		if err != nil {
			if errors.Is(err, ErrURLNotFound) {
				http.Error(w, "Short URL not found", http.StatusNotFound)
			} else {
				s.logger.Printf("error retrieving url: %v", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Long URL: %s", longURL)
	}
}

func handleRoot(s *URLService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")

		if path == "" {
			http.ServeFile(w, r, "index.html")
			return
		}

		longURL, err := s.getLongURL(path)
		if err != nil {
			if errors.Is(err, ErrURLNotFound) {
				http.NotFound(w, r)
			} else {
				s.logger.Printf("error retrieving url: %v", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
			return
		}

		http.Redirect(w, r, longURL, http.StatusMovedPermanently)
	}
}

func handleShorten(s *URLService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		err := r.ParseForm()
		if err != nil {
			http.Error(w, "failed to parse form", http.StatusBadRequest)
			return
		}

		longURL := r.FormValue("url")
		if longURL == "" {
			http.Error(w, ErrURLRequired.Error(), http.StatusBadRequest)
			return
		}

		if !isValidURL(longURL) {
			http.Error(w, ErrInvalidURL.Error(), http.StatusBadRequest)
			return
		}

		shortURL, err := s.shortenURL(longURL)
		if err != nil {
			s.logger.Printf("error shortening url: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		fullURL := fmt.Sprintf("http://%s/%s", r.Host, shortURL)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Short URL: %s", fullURL)
	}
}

func handleDelete(s *URLService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		urlToDelete := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/delete/"))
		if urlToDelete == "" {
			http.Error(w, ErrURLRequired.Error(), http.StatusBadRequest)
			return
		}

		if strings.Contains(urlToDelete, "://") {
			u, err := url.Parse(urlToDelete)
			if err == nil {
				urlToDelete = strings.TrimPrefix(u.Path, "/")
			}
		}

		err := s.deleteURL(urlToDelete)
		if err != nil {
			if errors.Is(err, ErrURLNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
			} else {
				s.logger.Printf("error deleting url: %v", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Short URL %s deleted successfully", urlToDelete)
	}
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.RequestURI, time.Since(start))
	})
}

func recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("panic: %v", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func generateShortURL(longURL string) string {
	hash := sha256.Sum256([]byte(longURL))
	return base64.RawURLEncoding.EncodeToString(hash[:6])
}

func generateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = charset[rand.Intn(len(charset))]
	}
	return string(result)
}

func isValidURL(urlStr string) bool {
	u, err := url.Parse(urlStr)
	if err != nil {
		return false
	}
	return u.Scheme != "" && u.Host != ""
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}
