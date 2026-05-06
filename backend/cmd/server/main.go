package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/html"
)

const maxUploadBytes = 18 << 20
const maxRemoteImageBytes = 12 << 20
const maxPreviewHTMLBytes = 3 << 20
const maxMarketplaceJSONBytes = 8 << 20
const previewUserAgent = "Mozilla/5.0 (compatible; VislistPreview/1.0; +http://localhost)"

var (
	wildberriesCatalogRE = regexp.MustCompile(`(?i)/(?:catalog|product)/(\d{5,})`)
	trailingDigitsRE     = regexp.MustCompile(`(\d{5,})(?:[/?#]?$|[/?#])`)
	titleTrailingIDRE    = regexp.MustCompile(`[-_\s]*\d{5,}$`)
	remoteImageURLRE     = regexp.MustCompile(`https?:\\?/\\?/(?:[^"'\s\\<>]+)\\?/(?:[^"'\s\\<>]+)\.(?:jpg|jpeg|png|webp|gif)(?:\?[^"'\s\\<>]*)?`)
)

type app struct {
	db            *sql.DB
	adminUsername string
	adminPassword string
	uploadDir     string
	webDir        string
}

type giftView struct {
	ID           int64     `json:"id"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	LinkURL      string    `json:"linkUrl"`
	ImageURL     string    `json:"imageUrl"`
	Reserved     bool      `json:"reserved"`
	ReservedByMe bool      `json:"reservedByMe"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type sessionView struct {
	Token    string `json:"token"`
	Role     string `json:"role"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

type productPreview struct {
	ImageURL string `json:"imageUrl"`
	Title    string `json:"title"`
}

func main() {
	databaseURL := getenv("DATABASE_URL", "postgres://vislist:vislist@localhost:5433/vislist?sslmode=disable")
	port := getenv("PORT", "8080")

	db, err := openDB(databaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer db.Close()

	a := &app{
		db:            db,
		adminUsername: getenv("ADMIN_USERNAME", "admin"),
		adminPassword: getenv("ADMIN_PASSWORD", "2026"),
		uploadDir:     getenv("UPLOAD_DIR", "uploads"),
		webDir:        getenv("WEB_DIR", "web"),
	}

	if err := os.MkdirAll(a.uploadDir, 0o755); err != nil {
		log.Fatalf("uploads: %v", err)
	}
	if err := a.ensureSchema(context.Background()); err != nil {
		log.Fatalf("schema: %v", err)
	}
	if err := a.ensureDefaultAdmin(context.Background()); err != nil {
		log.Fatalf("admin account: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", a.handleHealth)
	mux.HandleFunc("/api/auth/register", a.handleGuestRegister)
	mux.HandleFunc("/api/auth/login", a.handleGuestLogin)
	mux.HandleFunc("/api/gifts", a.handleGifts)
	mux.HandleFunc("/api/gifts/", a.handleGiftAction)
	mux.HandleFunc("/api/profile/reservations", a.handleProfileReservations)
	mux.HandleFunc("/api/admin/session", a.handleAdminSession)
	mux.HandleFunc("/api/admin/link-preview", a.handleAdminLinkPreview)
	mux.HandleFunc("/api/admin/gifts", a.handleAdminGifts)
	mux.HandleFunc("/api/admin/gifts/", a.handleAdminGiftByID)
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(a.uploadDir))))
	mux.HandleFunc("/", a.serveSPA)

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           withCORS(requestLogger(mux)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("vislist is listening on :%s", port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func openDB(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	var lastErr error
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		lastErr = db.PingContext(ctx)
		cancel()
		if lastErr == nil {
			return db, nil
		}
		time.Sleep(time.Second)
	}
	_ = db.Close()
	return nil, lastErr
}

func (a *app) ensureSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS guests (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			username TEXT,
			password_hash TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`ALTER TABLE guests ADD COLUMN IF NOT EXISTS username TEXT`,
		`ALTER TABLE guests ADD COLUMN IF NOT EXISTS password_hash TEXT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS guests_username_lower_unique
			ON guests (lower(username)) WHERE username IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS admins (
			id BIGSERIAL PRIMARY KEY,
			username TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS admins_username_lower_unique ON admins (lower(username))`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			role TEXT NOT NULL CHECK (role IN ('guest', 'admin')),
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			expires_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS sessions_user_role_idx ON sessions (user_id, role)`,
		`CREATE TABLE IF NOT EXISTS gifts (
			id BIGSERIAL PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			link_url TEXT NOT NULL DEFAULT '',
			image_url TEXT NOT NULL DEFAULT '',
			reserved_by_guest_id TEXT REFERENCES guests(id) ON DELETE SET NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS gifts_reserved_by_guest_id_idx ON gifts(reserved_by_guest_id)`,
	}
	for _, statement := range statements {
		if _, err := a.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) ensureDefaultAdmin(ctx context.Context) error {
	username := normalizeUsername(a.adminUsername)
	if username == "" {
		return fmt.Errorf("empty admin username")
	}

	var exists bool
	if err := a.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM admins WHERE lower(username) = lower($1))`, username).Scan(&exists); err != nil {
		return err
	}
	hash, err := hashPassword(a.adminPassword)
	if err != nil {
		return err
	}
	if exists {
		_, err = a.db.ExecContext(ctx, `UPDATE admins SET password_hash = $2 WHERE lower(username) = lower($1)`, username, hash)
		return err
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO admins (username, password_hash) VALUES ($1, $2)`, username, hash)
	return err
}

