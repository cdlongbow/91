package googledrive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInitUsesOnlineRenewAPI(t *testing.T) {
	var savedAccess, savedRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/renew" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("refresh_ui"); got != "old-refresh" {
			t.Fatalf("refresh_ui = %q", got)
		}
		if got := r.URL.Query().Get("server_use"); got != "true" {
			t.Fatalf("server_use = %q", got)
		}
		if got := r.URL.Query().Get("driver_txt"); got != "googleui_go" {
			t.Fatalf("driver_txt = %q", got)
		}
		writeTestJSON(w, tokenResp{
			AccessToken:  "new-access",
			RefreshToken: "new-refresh",
		})
	}))
	defer srv.Close()

	d := New(Config{
		ID:           "g",
		RefreshToken: "old-refresh",
		UseOnlineAPI: true,
		RenewAPIURL:  srv.URL + "/renew",
		OnTokenUpdate: func(access, refresh string) {
			savedAccess = access
			savedRefresh = refresh
		},
	})
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if d.accessToken != "new-access" || d.refreshToken != "new-refresh" {
		t.Fatalf("tokens not applied: access=%q refresh=%q", d.accessToken, d.refreshToken)
	}
	if savedAccess != "new-access" || savedRefresh != "new-refresh" {
		t.Fatalf("tokens not persisted: access=%q refresh=%q", savedAccess, savedRefresh)
	}
}

func TestListMapsGoogleDriveFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.URL.Path != "/drive/v3/files" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.Query().Get("q"), "'root' in parents") {
			t.Fatalf("unexpected q = %q", r.URL.Query().Get("q"))
		}
		writeTestJSON(w, filesResp{Files: []driveFile{
			{ID: "folder-1", Name: "Movies", MimeType: "application/vnd.google-apps.folder"},
			{
				ID:            "file-1",
				Name:          "clip.mp4",
				MimeType:      "video/mp4",
				Size:          "1234",
				MD5Checksum:   "abc",
				ThumbnailLink: "https://thumb.example/1",
			},
		}})
	}))
	defer srv.Close()

	d := New(Config{ID: "g", RootID: "root", APIBaseURL: srv.URL + "/drive/v3"})
	d.accessToken = "access"
	d.listInterval = -1

	entries, err := d.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d", len(entries))
	}
	if !entries[0].IsDir || entries[0].ID != "folder-1" {
		t.Fatalf("folder entry = %+v", entries[0])
	}
	if entries[1].ID != "file-1" || entries[1].Size != 1234 || entries[1].Hash != "abc" || entries[1].ThumbnailURL == "" {
		t.Fatalf("file entry = %+v", entries[1])
	}
}

func TestStreamURLReturnsAuthenticatedMediaLinkWithoutRedirectRequirement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.URL.Path != "/drive/v3/files/file-1" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		writeTestJSON(w, driveFile{
			ID:       "file-1",
			Name:     "clip.mp4",
			MimeType: "video/mp4",
			Size:     "1234",
		})
	}))
	defer srv.Close()

	d := New(Config{ID: "g", APIBaseURL: srv.URL + "/drive/v3"})
	d.accessToken = "access"

	link, err := d.StreamURL(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("StreamURL() error = %v", err)
	}
	if !strings.HasPrefix(link.URL, srv.URL+"/drive/v3/files/file-1?") {
		t.Fatalf("link URL = %q", link.URL)
	}
	if !strings.Contains(link.URL, "alt=media") {
		t.Fatalf("link URL missing alt=media: %q", link.URL)
	}
	if got := link.Headers.Get("Authorization"); got != "Bearer access" {
		t.Fatalf("link Authorization = %q", got)
	}
}

func TestRequestRefreshesOnUnauthorized(t *testing.T) {
	var fileCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/renew":
			writeTestJSON(w, tokenResp{
				AccessToken:  "new-access",
				RefreshToken: "new-refresh",
			})
		case "/drive/v3/files/file-1":
			fileCalls++
			if fileCalls == 1 {
				writeTestJSONStatus(w, http.StatusUnauthorized, apiErrorResp{Error: apiErrorBody{
					Code:    http.StatusUnauthorized,
					Message: "Invalid Credentials",
				}})
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer new-access" {
				t.Fatalf("Authorization after refresh = %q", got)
			}
			writeTestJSON(w, driveFile{ID: "file-1", Name: "clip.mp4", Size: "1"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := New(Config{
		ID:           "g",
		RefreshToken: "old-refresh",
		UseOnlineAPI: true,
		RenewAPIURL:  srv.URL + "/renew",
		APIBaseURL:   srv.URL + "/drive/v3",
	})
	d.accessToken = "old-access"

	if _, err := d.Stat(context.Background(), "file-1"); err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if fileCalls != 2 {
		t.Fatalf("fileCalls = %d", fileCalls)
	}
	if d.accessToken != "new-access" || d.refreshToken != "new-refresh" {
		t.Fatalf("tokens not refreshed: access=%q refresh=%q", d.accessToken, d.refreshToken)
	}
}

func writeTestJSON(w http.ResponseWriter, v any) {
	writeTestJSONStatus(w, http.StatusOK, v)
}

func writeTestJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
