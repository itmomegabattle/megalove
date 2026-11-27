package main

import (
	"bufio"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	adminCookie   = "megalove_admin"
	adminTTL      = 12 * time.Hour
	adminAttempts = 6
	adminHashSize = 16 + sha256.Size
)

type adminAuth struct {
	passwordHash []byte
	secureCookie bool

	mu            sync.Mutex
	sessionDigest [sha256.Size]byte
	sessionExpiry time.Time
	windowEnds    time.Time
	attempts      int
}

func deriveAdminKey(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, 600_000, sha256.Size)
}

func printAdminHash(input io.Reader, output io.Writer) error {
	line := bufio.NewScanner(input)
	line.Buffer(make([]byte, 1028), 1028)
	if !line.Scan() || len(line.Bytes()) > 1024 || utf8.RuneCount(line.Bytes()) < 15 {
		return errors.New("передай одну строку с паролем от 15 символов и до 1024 байт через stdin")
	}
	salt := make([]byte, 16, adminHashSize)
	rand.Read(salt)
	key, err := deriveAdminKey(line.Text(), salt)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, base64.RawStdEncoding.EncodeToString(append(salt, key...)))
	return err
}

func mountAdmin(mux *http.ServeMux, db *sql.DB, voting votingEvent, encodedHash, address string, localHTTP bool) error {
	admin := &adminAuth{secureCookie: !localHTTP}
	if localHTTP && !loopbackAddress(address) {
		return errors.New("MEGALOVE_ADMIN_LOCAL_HTTP требует локальный IP в MEGALOVE_ADDR")
	}
	if encodedHash != "" {
		decoded, err := base64.RawStdEncoding.DecodeString(encodedHash)
		if err != nil || len(decoded) != adminHashSize {
			return errors.New("некорректный MEGALOVE_ADMIN_HASH, создай его командой admin-hash")
		}
		admin.passwordHash = decoded
	}

	// Register all private admin handlers behind this shared authentication gate.
	private := http.NewServeMux()
	private.HandleFunc("GET /admin", voting.dashboard)
	private.HandleFunc("POST /admin/registration", voting.toggleRegistration)
	private.HandleFunc("GET /admin/events/{event}/registration/qr", voting.registrationQR)
	private.HandleFunc("POST /admin/events/create", voting.createEvent)
	private.HandleFunc("POST /admin/events/select", voting.selectEvent)
	private.HandleFunc("POST /admin/events/{event}/edit", voting.editEvent)
	private.HandleFunc("POST /admin/events/{event}/delete", voting.deleteEvent)
	private.HandleFunc("POST /admin/logout", admin.logout)
	private.HandleFunc("GET /admin/voting", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin#voting", http.StatusSeeOther)
	})
	private.HandleFunc("POST /admin/photo-preview", previewPortrait)
	private.HandleFunc("GET /admin/test-qr", func(w http.ResponseWriter, r *http.Request) {
		png, err := whiteBackupQR("https://vote.megabattle.site/test/")
		if err != nil {
			participantError(w, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `attachment; filename="megabattle-test.png"`)
		w.Write(png)
	})
	private.HandleFunc("POST /admin/voting", voting.changeState)
	private.HandleFunc("GET /admin/results", voting.resultsPage)
	private.HandleFunc("GET /admin/results/export", voting.exportResults)
	private.HandleFunc("GET /admin/results/journal", voting.ballotJournal)
	private.HandleFunc("GET /admin/results/timeline", voting.ballotTimeline)
	private.HandleFunc("GET /admin/backup-form/candidates", voting.backupCandidates)
	private.HandleFunc("GET /admin/events/{event}/backup", voting.backupPage)
	private.HandleFunc("GET /admin/events/{event}/backup/qr", voting.backupQR)
	private.HandleFunc("POST /admin/events/{event}/backup", voting.activateBackup)
	private.HandleFunc("POST /admin/events/{event}/backup/review", voting.reviewBackup)
	mountParticipants(private, db)
	adminRoutes := http.NewServeMux()
	adminRoutes.HandleFunc("GET /admin/login", admin.loginForm)
	adminRoutes.HandleFunc("POST /admin/login", admin.login)
	adminRoutes.Handle("/admin", admin.requireSession(private))
	adminRoutes.Handle("/admin/", admin.requireSession(private))
	originCheck := http.NewCrossOriginProtection().Handler(adminRoutes)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self' blob:; style-src 'self'; font-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if len(admin.passwordHash) == 0 {
			http.Error(w, "Вход администратора ещё не настроен", http.StatusServiceUnavailable)
			return
		}
		originCheck.ServeHTTP(w, r)
	})
	mux.Handle("/admin", handler)
	mux.Handle("/admin/", handler)
	return nil
}

func (a *adminAuth) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.validSession(r) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			} else {
				http.Error(w, "Требуется вход", http.StatusUnauthorized)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *adminAuth) validSession(r *http.Request) bool {
	cookie, err := r.Cookie(adminCookie)
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(cookie.Value))
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Before(a.sessionExpiry) && subtle.ConstantTimeCompare(digest[:], a.sessionDigest[:]) == 1
}

func (a *adminAuth) allowAttempt(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !now.Before(a.windowEnds) {
		a.windowEnds = now.Add(time.Minute)
		a.attempts = 0
	}
	if a.attempts >= adminAttempts {
		return false
	}
	a.attempts++
	return true
}

func (a *adminAuth) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		writeLogin(w, http.StatusBadRequest, "Не удалось прочитать форму.")
		return
	}
	password := r.PostForm.Get("password")
	if password == "" || len(password) > 1024 {
		writeLogin(w, http.StatusBadRequest, "Введи пароль.")
		return
	}
	if !a.allowAttempt(time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeLogin(w, http.StatusTooManyRequests, "Слишком много попыток. Попробуй через минуту.")
		return
	}
	key, err := deriveAdminKey(password, a.passwordHash[:16])
	if err != nil || subtle.ConstantTimeCompare(key, a.passwordHash[16:]) != 1 {
		writeLogin(w, http.StatusUnauthorized, "Неверный пароль.")
		return
	}

	rawToken := make([]byte, 32)
	rand.Read(rawToken)
	token := base64.RawURLEncoding.EncodeToString(rawToken)
	expires := time.Now().Add(adminTTL)
	a.mu.Lock()
	a.sessionDigest = sha256.Sum256([]byte(token))
	a.sessionExpiry = expires
	a.mu.Unlock()

	cookie := a.sessionCookie(token)
	cookie.Expires = expires
	cookie.MaxAge = int(adminTTL.Seconds())
	http.SetCookie(w, cookie)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *adminAuth) logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(adminCookie)
	if err == nil {
		digest := sha256.Sum256([]byte(cookie.Value))
		a.mu.Lock()
		if subtle.ConstantTimeCompare(digest[:], a.sessionDigest[:]) == 1 {
			a.sessionDigest = [sha256.Size]byte{}
			a.sessionExpiry = time.Time{}
		}
		a.mu.Unlock()
	}
	cleared := a.sessionCookie("")
	cleared.MaxAge = -1
	http.SetCookie(w, cleared)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (a *adminAuth) sessionCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name: adminCookie, Value: token, Path: "/admin", HttpOnly: true,
		Secure: a.secureCookie, SameSite: http.SameSiteStrictMode,
	}
}

func (a *adminAuth) loginForm(w http.ResponseWriter, r *http.Request) {
	if a.validSession(r) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	writeLogin(w, http.StatusOK, "")
}

func writeLogin(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := participantPages.ExecuteTemplate(w, "admin-login", message); err != nil {
		log.Printf("admin login template: %v", err)
	}
}