func (a *app) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *app) handleGuestRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var payload struct {
		Name     string `json:"name"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "Не получилось прочитать форму")
		return
	}

	name := strings.TrimSpace(payload.Name)
	username := normalizeUsername(payload.Username)
	password := strings.TrimSpace(payload.Password)
	if name == "" {
		writeError(w, http.StatusBadRequest, "Нужно имя")
		return
	}
	if len([]rune(name)) > 80 {
		writeError(w, http.StatusBadRequest, "Имя слишком длинное")
		return
	}
	if err := validateUsername(username); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validatePassword(password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if a.guestUsernameExists(r.Context(), username) {
		writeError(w, http.StatusConflict, "Такой логин уже занят")
		return
	}

	id, err := randomToken(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось создать аккаунт")
		return
	}
	hash, err := hashPassword(password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось защитить пароль")
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `
		INSERT INTO guests (id, name, username, password_hash)
		VALUES ($1, $2, $3, $4)
	`, id, name, username, hash); err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось сохранить аккаунт")
		return
	}

	token, err := a.createSession(r.Context(), id, "guest")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Аккаунт создан, но вход не сработал")
		return
	}
	writeJSON(w, http.StatusCreated, sessionView{Token: token, Role: "guest", Username: username, Name: name})
}

func (a *app) handleGuestLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "Не получилось прочитать форму")
		return
	}

	username := normalizeUsername(payload.Username)
	password := strings.TrimSpace(payload.Password)
	var id, name, passwordHash string
	err := a.db.QueryRowContext(r.Context(), `
		SELECT id, name, COALESCE(password_hash, '')
		FROM guests
		WHERE lower(username) = lower($1)
	`, username).Scan(&id, &name, &passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "Неверный логин или пароль")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось войти")
		return
	}
	if passwordHash == "" || !passwordMatches(passwordHash, password) {
		writeError(w, http.StatusUnauthorized, "Неверный логин или пароль")
		return
	}

	token, err := a.createSession(r.Context(), id, "guest")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось создать сессию")
		return
	}
	writeJSON(w, http.StatusOK, sessionView{Token: token, Role: "guest", Username: username, Name: name})
}

func (a *app) handleAdminSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "Не получилось прочитать форму")
		return
	}

	username := normalizeUsername(payload.Username)
	password := strings.TrimSpace(payload.Password)
	var id int64
	var storedUsername, passwordHash string
	err := a.db.QueryRowContext(r.Context(), `
		SELECT id, username, password_hash
		FROM admins
		WHERE lower(username) = lower($1)
	`, username).Scan(&id, &storedUsername, &passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "Неверный логин или пароль")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось войти")
		return
	}
	if !passwordMatches(passwordHash, password) {
		writeError(w, http.StatusUnauthorized, "Неверный логин или пароль")
		return
	}

	token, err := a.createSession(r.Context(), strconv.FormatInt(id, 10), "admin")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось создать сессию")
		return
	}
	writeJSON(w, http.StatusOK, sessionView{Token: token, Role: "admin", Username: storedUsername, Name: "Именинник"})
}

func (a *app) handleAdminLinkPreview(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var payload struct {
		LinkURL string `json:"linkUrl"`
	}
	if err := readJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "Не получилось прочитать ссылку")
		return
	}
	linkURL := normalizeLink(payload.LinkURL)
	if linkURL == "" {
		writeError(w, http.StatusBadRequest, "Нужна ссылка на товар")
		return
	}

	preview, err := a.fetchProductPreview(r.Context(), linkURL)
	if err != nil {
		if pageURL, parseErr := url.Parse(linkURL); parseErr == nil {
			if title := titleFromURL(pageURL); title != "" {
				writeJSON(w, http.StatusOK, map[string]string{
					"linkUrl":  linkURL,
					"imageUrl": "",
					"title":    title,
				})
				return
			}
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"linkUrl":  linkURL,
		"imageUrl": preview.ImageURL,
		"title":    preview.Title,
	})
}

func (a *app) handleGifts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	viewerID, ok := a.optionalGuestID(w, r)
	if !ok {
		return
	}
	gifts, err := a.listGifts(r.Context(), viewerID, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось загрузить подарки")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gifts": gifts})
}

func (a *app) handleGiftAction(w http.ResponseWriter, r *http.Request) {
	id, action, ok := parseGiftActionPath(r.URL.Path, "/api/gifts/")
	if !ok || action != "reserve" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
		a.reserveGift(w, r, id)
	case http.MethodDelete:
		a.releaseGift(w, r, id)
	default:
		methodNotAllowed(w)
	}
}

func (a *app) reserveGift(w http.ResponseWriter, r *http.Request, giftID int64) {
	userID, ok := a.requireGuestID(w, r)
	if !ok {
		return
	}

	result, err := a.db.ExecContext(r.Context(), `
		UPDATE gifts
		SET reserved_by_guest_id = $1, updated_at = now()
		WHERE id = $2 AND (reserved_by_guest_id IS NULL OR reserved_by_guest_id = $1)
	`, userID, giftID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось забронировать подарок")
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		if !a.giftExists(r.Context(), giftID) {
			writeError(w, http.StatusNotFound, "Подарок не найден")
			return
		}
		writeError(w, http.StatusConflict, "Этот подарок уже занят")
		return
	}

	gift, err := a.getGift(r.Context(), giftID, userID, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Бронь поставлена, но подарок не загрузился")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gift": gift})
}

func (a *app) releaseGift(w http.ResponseWriter, r *http.Request, giftID int64) {
	userID, ok := a.requireGuestID(w, r)
	if !ok {
		return
	}

	result, err := a.db.ExecContext(r.Context(), `
		UPDATE gifts
		SET reserved_by_guest_id = NULL, updated_at = now()
		WHERE id = $1 AND reserved_by_guest_id = $2
	`, giftID, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось снять бронь")
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		writeError(w, http.StatusConflict, "Бронь уже снята или принадлежит другому гостю")
		return
	}

	gift, err := a.getGift(r.Context(), giftID, userID, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Бронь снята, но подарок не загрузился")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gift": gift})
}

func (a *app) handleProfileReservations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	userID, ok := a.requireGuestID(w, r)
	if !ok {
		return
	}

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, title, description, link_url, image_url,
		       reserved_by_guest_id IS NOT NULL AS reserved,
		       true AS reserved_by_me,
		       created_at, updated_at
		FROM gifts
		WHERE reserved_by_guest_id = $1
		ORDER BY updated_at DESC, id DESC
	`, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось загрузить профиль")
		return
	}
	defer rows.Close()

	gifts, err := scanGifts(rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось прочитать профиль")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gifts": gifts})
}

