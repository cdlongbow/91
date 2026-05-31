package googledrive

import "time"

type tokenResp struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Text             string `json:"text"`
}

type filesResp struct {
	NextPageToken string       `json:"nextPageToken"`
	Files         []driveFile  `json:"files"`
	Error         apiErrorBody `json:"error"`
}

type driveFile struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	MimeType       string    `json:"mimeType"`
	ModifiedTime   time.Time `json:"modifiedTime"`
	CreatedTime    time.Time `json:"createdTime"`
	Size           string    `json:"size"`
	ThumbnailLink  string    `json:"thumbnailLink"`
	MD5Checksum    string    `json:"md5Checksum"`
	SHA1Checksum   string    `json:"sha1Checksum"`
	SHA256Checksum string    `json:"sha256Checksum"`
	Shortcut       struct {
		TargetID       string `json:"targetId"`
		TargetMimeType string `json:"targetMimeType"`
	} `json:"shortcutDetails"`
}

type apiErrorResp struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Errors  []struct {
		Domain  string `json:"domain"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"errors"`
}
