package main

import (
    "context"
    "crypto/sha256"
    "database/sql"
    "encoding/base64"
    "fmt"
    "log"
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

var (
    db   *sql.DB
    mu   sync.Mutex
    port = getEnv("PORT", "8080")
)

func main() {
    // Initialize structured logging
    logger := log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
    log.SetOutput(logger.Writer())

    // Open database connection
    var err error
    db, err = sql.Open("sqlite3", "./urls.db")
    if err != nil {
        log.Fatalf("Failed to open database: %v", err)
    }
    defer db.Close()

    // Create table if it doesn't exist
    createTableSQL := `CREATE TABLE IF NOT EXISTS urls (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        long_url TEXT NOT NULL,
        short_url TEXT NOT NULL UNIQUE
    );`
    _, err = db.Exec(createTableSQL)
    if err != nil {
        log.Fatalf("Failed to create table: %v", err)
    }

    // Create HTTP server
    server := &http.Server{
        Addr:    ":" + port,
        Handler: http.DefaultServeMux,
    }

    // Register handlers
    http.HandleFunc("/shorten", shortenHandler)
    http.HandleFunc("/redirect/", redirectHandler)
    http.HandleFunc("/delete/", deleteHandler)
    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        http.ServeFile(w, r, "index3.html")
    })

    // Graceful shutdown
    go func() {
        if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Fatalf("HTTP server error: %v", err)
        }
    }()
    log.Printf("Server started at :%s", port)

    // Signal handling for graceful shutdown
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
    <-sigChan

    log.Println("Shutting down server...")

    shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    if err := server.Shutdown(shutdownCtx); err != nil {
        log.Fatalf("HTTP shutdown error: %v", err)
    } else {
        log.Println("Graceful shutdown complete")
    }
}

func generateShortURL(longURL string) string {
    hash := sha256.Sum256([]byte(longURL))
    return base64.RawURLEncoding.EncodeToString(hash[:6])
}

func shortenHandler(w http.ResponseWriter, r *http.Request) {
    longURL := r.FormValue("url")
    if longURL == "" {
        http.Error(w, "URL is required", http.StatusBadRequest)
        return
    }

    if !isValidURL(longURL) {
        http.Error(w, "Invalid URL format", http.StatusBadRequest)
        return
    }

    shortURL, err := shortenURL(longURL)
    if err != nil {
        http.Error(w, "Failed to shorten URL", http.StatusInternalServerError)
        return
    }

    // Get the host from the request
    host := r.Host
    fullShortURL := fmt.Sprintf("http://%s/redirect/%s", host, shortURL)
    fmt.Fprintf(w, "Short URL: %s", fullShortURL)
}

func shortenURL(longURL string) (string, error) {
    mu.Lock()
    defer mu.Unlock()

    // Check if the long URL already exists
    var existingShortURL string
    err := db.QueryRow("SELECT short_url FROM urls WHERE long_url = ?", longURL).Scan(&existingShortURL)
    if err != nil && err != sql.ErrNoRows {
        return "", fmt.Errorf("failed to check if long URL exists: %w", err)
    }

    if existingShortURL != "" {
        // Return the existing short URL if the long URL is already shortened
        return existingShortURL, nil
    }

    // Generate a new short URL
    shortURL := generateShortURL(longURL)

    // Check if the generated short URL already exists
    var exists bool
    err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM urls WHERE short_url = ?)", shortURL).Scan(&exists)
    if err != nil {
        return "", fmt.Errorf("failed to check if short URL exists: %w", err)
    }

    if exists {
        // Regenerate short URL if it already exists
        return shortenURL(longURL)
    }

    // Insert new URL mapping
    _, err = db.Exec("INSERT INTO urls (long_url, short_url) VALUES (?, ?)", longURL, shortURL)
    if err != nil {
        return "", fmt.Errorf("failed to insert URL mapping: %w", err)
    }

    return shortURL, nil
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
    shortURL := strings.TrimPrefix(r.URL.Path, "/delete/")
    if shortURL == "" {
        http.Error(w, "Short URL is required", http.StatusBadRequest)
        return
    }

    mu.Lock()
    defer mu.Unlock()

    res, err := db.Exec("DELETE FROM urls WHERE short_url = ?", shortURL)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    rowsAffected, err := res.RowsAffected()
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    if rowsAffected == 0 {
        http.Error(w, "Short URL not found", http.StatusNotFound)
        return
    }

    fmt.Fprintf(w, "Short URL %s deleted successfully", shortURL)
}

func redirectHandler(w http.ResponseWriter, r *http.Request) {
    path := strings.TrimPrefix(r.URL.Path, "/redirect/")
    if path == "" {
        http.Error(w, "Short URL is required", http.StatusBadRequest)
        return
    }

    // Check if the request is for viewing the long URL
    isViewLongURL := strings.HasSuffix(path, "/long")
    if isViewLongURL {
        path = strings.TrimSuffix(path, "/long")
    }

    var longURL string
    err := db.QueryRow("SELECT long_url FROM urls WHERE short_url = ?", path).Scan(&longURL)
    if err != nil {
        if err == sql.ErrNoRows {
            http.Error(w, "Short URL not found", http.StatusNotFound)
        } else {
            http.Error(w, "Failed to retrieve URL", http.StatusInternalServerError)
        }
        return
    }

    if isViewLongURL {
        fmt.Fprintf(w, "Long URL: %s", longURL)
    } else {
        http.Redirect(w, r, longURL, http.StatusMovedPermanently)
    }
}

func isValidURL(urlStr string) bool {
    _, err := url.ParseRequestURI(urlStr)
    return err == nil
}

func getEnv(key, fallback string) string {
    if value, exists := os.LookupEnv(key); exists {
        return value
    }
    return fallback
}