func (a *app) handleAdminGifts(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		gifts, err := a.listGifts(r.Context(), "", true)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Не получилось загрузить подарки")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"gifts": gifts})
	case http.MethodPost:
		a.createGift(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (a *app) createGift(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeError(w, http.StatusBadRequest, "Форма слишком большая или повреждена")
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		writeError(w, http.StatusBadRequest, "Нужно название подарка")
		return
	}
	description := strings.TrimSpace(r.FormValue("description"))
	linkURL := normalizeLink(r.FormValue("linkUrl"))

	imageURL, _, err := a.saveUploadedImage(r, "image")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	previewImageURL := cleanPreviewImageURL(r.FormValue("previewImageUrl"))
	if imageURL == "" {
		imageURL = previewImageURL
	} else if previewImageURL != "" && previewImageURL != imageURL {
		a.removeUploadedFile(previewImageURL)
	}

	var id int64
	if err := a.db.QueryRowContext(r.Context(), `
		INSERT INTO gifts (title, description, link_url, image_url)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, title, description, linkURL, imageURL).Scan(&id); err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось добавить подарок")
		return
	}
	gift, err := a.getGift(r.Context(), id, "", true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Подарок добавлен, но не загрузился")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"gift": gift})
}

func (a *app) handleAdminGiftByID(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id, ok := parseIDPath(r.URL.Path, "/api/admin/gifts/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		a.updateGift(w, r, id)
	case http.MethodDelete:
		a.deleteGift(w, r, id)
	default:
		methodNotAllowed(w)
	}
}

func (a *app) updateGift(w http.ResponseWriter, r *http.Request, id int64) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeError(w, http.StatusBadRequest, "Форма слишком большая или повреждена")
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		writeError(w, http.StatusBadRequest, "Нужно название подарка")
		return
	}
	description := strings.TrimSpace(r.FormValue("description"))
	linkURL := normalizeLink(r.FormValue("linkUrl"))

	currentImage, err := a.currentImageURL(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Подарок не найден")
			return
		}
		writeError(w, http.StatusInternalServerError, "Не получилось прочитать подарок")
		return
	}

	imageURL, hasImage, err := a.saveUploadedImage(r, "image")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !hasImage {
		previewImageURL := cleanPreviewImageURL(r.FormValue("previewImageUrl"))
		if previewImageURL != "" && previewImageURL != currentImage {
			imageURL = previewImageURL
			hasImage = true
		} else {
			imageURL = currentImage
		}
	}

	result, err := a.db.ExecContext(r.Context(), `
		UPDATE gifts
		SET title = $1, description = $2, link_url = $3, image_url = $4, updated_at = now()
		WHERE id = $5
	`, title, description, linkURL, imageURL, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось обновить подарок")
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		writeError(w, http.StatusNotFound, "Подарок не найден")
		return
	}
	if hasImage && imageURL != currentImage {
		a.removeUploadedFile(currentImage)
	}

	gift, err := a.getGift(r.Context(), id, "", true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Подарок обновлён, но не загрузился")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gift": gift})
}

func (a *app) deleteGift(w http.ResponseWriter, r *http.Request, id int64) {
	imageURL, err := a.currentImageURL(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Подарок не найден")
			return
		}
		writeError(w, http.StatusInternalServerError, "Не получилось прочитать подарок")
		return
	}
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM gifts WHERE id = $1`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось удалить подарок")
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		writeError(w, http.StatusNotFound, "Подарок не найден")
		return
	}
	a.removeUploadedFile(imageURL)
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) listGifts(ctx context.Context, viewerID string, admin bool) ([]giftView, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT id, title, description, link_url, image_url,
		       reserved_by_guest_id IS NOT NULL AS reserved,
		       COALESCE(reserved_by_guest_id = $1, false) AS reserved_by_me,
		       created_at, updated_at
		FROM gifts
		ORDER BY created_at DESC, id DESC
	`, viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	gifts, err := scanGifts(rows)
	if err != nil {
		return nil, err
	}
	if admin {
		for i := range gifts {
			gifts[i].ReservedByMe = false
		}
	}
	return gifts, nil
}

func (a *app) getGift(ctx context.Context, id int64, viewerID string, admin bool) (giftView, error) {
	var gift giftView
	err := a.db.QueryRowContext(ctx, `
		SELECT id, title, description, link_url, image_url,
		       reserved_by_guest_id IS NOT NULL AS reserved,
		       COALESCE(reserved_by_guest_id = $1, false) AS reserved_by_me,
		       created_at, updated_at
		FROM gifts
		WHERE id = $2
	`, viewerID, id).Scan(
		&gift.ID,
		&gift.Title,
		&gift.Description,
		&gift.LinkURL,
		&gift.ImageURL,
		&gift.Reserved,
		&gift.ReservedByMe,
		&gift.CreatedAt,
		&gift.UpdatedAt,
	)
	if admin {
		gift.ReservedByMe = false
	}
	return gift, err
}

func scanGifts(rows *sql.Rows) ([]giftView, error) {
	gifts := make([]giftView, 0)
	for rows.Next() {
		var gift giftView
		if err := rows.Scan(
			&gift.ID,
			&gift.Title,
			&gift.Description,
			&gift.LinkURL,
			&gift.ImageURL,
			&gift.Reserved,
			&gift.ReservedByMe,
			&gift.CreatedAt,
			&gift.UpdatedAt,
		); err != nil {
			return nil, err
		}
		gifts = append(gifts, gift)
	}
	return gifts, rows.Err()
}

func (a *app) createSession(ctx context.Context, userID, role string) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	_, err = a.db.ExecContext(ctx, `
		INSERT INTO sessions (token, user_id, role, expires_at)
		VALUES ($1, $2, $3, now() + interval '365 days')
	`, token, userID, role)
	return token, err
}

func (a *app) optionalGuestID(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := strings.TrimSpace(r.Header.Get("X-Guest-Token"))
	if token == "" {
		return "", true
	}
	id, ok := a.userIDForSession(w, r, token, "guest")
	return id, ok
}

func (a *app) requireGuestID(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := strings.TrimSpace(r.Header.Get("X-Guest-Token"))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "Нужно войти как гость")
		return "", false
	}
	return a.userIDForSession(w, r, token, "guest")
}

func (a *app) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	token := strings.TrimSpace(r.Header.Get("X-Admin-Token"))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "Нужно войти как именинник")
		return false
	}
	_, ok := a.userIDForSession(w, r, token, "admin")
	return ok
}

func (a *app) userIDForSession(w http.ResponseWriter, r *http.Request, token, role string) (string, bool) {
	var userID string
	err := a.db.QueryRowContext(r.Context(), `
		SELECT user_id
		FROM sessions
		WHERE token = $1 AND role = $2 AND expires_at > now()
	`, token, role).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "Сессия устарела, войди заново")
		return "", false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не получилось проверить сессию")
		return "", false
	}
	return userID, true
}

func (a *app) guestUsernameExists(ctx context.Context, username string) bool {
	var exists bool
	err := a.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM guests WHERE lower(username) = lower($1))`, username).Scan(&exists)
	return err == nil && exists
}

