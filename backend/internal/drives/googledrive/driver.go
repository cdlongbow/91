package googledrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"

	"github.com/video-site/backend/internal/drives"
)

const (
	Kind                = "googledrive"
	defaultAPIBaseURL   = "https://www.googleapis.com/drive/v3"
	defaultOAuthURL     = "https://www.googleapis.com/oauth2/v4/token"
	defaultRenewAPIURL  = "https://api.oplist.org/googleui/renewapi"
	defaultListInterval = 1 * time.Second
	defaultListCooldown = 5 * time.Minute

	filesListFields = "files(id,name,mimeType,size,modifiedTime,createdTime,thumbnailLink,shortcutDetails,md5Checksum,sha1Checksum,sha256Checksum),nextPageToken"
	fileInfoFields  = "id,name,mimeType,size,modifiedTime,createdTime,thumbnailLink,shortcutDetails,md5Checksum,sha1Checksum,sha256Checksum"
)

type Driver struct {
	id            string
	rootID        string
	refreshToken  string
	accessToken   string
	clientID      string
	clientSecret  string
	useOnlineAPI  bool
	renewAPIURL   string
	oauthURL      string
	apiBaseURL    string
	client        *resty.Client
	onTokenUpdate func(access, refresh string)

	listMu       sync.Mutex
	lastListAt   time.Time
	listInterval time.Duration
	listCooldown time.Duration
}

type Config struct {
	ID           string
	RootID       string
	RefreshToken string
	AccessToken  string
	ClientID     string
	ClientSecret string
	UseOnlineAPI bool
	RenewAPIURL  string
	OAuthURL     string
	APIBaseURL   string

	OnTokenUpdate func(access, refresh string)
}

func New(c Config) *Driver {
	rootID := strings.TrimSpace(c.RootID)
	if rootID == "" {
		rootID = "root"
	}
	renewAPIURL := strings.TrimSpace(c.RenewAPIURL)
	if renewAPIURL == "" {
		renewAPIURL = defaultRenewAPIURL
	}
	oauthURL := strings.TrimSpace(c.OAuthURL)
	if oauthURL == "" {
		oauthURL = defaultOAuthURL
	}
	apiBaseURL := strings.TrimRight(strings.TrimSpace(c.APIBaseURL), "/")
	if apiBaseURL == "" {
		apiBaseURL = defaultAPIBaseURL
	}
	return &Driver{
		id:            c.ID,
		rootID:        rootID,
		refreshToken:  strings.TrimSpace(c.RefreshToken),
		accessToken:   strings.TrimSpace(c.AccessToken),
		clientID:      strings.TrimSpace(c.ClientID),
		clientSecret:  strings.TrimSpace(c.ClientSecret),
		useOnlineAPI:  c.UseOnlineAPI,
		renewAPIURL:   renewAPIURL,
		oauthURL:      oauthURL,
		apiBaseURL:    apiBaseURL,
		onTokenUpdate: c.OnTokenUpdate,
		client: resty.New().
			SetTimeout(30*time.Second).
			SetHeader("Accept", "application/json, text/plain, */*"),
		listInterval: defaultListInterval,
		listCooldown: defaultListCooldown,
	}
}

func (d *Driver) Kind() string   { return Kind }
func (d *Driver) ID() string     { return d.id }
func (d *Driver) RootID() string { return d.rootID }

func (d *Driver) Init(ctx context.Context) error {
	if d.refreshToken == "" {
		return errors.New("googledrive init: refresh_token is required")
	}
	return d.refresh(ctx)
}

func (d *Driver) List(ctx context.Context, dirID string) ([]drives.Entry, error) {
	if dirID == "" {
		dirID = d.rootID
	}
	d.listMu.Lock()
	defer d.listMu.Unlock()

	pageToken := ""
	out := make([]drives.Entry, 0)
	for {
		if err := d.waitForListSlotLocked(ctx); err != nil {
			return nil, err
		}
		var resp filesResp
		err := d.request(ctx, d.filesURL(), http.MethodGet, func(req *resty.Request) {
			params := map[string]string{
				"fields":   filesListFields,
				"pageSize": "1000",
				"q":        fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(dirID, "'", "\\'")),
				"orderBy":  "folder,name,modifiedTime desc",
			}
			if pageToken != "" {
				params["pageToken"] = pageToken
			}
			req.SetQueryParams(params)
		}, &resp)
		if err != nil {
			if wait, ok := drives.RateLimitRetryAfter(err); ok {
				if wait <= 0 {
					wait = d.listCooldown
				}
				if sleepErr := sleepContext(ctx, wait); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			return nil, fmt.Errorf("googledrive list: %w", err)
		}
		if err := d.fillShortcutFileMetadata(ctx, resp.Files); err != nil {
			return nil, fmt.Errorf("googledrive shortcut metadata: %w", err)
		}
		for _, f := range resp.Files {
			out = append(out, fileToEntry(f, dirID))
		}
		pageToken = resp.NextPageToken
		if pageToken == "" {
			return out, nil
		}
	}
}

