package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

func main() {
	const folder = "1CHx9TOS4L5avbIQWmVvdRVVHS7fJHQqW"
	pick := len(os.Args) == 3 && os.Args[2] == "--pick-vote"
	export := len(os.Args) == 3 && os.Args[2] == "--export-vote"
	if len(os.Args) != 2 && !pick && !export {
		log.Fatal("Usage: google-connect <OAuth client JSON> [--pick-vote | --export-vote]")
	}
	raw, err := os.ReadFile(os.Args[1])
	check(err)
	cfg, err := google.ConfigFromJSON(raw, "https://www.googleapis.com/auth/drive.file")
	check(err)
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "MegaLove")
	check(os.MkdirAll(dir, 0700))
	if export {
		tokenJSON, err := os.ReadFile(filepath.Join(dir, "google-oauth-vote-token.json"))
		check(err)
		var token oauth2.Token
		check(json.Unmarshal(tokenJSON, &token))
		if token.RefreshToken == "" {
			log.Fatal("VOTE token has no refresh token")
		}
		credentials, err := json.Marshal(map[string]string{
			"type": "authorized_user", "client_id": cfg.ClientID,
			"client_secret": cfg.ClientSecret, "refresh_token": token.RefreshToken,
		})
		check(err)
		saveNew(filepath.Join(dir, "google-vote-credentials.json"), credentials)
		fmt.Println("VOTE server credentials saved under LocalAppData/MegaLove.")
		return
	}
	dst := filepath.Join(dir, "google-oauth-token.json")
	if pick {
		dst = filepath.Join(dir, "google-oauth-vote-token.json")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		log.Fatal("Token file already exists or cannot be checked")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	cfg.RedirectURL = "http://" + ln.Addr().String() + "/callback"
	state, verifier := rand.Text(), oauth2.GenerateVerifier()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result := make(chan string, 1)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/callback" || q.Get("state") != state {
			http.NotFound(w, r)
			return
		}
		if pick && q.Get("picked_file_ids") != folder {
			fmt.Fprintln(w, "VOTE was not selected.")
			select {
			case result <- "":
			default:
			}
			return
		}
		fmt.Fprintln(w, "Return to PowerShell to see the connection result.")
		select {
		case result <- q.Get("code"):
		default:
		}
	})}
	defer srv.Close()
	go srv.Serve(ln)
	options := []oauth2.AuthCodeOption{oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("login_hint", "itmomegabattle@gmail.com")}
	if pick {
		options = append(options, oauth2.SetAuthURLParam("prompt", "consent"), oauth2.SetAuthURLParam("trigger_onepick", "true"), oauth2.SetAuthURLParam("allow_folder_selection", "true"), oauth2.SetAuthURLParam("file_ids", folder))
	} else {
		options = append(options, oauth2.SetAuthURLParam("prompt", "consent select_account"))
	}
	url := cfg.AuthCodeURL(state, options...)
	check(exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start())
	select {
	case code := <-result:
		if code == "" {
			log.Fatal("Authorization was not granted")
		}
		token, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
		if err != nil || token.RefreshToken == "" {
			log.Fatal("Google did not return a refresh token")
		}
		if pick {
			client := cfg.Client(ctx, token)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/drive/v3/files/"+folder+"?fields=mimeType,capabilities(canAddChildren)", nil)
			check(err)
			resp, err := client.Do(req)
			check(err)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				log.Fatalf("Drive folder HTTP %d", resp.StatusCode)
			}
			var info struct {
				MimeType     string
				Capabilities struct{ CanAddChildren bool }
			}
			check(json.NewDecoder(resp.Body).Decode(&info))
			if info.MimeType != "application/vnd.google-apps.folder" || !info.Capabilities.CanAddChildren {
				log.Fatal("No permission to add files to VOTE")
			}
		}
		data, err := json.Marshal(token)
		check(err)
		saveNew(dst, data)
		fmt.Println("Google authorization saved.")
	case <-ctx.Done():
		log.Fatal("Authorization timed out")
	}
}

func saveNew(path string, data []byte) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	check(err)
	_, err = f.Write(data)
	closeErr := f.Close()
	check(err)
	check(closeErr)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