func (a *app) giftExists(ctx context.Context, id int64) bool {
	var exists bool
	err := a.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM gifts WHERE id = $1)`, id).Scan(&exists)
	return err == nil && exists
}

func (a *app) currentImageURL(ctx context.Context, id int64) (string, error) {
	var imageURL string
	err := a.db.QueryRowContext(ctx, `SELECT image_url FROM gifts WHERE id = $1`, id).Scan(&imageURL)
	return imageURL, err
}

func (a *app) saveUploadedImage(r *http.Request, field string) (string, bool, error) {
	file, header, err := r.FormFile(field)
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("Не получилось прочитать фото")
	}
	defer file.Close()

	ext, err := allowedImageExt(file, header)
	if err != nil {
		return "", false, err
	}
	nameToken, err := randomToken(18)
	if err != nil {
		return "", false, fmt.Errorf("Не получилось сохранить фото")
	}
	filename := nameToken + ext
	dstPath := filepath.Join(a.uploadDir, filename)

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", false, fmt.Errorf("Не получилось обработать фото")
	}
	dst, err := os.Create(dstPath)
	if err != nil {
		return "", false, fmt.Errorf("Не получилось сохранить фото")
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		return "", false, fmt.Errorf("Не получилось записать фото")
	}
	return "/uploads/" + url.PathEscape(filename), true, nil
}

func (a *app) fetchProductPreview(ctx context.Context, linkURL string) (productPreview, error) {
	pageURL, err := url.Parse(linkURL)
	if err != nil || pageURL.Host == "" || (pageURL.Scheme != "http" && pageURL.Scheme != "https") {
		return productPreview{}, fmt.Errorf("Ссылка должна быть обычным http или https адресом")
	}

	client := &http.Client{Timeout: 9 * time.Second}
	if isDirectImageURL(pageURL) {
		imageURL, err := a.downloadRemoteImage(ctx, client, pageURL.String(), "")
		if err != nil {
			return productPreview{}, err
		}
		return productPreview{ImageURL: imageURL, Title: titleFromURL(pageURL)}, nil
	}
	if isWildberriesHost(pageURL.Host) {
		if preview, err := a.fetchWildberriesPreview(ctx, client, pageURL); err == nil {
			return preview, nil
		} else {
			return productPreview{}, err
		}
	}
	if candidates := marketplacePreviewCandidates(pageURL); len(candidates) > 0 {
		if imageURL, err := a.downloadFirstRemoteImage(ctx, client, candidates, pageURL.String()); err == nil {
			return productPreview{ImageURL: imageURL, Title: a.fetchMarketplaceTitle(ctx, client, pageURL)}, nil
		}
	}
	if isOzonHost(pageURL.Host) {
		if preview, err := a.fetchOzonPreview(ctx, client, pageURL); err == nil {
			return preview, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL.String(), nil)
	if err != nil {
		return productPreview{}, fmt.Errorf("Не получилось открыть ссылку")
	}
	req.Header.Set("User-Agent", previewUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		return productPreview{}, fmt.Errorf("Не получилось загрузить страницу товара")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return productPreview{}, fmt.Errorf("Страница товара не ответила нормально")
	}

	doc, err := html.Parse(io.LimitReader(resp.Body, maxPreviewHTMLBytes))
	if err != nil {
		return productPreview{}, fmt.Errorf("Не получилось разобрать страницу товара")
	}
	previewURL := extractPreviewImageURL(doc, resp.Request.URL)
	if previewURL == "" {
		return productPreview{}, fmt.Errorf("Не нашёл фото товара на этой странице")
	}

	imageURL, err := a.downloadRemoteImage(ctx, client, previewURL, resp.Request.URL.String())
	if err != nil {
		return productPreview{}, err
	}
	title := extractPreviewTitle(doc)
	if title == "" {
		title = titleFromURL(pageURL)
	}
	return productPreview{ImageURL: imageURL, Title: title}, nil
}

func marketplacePreviewCandidates(pageURL *url.URL) []string {
	if isWildberriesHost(pageURL.Host) {
		return wildberriesPreviewCandidates(pageURL)
	}
	return nil
}

func (a *app) fetchMarketplaceTitle(ctx context.Context, client *http.Client, pageURL *url.URL) string {
	if isWildberriesHost(pageURL.Host) {
		if title := a.fetchWildberriesTitle(ctx, client, pageURL); title != "" {
			return title
		}
	}
	return titleFromURL(pageURL)
}

func isDirectImageURL(pageURL *url.URL) bool {
	if pageURL.Host == "" || (pageURL.Scheme != "http" && pageURL.Scheme != "https") {
		return false
	}
	ext := strings.ToLower(path.Ext(pageURL.Path))
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" || ext == ".gif"
}

func isWildberriesHost(host string) bool {
	host = strings.ToLower(host)
	return host == "wb.ru" ||
		host == "wb.by" ||
		host == "wildberries.ru" ||
		host == "wildberries.by" ||
		strings.HasSuffix(host, ".wb.ru") ||
		strings.HasSuffix(host, ".wb.by") ||
		strings.HasSuffix(host, ".wildberries.ru") ||
		strings.HasSuffix(host, ".wildberries.by")
}

func isOzonHost(host string) bool {
	host = strings.ToLower(host)
	return host == "ozon.ru" ||
		host == "ozon.kz" ||
		host == "ozon.by" ||
		strings.HasSuffix(host, ".ozon.ru") ||
		strings.HasSuffix(host, ".ozon.kz") ||
		strings.HasSuffix(host, ".ozon.by")
}

func wildberriesPreviewCandidates(pageURL *url.URL) []string {
	nmID := wildberriesProductID(pageURL)
	if nmID <= 0 {
		return nil
	}
	vol := nmID / 100000
	part := nmID / 1000
	pathPart := fmt.Sprintf("/vol%d/part%d/%d/images/big/1.webp", vol, part, nmID)
	host := wildberriesBasketHost(wildberriesBasketByVol(vol))
	if host == "" {
		return nil
	}
	return []string{"https://" + host + pathPart}
}

func (a *app) fetchWildberriesPreview(ctx context.Context, client *http.Client, pageURL *url.URL) (productPreview, error) {
	nmID := wildberriesProductID(pageURL)
	if nmID <= 0 {
		return productPreview{}, fmt.Errorf("Не получилось найти ID товара WB")
	}
	vol := nmID / 100000
	part := nmID / 1000
	imagePath := fmt.Sprintf("/vol%d/part%d/%d/images/big/1.webp", vol, part, nmID)
	infoPath := fmt.Sprintf("/vol%d/part%d/%d/info/ru/card.json", vol, part, nmID)

	var lastErr error
	for _, hostNumber := range wildberriesBasketCandidateNumbers(vol) {
		host := wildberriesBasketHost(hostNumber)
		if host == "" {
			continue
		}
		title := a.fetchJSONTitle(ctx, client, "https://"+host+infoPath, pageURL.String())
		if title == "" {
			continue
		}
		imageURL, err := a.downloadRemoteImage(ctx, client, "https://"+host+imagePath, pageURL.String())
		if err == nil {
			return productPreview{ImageURL: imageURL, Title: title}, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return productPreview{}, lastErr
	}
	return productPreview{}, fmt.Errorf("Не нашёл фото товара на WB")
}

func wildberriesProductID(pageURL *url.URL) int64 {
	raw := pageURL.EscapedPath()
	if match := wildberriesCatalogRE.FindStringSubmatch(raw); len(match) == 2 {
		id, _ := strconv.ParseInt(match[1], 10, 64)
		return id
	}
	for _, key := range []string{"nm", "nmId", "nm_id", "product_id"} {
		if value := pageURL.Query().Get(key); value != "" {
			id, _ := strconv.ParseInt(value, 10, 64)
			if id > 0 {
				return id
			}
		}
	}
	if match := trailingDigitsRE.FindStringSubmatch(pageURL.String()); len(match) == 2 {
		id, _ := strconv.ParseInt(match[1], 10, 64)
		return id
	}
	return 0
}

func (a *app) fetchWildberriesTitle(ctx context.Context, client *http.Client, pageURL *url.URL) string {
	candidates := wildberriesInfoCandidates(pageURL)
	if apiURL := wildberriesCardURL(pageURL); apiURL != "" {
		candidates = append(candidates, apiURL)
	}
	for _, apiURL := range uniqueStrings(candidates) {
		if title := a.fetchJSONTitle(ctx, client, apiURL, pageURL.String()); title != "" {
			return title
		}
	}
	return ""
}

func (a *app) fetchJSONTitle(ctx context.Context, client *http.Client, apiURL, referer string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", previewUserAgent)
	req.Header.Set("Accept", "application/json,text/plain,*/*")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxMarketplaceJSONBytes))
	_ = resp.Body.Close()
	if readErr != nil || resp.StatusCode < 200 || resp.StatusCode > 299 || len(raw) == 0 {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ""
	}
	return titleFromJSON(decoded)
}

func wildberriesInfoCandidates(pageURL *url.URL) []string {
	nmID := wildberriesProductID(pageURL)
	if nmID <= 0 {
		return nil
	}
	vol := nmID / 100000
	part := nmID / 1000
	pathPart := fmt.Sprintf("/vol%d/part%d/%d/info/ru/card.json", vol, part, nmID)
	candidates := make([]string, 0, 32)
	addCandidate := func(hostNumber int64) {
		if hostNumber <= 0 {
			return
		}
		host := wildberriesBasketHost(hostNumber)
		if host == "" {
			return
		}
		candidates = append(candidates, "https://"+host+pathPart)
	}

	for _, hostNumber := range wildberriesBasketCandidateNumbers(vol) {
		addCandidate(hostNumber)
	}
	return uniqueStrings(candidates)
}

func wildberriesBasketCandidateNumbers(vol int64) []int64 {
	candidates := make([]int64, 0, 31)
	addCandidate := func(hostNumber int64) {
		if hostNumber <= 0 {
			return
		}
		for _, existing := range candidates {
			if existing == hostNumber {
				return
			}
		}
		candidates = append(candidates, hostNumber)
	}

	addCandidate(wildberriesBasketByVol(vol))
	for hostNumber := int64(1); hostNumber <= 80; hostNumber++ {
		addCandidate(hostNumber)
	}
	return candidates
}

func wildberriesBasketHost(hostNumber int64) string {
	if hostNumber <= 0 {
		return ""
	}
	if hostNumber >= 10 {
		return fmt.Sprintf("basket-%d.wbbasket.ru", hostNumber)
	}
	return fmt.Sprintf("basket-%02d.wbbasket.ru", hostNumber)
}

func wildberriesCardURL(pageURL *url.URL) string {
	nmID := wildberriesProductID(pageURL)
	if nmID <= 0 {
		return ""
	}
	query := url.Values{}
	query.Set("appType", "1")
	query.Set("curr", "rub")
	query.Set("dest", "-1257786")
	query.Set("spp", "30")
	query.Set("nm", strconv.FormatInt(nmID, 10))
	return "https://card.wb.ru/cards/v2/detail?" + query.Encode()
}

func wildberriesBasketByVol(vol int64) int64 {
	switch {
	case vol <= 143:
		return 1
	case vol <= 287:
		return 2
	case vol <= 431:
		return 3
	case vol <= 719:
		return 4
	case vol <= 1007:
		return 5
	case vol <= 1061:
		return 6
	case vol <= 1115:
		return 7
	case vol <= 1169:
		return 8
	case vol <= 1313:
		return 9
	case vol <= 1601:
		return 10
	case vol <= 1655:
		return 11
	case vol <= 1919:
		return 12
	case vol <= 2045:
		return 13
	case vol <= 2189:
		return 14
	case vol <= 2405:
		return 15
	case vol <= 2621:
		return 16
	case vol <= 2837:
		return 17
	case vol <= 3053:
		return 18
	case vol <= 9535:
		return 40
	default:
		return 40
	}
}

func (a *app) fetchOzonPreview(ctx context.Context, client *http.Client, pageURL *url.URL) (productPreview, error) {
	apiURL := ozonComposerURL(pageURL)
	if apiURL == "" {
		return productPreview{}, fmt.Errorf("Не получилось собрать запрос к Ozon")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return productPreview{}, fmt.Errorf("Не получилось открыть Ozon")
	}
	req.Header.Set("User-Agent", previewUserAgent)
	req.Header.Set("Accept", "application/json,text/plain,*/*")
	req.Header.Set("Referer", pageURL.String())

	resp, err := client.Do(req)
	if err != nil {
		return productPreview{}, fmt.Errorf("Ozon не отдал данные товара")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return productPreview{}, fmt.Errorf("Ozon не отдал данные товара")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMarketplaceJSONBytes))
	if err != nil || len(raw) == 0 {
		return productPreview{}, fmt.Errorf("Не получилось прочитать данные Ozon")
	}

	candidates := extractRemoteImageURLs(string(raw), isOzonImageURL)
	title := ""
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err == nil {
		candidates = append(candidates, imageURLsFromJSON(decoded, isOzonImageURL)...)
		title = titleFromJSON(decoded)
	}
	candidates = uniqueStrings(candidates)
	if len(candidates) == 0 {
		return productPreview{}, fmt.Errorf("Не нашёл фото товара на Ozon")
	}
	imageURL, err := a.downloadFirstRemoteImage(ctx, client, candidates, pageURL.String())
	if err != nil {
		return productPreview{}, err
	}
	if title == "" {
		title = titleFromURL(pageURL)
	}
	return productPreview{ImageURL: imageURL, Title: title}, nil
}

func ozonComposerURL(pageURL *url.URL) string {
	pathWithQuery := pageURL.EscapedPath()
	if pathWithQuery == "" {
		return ""
	}
	if pageURL.RawQuery != "" {
		pathWithQuery += "?" + pageURL.RawQuery
	}
	query := url.Values{}
	query.Set("url", pathWithQuery)
	return "https://" + ozonAPIHost(pageURL.Host) + "/api/composer-api.bx/page/json/v2?" + query.Encode()
}

func ozonAPIHost(host string) string {
	host = strings.ToLower(host)
	switch {
	case host == "ozon.kz" || strings.HasSuffix(host, ".ozon.kz"):
		return "www.ozon.kz"
	case host == "ozon.by" || strings.HasSuffix(host, ".ozon.by"):
		return "www.ozon.by"
	default:
		return "www.ozon.ru"
	}
}

func extractPreviewImageURL(doc *html.Node, pageURL *url.URL) string {
	candidates := make([]string, 0, 4)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "meta":
				key := firstAttr(n, "property", "name", "itemprop")
				if isPreviewImageKey(key) {
					candidates = append(candidates, attrValue(n, "content"))
				}
			case "link":
				rel := strings.ToLower(attrValue(n, "rel"))
				if strings.Contains(rel, "image_src") {
					candidates = append(candidates, attrValue(n, "href"))
				}
			case "script":
				if strings.Contains(strings.ToLower(attrValue(n, "type")), "ld+json") {
					var decoded any
					if err := json.Unmarshal([]byte(textContent(n)), &decoded); err == nil {
						candidates = append(candidates, imageURLsFromJSON(decoded, func(string) bool { return true })...)
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	for _, candidate := range candidates {
		if resolved := resolvePreviewURL(candidate, pageURL); resolved != "" {
			return resolved
		}
	}
	return ""
}

func extractPreviewTitle(doc *html.Node) string {
	candidates := make([]string, 0, 4)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "meta":
				key := firstAttr(n, "property", "name", "itemprop")
				if isPreviewTitleKey(key) {
					candidates = append(candidates, attrValue(n, "content"))
				}
			case "title":
				candidates = append(candidates, textContent(n))
			case "script":
				if strings.Contains(strings.ToLower(attrValue(n, "type")), "ld+json") {
					var decoded any
					if err := json.Unmarshal([]byte(textContent(n)), &decoded); err == nil {
						candidates = append(candidates, titleFromJSON(decoded))
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	for _, candidate := range candidates {
		if title := cleanPreviewTitle(candidate); title != "" {
			return title
		}
	}
	return ""
}

func textContent(n *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			builder.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return builder.String()
}

func imageURLsFromJSON(value any, allow func(string) bool) []string {
	candidates := make([]string, 0, 4)
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, nested := range typed {
				if isJSONImageKey(key) {
					candidates = append(candidates, imageURLsFromJSONValue(nested, allow)...)
					continue
				}
				walk(nested)
			}
		case []any:
			for _, nested := range typed {
				walk(nested)
			}
		case string:
			candidates = append(candidates, extractRemoteImageURLs(typed, allow)...)
			if strings.Contains(typed, "{") || strings.Contains(typed, "[") {
				var nested any
				if err := json.Unmarshal([]byte(typed), &nested); err == nil {
					walk(nested)
				}
			}
		}
	}
	walk(value)
	return uniqueStrings(candidates)
}

func imageURLsFromJSONValue(value any, allow func(string) bool) []string {
	switch typed := value.(type) {
	case string:
		if resolved := normalizeRemoteImageCandidate(typed); resolved != "" && allow(resolved) {
			return []string{resolved}
		}
		return extractRemoteImageURLs(typed, allow)
	case []any:
		candidates := make([]string, 0, len(typed))
		for _, item := range typed {
			candidates = append(candidates, imageURLsFromJSONValue(item, allow)...)
		}
		return candidates
	default:
		return imageURLsFromJSON(typed, allow)
	}
}

func titleFromJSON(value any) string {
	preferredKeys := []string{
		"productname",
		"product_name",
		"goodsname",
		"goods_name",
		"imt_name",
		"itemname",
		"item_name",
		"name",
		"title",
	}
	for _, key := range preferredKeys {
		if title := findTitleForJSONKey(value, key); title != "" {
			return title
		}
	}
	return ""
}

func findTitleForJSONKey(value any, key string) string {
	switch typed := value.(type) {
	case map[string]any:
		for currentKey, nested := range typed {
			if strings.ToLower(currentKey) == key {
				switch value := nested.(type) {
				case string:
					if title := cleanPreviewTitle(value); title != "" {
						return title
					}
				case []any:
					for _, item := range value {
						if text, ok := item.(string); ok {
							if title := cleanPreviewTitle(text); title != "" {
								return title
							}
						}
					}
				}
			}
		}
		for _, nested := range typed {
			if title := findTitleForJSONKey(nested, key); title != "" {
				return title
			}
		}
	case []any:
		for _, nested := range typed {
			if title := findTitleForJSONKey(nested, key); title != "" {
				return title
			}
		}
	case string:
		if strings.Contains(typed, "{") || strings.Contains(typed, "[") {
			var nested any
			if err := json.Unmarshal([]byte(typed), &nested); err == nil {
				return findTitleForJSONKey(nested, key)
			}
		}
	}
	return ""
}

func isJSONImageKey(key string) bool {
	key = strings.ToLower(key)
	return key == "image" || key == "images" || key == "imageurl" || key == "image_url" || key == "coverimage" || key == "cover_image" || key == "src" || key == "srcmobile"
}

func extractRemoteImageURLs(text string, allow func(string) bool) []string {
	matches := remoteImageURLRE.FindAllString(text, -1)
	candidates := make([]string, 0, len(matches))
	for _, match := range matches {
		if candidate := normalizeRemoteImageCandidate(match); candidate != "" && allow(candidate) {
			candidates = append(candidates, candidate)
		}
	}
	return uniqueStrings(candidates)
}

func normalizeRemoteImageCandidate(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, `\/`, `/`)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	ext := strings.ToLower(path.Ext(parsed.Path))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".webp" && ext != ".gif" {
		return ""
	}
	return parsed.String()
}

func isOzonImageURL(candidate string) bool {
	parsed, err := url.Parse(candidate)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Host)
	return host == "cdn1.ozone.ru" || host == "cdn2.ozone.ru" || strings.HasSuffix(host, ".ozone.ru")
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func isPreviewImageKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "og:image", "og:image:url", "og:image:secure_url", "twitter:image", "twitter:image:src", "image":
		return true
	default:
		return false
	}
}

func isPreviewTitleKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "og:title", "twitter:title", "title", "name":
		return true
	default:
		return false
	}
}

func cleanPreviewTitle(value string) string {
	title := strings.TrimSpace(strings.Join(strings.Fields(value), " "))
	title = strings.Trim(title, "\"'`")
	if title == "" || len([]rune(title)) > 180 {
		return ""
	}
	for _, marker := range []string{" | Ozon", " | OZON", " | Wildberries", " | WB"} {
		if index := strings.Index(title, marker); index > 0 {
			title = strings.TrimSpace(title[:index])
		}
	}
	if title == "" || strings.Contains(title, "http://") || strings.Contains(title, "https://") {
		return ""
	}
	return title
}