func (d *Driver) waitForListSlotLocked(ctx context.Context) error {
	if d.listInterval <= 0 || d.lastListAt.IsZero() {
		d.lastListAt = time.Now()
		return ctx.Err()
	}
	next := d.lastListAt.Add(d.listInterval)
	now := time.Now()
	if now.Before(next) {
		if err := sleepContext(ctx, next.Sub(now)); err != nil {
			return err
		}
	}
	d.lastListAt = time.Now()
	return ctx.Err()
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (d *Driver) Stat(ctx context.Context, fileID string) (*drives.Entry, error) {
	var f driveFile
	if err := d.request(ctx, d.fileURL(fileID), http.MethodGet, func(req *resty.Request) {
		req.SetQueryParam("fields", fileInfoFields)
	}, &f); err != nil {
		return nil, fmt.Errorf("googledrive stat: %w", err)
	}
	e := fileToEntry(f, "")
	return &e, nil
}

func (d *Driver) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	if fileID == "" {
		return nil, errors.New("googledrive stream: empty file id")
	}
	if _, err := d.Stat(ctx, fileID); err != nil {
		return nil, fmt.Errorf("googledrive stream: %w", err)
	}
	u := d.fileURL(fileID) + "?alt=media&acknowledgeAbuse=true&supportsAllDrives=true"
	return &drives.StreamLink{
		URL: u,
		Headers: http.Header{
			"Authorization": []string{"Bearer " + d.accessToken},
		},
		Expires: time.Now().Add(30 * time.Minute),
	}, nil
}

func (d *Driver) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}

func (d *Driver) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}

func (d *Driver) refresh(ctx context.Context) error {
	if d.useOnlineAPI && d.renewAPIURL != "" {
		var out tokenResp
		res, err := d.client.R().
			SetContext(ctx).
			SetQueryParams(map[string]string{
				"refresh_ui": d.refreshToken,
				"server_use": "true",
				"driver_txt": "googleui_go",
			}).
			SetResult(&out).
			SetError(&out).
			Get(d.renewAPIURL)
		if err != nil {
			return fmt.Errorf("googledrive refresh token: %w", err)
		}
		if err := tokenResponseError("googledrive refresh token", res, out, true); err != nil {
			return err
		}
		d.applyToken(out)
		return nil
	}
	if d.clientID == "" || d.clientSecret == "" {
		return errors.New("googledrive refresh token: client_id and client_secret are required when online API is disabled")
	}
	var out tokenResp
	res, err := d.client.R().
		SetContext(ctx).
		SetFormData(map[string]string{
			"client_id":     d.clientID,
			"client_secret": d.clientSecret,
			"refresh_token": d.refreshToken,
			"grant_type":    "refresh_token",
		}).
		SetResult(&out).
		SetError(&out).
		Post(d.oauthURL)
	if err != nil {
		return fmt.Errorf("googledrive refresh token: %w", err)
	}
	if err := tokenResponseError("googledrive refresh token", res, out, false); err != nil {
		return err
	}
	d.applyToken(out)
	return nil
}

func (d *Driver) applyToken(out tokenResp) {
	d.accessToken = out.AccessToken
	if strings.TrimSpace(out.RefreshToken) != "" {
		d.refreshToken = out.RefreshToken
	}
	if d.onTokenUpdate != nil {
		d.onTokenUpdate(d.accessToken, d.refreshToken)
	}
}

func tokenResponseError(prefix string, res *resty.Response, out tokenResp, requireRefresh bool) error {
	if out.Text != "" {
		return fmt.Errorf("%s: %s", prefix, out.Text)
	}
	if out.Error != "" {
		if out.ErrorDescription != "" {
			return fmt.Errorf("%s: %s", prefix, out.ErrorDescription)
		}
		return fmt.Errorf("%s: %s", prefix, out.Error)
	}
	if res != nil && res.IsError() {
		return fmt.Errorf("%s: status=%d body=%s", prefix, res.StatusCode(), strings.TrimSpace(res.String()))
	}
	if out.AccessToken == "" || (requireRefresh && out.RefreshToken == "") {
		return fmt.Errorf("%s: empty token", prefix)
	}
	return nil
}

func (d *Driver) request(ctx context.Context, rawURL, method string, configure func(*resty.Request), out any) error {
	return d.requestOnce(ctx, rawURL, method, configure, out, true)
}

func (d *Driver) requestOnce(ctx context.Context, rawURL, method string, configure func(*resty.Request), out any, retry bool) error {
	req := d.client.R().
		SetContext(ctx).
		SetHeader("Authorization", "Bearer "+d.accessToken).
		SetQueryParam("includeItemsFromAllDrives", "true").
		SetQueryParam("supportsAllDrives", "true")
	if configure != nil {
		configure(req)
	}
	if out != nil {
		req.SetResult(out)
	}
	var apiErr apiErrorResp
	req.SetError(&apiErr)
	res, err := req.Execute(method, rawURL)
	if err != nil {
		return err
	}
	if isGoogleRateLimit(res, apiErr.Error) {
		return googleRateLimitError(res, apiErr.Error.Message)
	}
	if apiErr.Error.Code != 0 {
		if apiErr.Error.Code == http.StatusUnauthorized && retry {
			if err := d.refresh(ctx); err != nil {
				return err
			}
			return d.requestOnce(ctx, rawURL, method, configure, out, false)
		}
		return googleAPIError(apiErr.Error)
	}
	if res.IsError() {
		return fmt.Errorf("google drive api error: status=%d body=%s", res.StatusCode(), strings.TrimSpace(res.String()))
	}
	return nil
}

