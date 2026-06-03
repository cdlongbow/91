package p123

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

type apiEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type loginResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Token string `json:"token"`
	} `json:"data"`
}

type qrGenerateResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		UniID string `json:"uniID"`
		URL   string `json:"url"`
	} `json:"data"`
}

type qrResultResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		LoginStatus  int    `json:"loginStatus"`
		ScanPlatform int    `json:"scanPlatform"`
		LoginType    int    `json:"login_type"`
		Token        string `json:"token"`
		AccessToken  string `json:"accessToken"`
	} `json:"data"`
}

func (r qrResultResp) TokenValue() string {
	if strings.TrimSpace(r.Data.Token) != "" {
		return r.Data.Token
	}
	return r.Data.AccessToken
}

type qrWXCodeResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		WXCodeLower string `json:"wxCode"`
		WXCodeTitle string `json:"WxCode"`
		Code        string `json:"code"`
	} `json:"data"`
}

func (r qrWXCodeResp) WXCode() string {
	if r.Data.WXCodeLower != "" {
		return r.Data.WXCodeLower
	}
	if r.Data.WXCodeTitle != "" {
		return r.Data.WXCodeTitle
	}
	return r.Data.Code
}

type fileListResp struct {
	Data struct {
		Next     string    `json:"Next"`
		Total    int       `json:"Total"`
		InfoList []panFile `json:"InfoList"`
	} `json:"data"`
}

type panFile struct {
	FileName  string       `json:"FileName"`
	Size      int64        `json:"Size"`
	UpdateAt  flexibleTime `json:"UpdateAt"`
	FileID    int64        `json:"FileId"`
	Type      int          `json:"Type"`
	Etag      string       `json:"Etag"`
	S3KeyFlag string       `json:"S3KeyFlag"`
}

type cachedFile struct {
	file     panFile
	parentID string
}

type downloadInfoResp struct {
	Data struct {
		DownloadURL      string `json:"DownloadUrl"`
		DownloadURLLower string `json:"downloadUrl"`
	} `json:"data"`
}

func (r downloadInfoResp) URL() string {
	if r.Data.DownloadURL != "" {
		return r.Data.DownloadURL
	}
	return r.Data.DownloadURLLower
}

type redirectResp struct {
	Data struct {
		RedirectURL      string `json:"redirect_url"`
		RedirectURLCamel string `json:"redirectUrl"`
		RedirectURLTitle string `json:"RedirectUrl"`
	} `json:"data"`
}

func (r redirectResp) URL() string {
	if r.Data.RedirectURL != "" {
		return r.Data.RedirectURL
	}
	if r.Data.RedirectURLCamel != "" {
		return r.Data.RedirectURLCamel
	}
	return r.Data.RedirectURLTitle
}

type mkdirResp struct {
	Data struct {
		FileID int64 `json:"FileId"`
	} `json:"data"`
}

type uploadResp struct {
	Data struct {
		AccessKeyID     string `json:"AccessKeyId"`
		Bucket          string `json:"Bucket"`
		Key             string `json:"Key"`
		SecretAccessKey string `json:"SecretAccessKey"`
		SessionToken    string `json:"SessionToken"`
		FileID          int64  `json:"FileId"`
		Reuse           bool   `json:"Reuse"`
		EndPoint        string `json:"EndPoint"`
		StorageNode     string `json:"StorageNode"`
		UploadID        string `json:"UploadId"`
	} `json:"data"`
}

type s3PreSignedURLsResp struct {
	Data struct {
		PreSignedURLs map[string]string `json:"presignedUrls"`
	} `json:"data"`
}

type flexibleTime struct {
	t time.Time
}

func (t *flexibleTime) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || string(data) == `""` {
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		t.t = parseTimeString(s)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		if n > 1_000_000_000_000 {
			t.t = time.UnixMilli(n)
		} else {
			t.t = time.Unix(n, 0)
		}
		return nil
	}
	return nil
}

func (t flexibleTime) Time() time.Time {
	return t.t
}

func parseTimeString(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if parsed, err := time.ParseInLocation(layout, s, time.FixedZone("UTC+8", 8*3600)); err == nil {
			return parsed
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1_000_000_000_000 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	return time.Time{}
}