func titleFromURL(pageURL *url.URL) string {
	pathPart := strings.Trim(pageURL.EscapedPath(), "/")
	if pathPart == "" {
		return ""
	}
	base := path.Base(pathPart)
	if value, err := url.PathUnescape(base); err == nil {
		base = value
	}
	base = titleTrailingIDRE.ReplaceAllString(base, "")
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.ReplaceAll(base, "_", " ")
	return cleanPreviewTitle(base)
}

func firstAttr(n *html.Node, names ...string) string {
	for _, name := range names {
		if value := attrValue(n, name); value != "" {
			return value
		}
	}
	return ""
}

func attrValue(n *html.Node, name string) string {
	name = strings.ToLower(name)
	for _, attr := range n.Attr {
		if strings.ToLower(attr.Key) == name {
			return strings.TrimSpace(attr.Val)
		}
	}
	return ""
}

func resolvePreviewURL(raw string, pageURL *url.URL) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	resolved := pageURL.ResolveReference(parsed)
	if resolved.Host == "" || (resolved.Scheme != "http" && resolved.Scheme != "https") {
		return ""
	}
	return resolved.String()
}

func (a *app) downloadRemoteImage(ctx context.Context, client *http.Client, imageURL, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", fmt.Errorf("Не получилось открыть фото товара")
	}
	req.Header.Set("User-Agent", previewUserAgent)
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Не получилось скачать фото товара")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("Фото товара не загрузилось")
	}

	limited := io.LimitReader(resp.Body, maxRemoteImageBytes+1)
	buf := make([]byte, 512)
	n, readErr := io.ReadFull(limited, buf)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return "", fmt.Errorf("Не получилось проверить фото товара")
	}
	if n == 0 {
		return "", fmt.Errorf("Фото товара пустое")
	}

	ext, ok := imageExtForContentType(http.DetectContentType(buf[:n]))
	if !ok {
		ext, ok = imageExtForContentType(resp.Header.Get("Content-Type"))
	}
	if !ok {
		return "", fmt.Errorf("Фото товара должно быть JPG, PNG, WebP или GIF")
	}

	nameToken, err := randomToken(18)
	if err != nil {
		return "", fmt.Errorf("Не получилось сохранить фото товара")
	}
	filename := nameToken + ext
	dstPath := filepath.Join(a.uploadDir, filename)
	dst, err := os.Create(dstPath)
	if err != nil {
		return "", fmt.Errorf("Не получилось сохранить фото товара")
	}
	defer dst.Close()

	total := int64(n)
	if _, err := dst.Write(buf[:n]); err != nil {
		_ = os.Remove(dstPath)
		return "", fmt.Errorf("Не получилось записать фото товара")
	}
	written, err := io.Copy(dst, limited)
	total += written
	if err != nil {
		_ = os.Remove(dstPath)
		return "", fmt.Errorf("Не получилось записать фото товара")
	}
	if total > maxRemoteImageBytes {
		_ = os.Remove(dstPath)
		return "", fmt.Errorf("Фото товара слишком большое")
	}

	return "/uploads/" + url.PathEscape(filename), nil
}