func (d *Driver) fillShortcutFileMetadata(ctx context.Context, files []driveFile) error {
	for i := range files {
		f := &files[i]
		if f.MimeType != "application/vnd.google-apps.shortcut" ||
			f.Shortcut.TargetID == "" ||
			f.Shortcut.TargetMimeType == "application/vnd.google-apps.folder" {
			continue
		}
		var target driveFile
		if err := d.request(ctx, d.fileURL(f.Shortcut.TargetID), http.MethodGet, func(req *resty.Request) {
			req.SetQueryParam("fields", fileInfoFields)
		}, &target); err != nil {
			return err
		}
		if target.Size != "" {
			f.Size = target.Size
		}
		if target.MD5Checksum != "" {
			f.MD5Checksum = target.MD5Checksum
		}
		if target.SHA1Checksum != "" {
			f.SHA1Checksum = target.SHA1Checksum
		}
		if target.SHA256Checksum != "" {
			f.SHA256Checksum = target.SHA256Checksum
		}
	}
	return nil
}

func (d *Driver) filesURL() string {
	return d.apiBaseURL + "/files"
}

func (d *Driver) fileURL(fileID string) string {
	return d.filesURL() + "/" + url.PathEscape(fileID)
}

func fileToEntry(f driveFile, fallbackParentID string) drives.Entry {
	id := f.ID
	isDir := f.MimeType == "application/vnd.google-apps.folder"
	if f.MimeType == "application/vnd.google-apps.shortcut" && f.Shortcut.TargetID != "" {
		id = f.Shortcut.TargetID
		isDir = f.Shortcut.TargetMimeType == "application/vnd.google-apps.folder"
	}
	size, _ := strconv.ParseInt(f.Size, 10, 64)
	hash := f.MD5Checksum
	if hash == "" {
		hash = f.SHA1Checksum
	}
	if hash == "" {
		hash = f.SHA256Checksum
	}
	return drives.Entry{
		ID:           id,
		Name:         f.Name,
		Size:         size,
		Hash:         hash,
		IsDir:        isDir,
		ParentID:     fallbackParentID,
		MimeType:     mimeType(f),
		ModTime:      f.ModifiedTime,
		ThumbnailURL: f.ThumbnailLink,
	}
}

func mimeType(f driveFile) string {
	if f.MimeType != "" && f.MimeType != "application/vnd.google-apps.shortcut" {
		return f.MimeType
	}
	if f.Shortcut.TargetMimeType != "" {
		return f.Shortcut.TargetMimeType
	}
	ext := strings.ToLower(path.Ext(f.Name))
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".avi":
		return "video/x-msvideo"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

func isGoogleRateLimit(res *resty.Response, body apiErrorBody) bool {
	if res != nil && res.StatusCode() == http.StatusTooManyRequests {
		return true
	}
	if body.Code == http.StatusTooManyRequests {
		return true
	}
	for _, e := range body.Errors {
		reason := strings.ToLower(strings.TrimSpace(e.Reason))
		switch reason {
		case "ratelimitexceeded", "userratelimitexceeded", "downloadquotaexceeded", "sharingratelimitexceeded":
			return true
		}
	}
	msg := strings.ToLower(body.Message)
	return strings.Contains(msg, "rate limit") || strings.Contains(msg, "too many requests") || strings.Contains(msg, "quota exceeded")
}

func googleRateLimitError(res *resty.Response, message string) error {
	if strings.TrimSpace(message) == "" {
		message = "google drive rate limited"
	}
	if res != nil && strings.TrimSpace(res.String()) != "" {
		message = fmt.Sprintf("%s: status=%d body=%s", message, res.StatusCode(), strings.TrimSpace(res.String()))
	}
	return &drives.RateLimitError{
		Provider:   Kind,
		RetryAfter: parseRetryAfter(res),
		Err:        errors.New(message),
	}
}

func googleAPIError(body apiErrorBody) error {
	if body.Message != "" {
		return errors.New(body.Message)
	}
	if body.Code != 0 {
		return fmt.Errorf("google drive api error: code=%d", body.Code)
	}
	return errors.New("google drive api error")
}

func parseRetryAfter(res *resty.Response) time.Duration {
	if res == nil {
		return 0
	}
	raw := strings.TrimSpace(res.Header().Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		d := time.Until(when)
		if d > 0 {
			return d
		}
	}
	return 0
}

var _ drives.Drive = (*Driver)(nil)