func (a *app) downloadFirstRemoteImage(ctx context.Context, client *http.Client, candidates []string, referer string) (string, error) {
	var lastErr error
	for _, candidate := range uniqueStrings(candidates) {
		imageURL, err := a.downloadRemoteImage(ctx, client, candidate, referer)
		if err == nil {
			return imageURL, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("Не нашёл фото товара")
}

func imageExtForContentType(contentType string) (string, bool) {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch contentType {
	case "image/jpeg":
		return ".jpg", true
	case "image/png":
		return ".png", true
	case "image/webp":
		return ".webp", true
	case "image/gif":
		return ".gif", true
	default:
		return "", false
	}
}

func cleanPreviewImageURL(raw string) string {
	value := strings.TrimSpace(raw)
	if !strings.HasPrefix(value, "/uploads/") {
		return ""
	}
	name, err := url.PathUnescape(strings.TrimPrefix(value, "/uploads/"))
	if err != nil || name == "" || strings.Contains(name, "/") || strings.Contains(name, `\`) {
		return ""
	}
	return "/uploads/" + url.PathEscape(name)
}

func allowedImageExt(file multipart.File, header *multipart.FileHeader) (string, error) {
	buf := make([]byte, 512)
	n, err := file.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("Не получилось проверить фото")
	}
	contentType := http.DetectContentType(buf[:n])
	switch contentType {
	case "image/jpeg":
		return ".jpg", nil
	case "image/png":
		return ".png", nil
	case "image/webp":
		return ".webp", nil
	case "image/gif":
		return ".gif", nil
	default:
		ext := strings.ToLower(filepath.Ext(header.Filename))
		if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" || ext == ".gif" {
			return ext, nil
		}
		return "", fmt.Errorf("Фото должно быть JPG, PNG, WebP или GIF")
	}
}

func (a *app) removeUploadedFile(imageURL string) {
	if !strings.HasPrefix(imageURL, "/uploads/") {
		return
	}
	name, err := url.PathUnescape(strings.TrimPrefix(imageURL, "/uploads/"))
	if err != nil || name == "" || strings.Contains(name, "/") || strings.Contains(name, `\`) {
		return
	}
	_ = os.Remove(filepath.Join(a.uploadDir, name))
}

func (a *app) serveSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	cleanURLPath := path.Clean("/" + r.URL.Path)
	requested := filepath.Join(a.webDir, strings.TrimPrefix(cleanURLPath, "/"))
	if info, err := os.Stat(requested); err == nil && !info.IsDir() {
		http.ServeFile(w, r, requested)
		return
	}
	http.ServeFile(w, r, filepath.Join(a.webDir, "index.html"))
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Admin-Token, X-Guest-Token")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}

func normalizeUsername(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func validateUsername(username string) error {
	if len([]rune(username)) < 3 {
		return fmt.Errorf("Логин должен быть от 3 символов")
	}
	if len([]rune(username)) > 40 {
		return fmt.Errorf("Логин слишком длинный")
	}
	return nil
}

func validatePassword(password string) error {
	if len([]rune(password)) < 4 {
		return fmt.Errorf("Пароль должен быть от 4 символов")
	}
	if len([]rune(password)) > 120 {
		return fmt.Errorf("Пароль слишком длинный")
	}
	return nil
}

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

func passwordMatches(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func normalizeLink(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	return "https://" + value
}

func parseGiftActionPath(rawPath, prefix string) (int64, string, bool) {
	trimmed := strings.Trim(strings.TrimPrefix(rawPath, prefix), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 {
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, "", false
	}
	return id, parts[1], true
}

func parseIDPath(rawPath, prefix string) (int64, bool) {
	trimmed := strings.Trim(strings.TrimPrefix(rawPath, prefix), "/")
	if trimmed == "" || strings.Contains(trimmed, "/") {
		return 0, false
	}
	id, err := strconv.ParseInt(trimmed, 10, 64)
	return id, err == nil && id > 0
}

func readJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "Метод не поддерживается")
}

func randomToken(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
