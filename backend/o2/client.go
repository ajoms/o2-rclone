package o2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/lib/rest"
)

const (
	defaultAPIURL    = "https://cloud.o2online.es/sapi/"
	defaultUploadURL = "https://upload.cloud.o2online.es/sapi/"
	keepaliveDefault = 240
	pageSize         = 200
)

// Client is the HTTP client for the O2 SAPI
type Client struct {
	f    *Fs
	rest *rest.Client
	mu   sync.Mutex // protects session fields and renewal
}

func newClient(f *Fs, httpClient *http.Client) *Client {
	c := &Client{f: f}
	c.rest = rest.NewClient(httpClient).
		SetRoot(f.opt.APIURL).
		SetErrorHandler(errorHandler).
		SetHeader("User-Agent", f.opt.UserAgent).
		SetHeader("Origin", strings.TrimSuffix(f.opt.APIURL, "/sapi/")).
		SetHeader("Referer", strings.TrimSuffix(f.opt.APIURL, "/sapi/")+"/").
		SetHeader("Accept-Language", "es-ES,es;q=0.9,en;q=0.8")
	return c
}

func errorHandler(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return fmt.Errorf("o2 api: HTTP %d: %s", resp.StatusCode, string(body[:min(len(body), 500)]))
}

func (c *Client) sessionHeaders() map[string]string {
	headers := map[string]string{}
	if c.f.opt.OAuthBundle != "" {
		headers["Authorization"] = "oauth " + c.f.opt.OAuthBundle
	}
	if c.f.opt.DeviceID != "" {
		headers["X-deviceid"] = c.f.opt.DeviceID
	}
	if c.f.opt.DeviceName != "" {
		headers["X-devicename"] = c.f.opt.DeviceName
	}
	if c.f.opt.CookieJSessionID != "" {
		headers["Cookie"] = "JSESSIONID=" + c.f.opt.CookieJSessionID
	}
	return headers
}

func (c *Client) baseHeaders() map[string]string {
	h := c.sessionHeaders()
	h["Accept"] = "application/json"
	h["Origin"] = strings.TrimSuffix(c.f.opt.APIURL, "/sapi/")
	h["Referer"] = strings.TrimSuffix(c.f.opt.APIURL, "/sapi/") + "/"
	h["Accept-Language"] = "es-ES,es;q=0.9,en;q=0.8"
	h["User-Agent"] = c.f.opt.UserAgent
	return h
}

func (c *Client) mergeSessionHeaders(h map[string]string) {
	for k, v := range h {
		switch strings.ToLower(k) {
		case "authorization":
			c.f.opt.OAuthBundle = strings.TrimSpace(strings.TrimPrefix(v, "oauth "))
		case "x-deviceid":
			c.f.opt.DeviceID = v
		case "x-devicename":
			c.f.opt.DeviceName = v
		case "cookie":
			cookies := strings.Split(v, ";")
			for _, cookie := range cookies {
				parts := strings.SplitN(strings.TrimSpace(cookie), "=", 2)
				if len(parts) == 2 && parts[0] == "JSESSIONID" {
					c.f.opt.CookieJSessionID = parts[1]
				}
			}
		}
	}
}

func (c *Client) updateSessionFromData(data map[string]any) {
	if vk, ok := data["validationkey"].(string); ok && vk != "" {
		c.f.opt.ValidationKey = vk
	}
	if vk, ok := data["validationKey"].(string); ok && vk != "" {
		c.f.opt.ValidationKey = vk
	}
	if at, ok := data["access_token"].(string); ok && at != "" {
		c.f.opt.OAuthBundle = at
	}
	if at, ok := data["accessToken"].(string); ok && at != "" {
		c.f.opt.OAuthBundle = at
	}
	if et, ok := data["encryption-token"].(string); ok && et != "" {
		c.f.opt.EncryptionToken = et
	}
	if et, ok := data["encryptionToken"].(string); ok && et != "" {
		c.f.opt.EncryptionToken = et
	}
	if jsessionid, ok := data["jsessionid"].(string); ok && jsessionid != "" {
		c.f.opt.CookieJSessionID = jsessionid
	}
	if jsessionid, ok := data["JSESSIONID"].(string); ok && jsessionid != "" {
		c.f.opt.CookieJSessionID = jsessionid
	}
}

func (c *Client) oauthLogin() error {
	return c.oauthLoginLocked()
}

func (c *Client) oauthLoginWithVK(vk string) error {
	return c.oauthLoginWithVKLocked(vk)
}

func (c *Client) oauthLoginLocked() error {
	return c.oauthLoginWithVKLocked(c.f.opt.ValidationKey)
}

func (c *Client) oauthLoginWithVKLocked(vk string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	u, err := url.Parse(c.f.opt.APIURL)
	if err != nil {
		return fmt.Errorf("invalid api_url: %w", err)
	}
	u.Path = path.Join(u.Path, "login", "oauth")
	q := u.Query()
	q.Set("action", "login")
	q.Set("responsetime", "true")
	if vk != "" {
		q.Set("validationkey", vk)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("POST", u.String(), nil)
	if err != nil {
		return err
	}
	for k, v := range c.baseHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.f.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth login request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("oauth login failed: HTTP %d", resp.StatusCode)
	}

	// capture rotated bundle from response header
	if auth := resp.Header.Get("Authorization"); auth != "" {
		c.mergeSessionHeaders(map[string]string{"Authorization": auth})
	}
	c.mergeSessionHeaders(map[string]string{"Cookie": resp.Header.Get("Set-Cookie")})

	var payload struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("oauth login: invalid JSON: %w", err)
	}
	c.updateSessionFromData(payload.Data)
	return c.saveConfig()
}

func (c *Client) saveConfig() error {
	config.FileSetValue(c.f.name, "validation_key", c.f.opt.ValidationKey)
	config.FileSetValue(c.f.name, "oauth_bundle", c.f.opt.OAuthBundle)
	config.FileSetValue(c.f.name, "cookie_jsessionid", c.f.opt.CookieJSessionID)
	config.FileSetValue(c.f.name, "device_id", c.f.opt.DeviceID)
	config.FileSetValue(c.f.name, "device_name", c.f.opt.DeviceName)
	config.FileSetValue(c.f.name, "user_agent", c.f.opt.UserAgent)
	config.FileSetValue(c.f.name, "encryption_token", c.f.opt.EncryptionToken)
	return nil
}

// request performs an API request with auth and automatic session renewal on 401
func (c *Client) request(ctx context.Context, method, resource string, params url.Values, body any) (*http.Response, error) {
	return c.requestWithRetry(ctx, method, resource, params, body, true)
}

func (c *Client) requestWithRetry(ctx context.Context, method, resource string, params url.Values, body any, retry bool) (*http.Response, error) {
	reqURL := c.buildURL(resource, params)

	var bodyReader io.Reader
	var bodyJSON string
	if body != nil {
		switch b := body.(type) {
		case []byte:
			bodyReader = strings.NewReader(string(b))
			bodyJSON = string(b)
		case string:
			bodyReader = strings.NewReader(b)
		case io.Reader:
			bodyReader = b
		default:
			data, err := json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("marshal request body: %w", err)
			}
			bodyReader = strings.NewReader(string(data))
			bodyJSON = string(data)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if bodyJSON != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.baseHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := c.f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if retry && (resp.StatusCode == 401 || resp.StatusCode == 403) {
		resp.Body.Close()
		err := c.recoverSession(resp)
		if err == nil {
			return c.requestWithRetry(ctx, method, resource, params, body, false)
		}
	}

	return resp, nil
}

func (c *Client) recoverSession(rejectedResp *http.Response) error {
	// check if the 401 response contains a rotated bundle in authorization header
	if auth := rejectedResp.Header.Get("Authorization"); auth != "" {
		newBundle := strings.TrimSpace(strings.TrimPrefix(auth, "oauth"))
		if newBundle != "" {
			// try the oauth login with the rotated bundle
			return c.oauthLoginWithVKLocked(newBundle)
		}
	}

	// no rotated bundle — try oauth login with current session
	return c.oauthLoginLocked()
}

func (c *Client) buildURL(resource string, params url.Values) string {
	u, _ := url.Parse(c.f.opt.APIURL)
	u.Path = path.Join(u.Path, resource)
	if params == nil {
		params = url.Values{}
	}
	params.Set("validationkey", c.f.opt.ValidationKey)
	u.RawQuery = params.Encode()
	return u.String()
}

func (c *Client) buildUploadURL(params url.Values) string {
	u, _ := url.Parse(c.f.opt.UploadURL)
	u.Path = path.Join(u.Path, "upload")
	if params == nil {
		params = url.Values{}
	}
	params.Set("validationkey", c.f.opt.ValidationKey)
	u.RawQuery = params.Encode()
	return u.String()
}

// o2Item represents an item from the O2 API
type o2Item struct {
	id        string
	name      string
	folderID  string
	isFolder  bool
	size      int64
	modTime   time.Time
	mediaKind string
	directURL string
	node      string
	token     string
	fingerprint string
}

// listFolder lists the contents of a folder (folders + files)
func (c *Client) listFolder(ctx context.Context, folderID string) ([]o2Item, error) {
	var items []o2Item
	folders, err := c.listFolders(ctx, folderID)
	if err != nil {
		return nil, err
	}
	items = append(items, folders...)
	files, err := c.listFiles(ctx, folderID)
	if err != nil {
		return nil, err
	}
	items = append(items, files...)
	return items, nil
}

// listFolders lists subfolders
func (c *Client) listFolders(ctx context.Context, parentID string) ([]o2Item, error) {
	params := url.Values{"action": {"list"}, "parentid": {parentID}}
	resp, err := c.request(ctx, "GET", "media/folder", params, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("list folders: HTTP %d", resp.StatusCode)
	}

	// parse raw JSON to handle id as either string or number
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("list folders decode: %w", err)
	}

	data, _ := raw["data"].(map[string]any)
	if data == nil {
		data = raw
	}

	var items []o2Item
	folders, _ := data["folders"].([]any)
	for _, f := range folders {
		folder, _ := f.(map[string]any)
		if folder == nil {
			continue
		}
		id := anyToString(folder["id"])
		name, _ := folder["name"].(string)
		if id == "" || name == "" {
			continue
		}
		items = append(items, o2Item{
			id:       id,
			name:     name,
			folderID: parentID,
			isFolder: true,
			modTime:  parseTime(anyToString(folder["modificationdate"]), anyToString(folder["creationdate"])),
		})
	}
	return items, nil
}

// listFiles lists files in a folder (paginated)
func (c *Client) listFiles(ctx context.Context, folderID string) ([]o2Item, error) {
	var allItems []o2Item
	for offset := 0; offset < 100000; offset += pageSize {
		params := url.Values{
			"action":   {"get"},
			"folderid": {folderID},
			"limit":    {fmt.Sprint(pageSize)},
		}
		if offset > 0 {
			params.Set("offset", fmt.Sprint(offset))
		}

		type listFields struct {
			Fields []string `json:"fields"`
		}
		payload := map[string]any{"data": listFields{Fields: mediaListFields}}
		resp, err := c.request(ctx, "POST", "media", params, payload)
		if err != nil {
			return allItems, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return allItems, fmt.Errorf("list files: HTTP %d", resp.StatusCode)
		}

		var parsed struct {
			Data struct {
				MediaServerURL string           `json:"mediaserverurl"`
				Media          []json.RawMessage `json:"media"`
				Files          []json.RawMessage `json:"files"`
				Videos         []json.RawMessage `json:"videos"`
				Audios         []json.RawMessage `json:"audios"`
				Pictures       []json.RawMessage `json:"pictures"`
				Images         []json.RawMessage `json:"images"`
				Items          []json.RawMessage `json:"items"`
				More           bool             `json:"more"`
			} `json:"data"`
			Files2  []json.RawMessage `json:"files"`
			Media2  []json.RawMessage `json:"media"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return allItems, fmt.Errorf("list files decode: %w", err)
		}

		rawFiles := append(parsed.Data.Files, parsed.Data.Videos...)
		rawFiles = append(rawFiles, parsed.Data.Audios...)
		rawFiles = append(rawFiles, parsed.Data.Pictures...)
		rawFiles = append(rawFiles, parsed.Data.Images...)
		rawFiles = append(rawFiles, parsed.Data.Items...)
		rawFiles = append(rawFiles, parsed.Data.Media...)
		if len(rawFiles) == 0 {
			rawFiles = parsed.Files2
		}
		if len(rawFiles) == 0 {
			rawFiles = parsed.Media2
		}


		mediaServer := parsed.Data.MediaServerURL
		if mediaServer == "" {
			mediaServer = strings.TrimSuffix(c.f.opt.APIURL, "/sapi/")
		}

		seen := map[string]bool{}
		added := 0
		for _, raw := range rawFiles {
			item := parseMediaItem(raw, folderID, mediaServer)
			if item == nil {
				continue
			}
			if item.name == "" {
				continue
			}
			if item.isFolder {
				continue
			}
			key := item.id
			if key == "" {
				key = item.name
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			allItems = append(allItems, *item)
			added++
		}

		if added == 0 || !parsed.Data.More {
			break
		}
	}
	return allItems, nil
}

var mediaListFields = []string{
	"name", "modificationdate", "creationdate", "size",
	"thumbnails", "thumbnaildimensions", "viewurl",
	"videometadata", "audiometadata", "favorite", "shared",
	"etag", "origin", "folderid", "uploaded",
}

// resolveDownloadURL resolves the download URL for an item
func (c *Client) resolveDownloadURL(ctx context.Context, item *o2Item) (string, error) {
	if item.directURL != "" {
		return item.directURL, nil
	}

	type mediaGet struct {
		IDs    []string `json:"ids"`
		Fields []string `json:"fields"`
	}
	payload := map[string]any{"data": mediaGet{
		IDs:    []string{item.id},
		Fields: []string{"name", "url", "origin", "folderid", "size", "etag"},
	}}
	params := url.Values{"action": {"get"}, "origin": {"omh,dropbox"}}
	resp, err := c.request(ctx, "POST", "media", params, payload)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("resolve download URL: HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Data struct {
			MediaServerURL string `json:"mediaserverurl"`
			Media          []struct {
				DownloadURL string `json:"downloadurl"`
				URL         string `json:"url"`
				ViewURL     string `json:"viewurl"`
			} `json:"media"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("resolve download URL decode: %w", err)
	}

	mediaServer := parsed.Data.MediaServerURL
	if mediaServer == "" {
		mediaServer = strings.TrimSuffix(c.f.opt.APIURL, "/sapi/")
	}

	for _, m := range parsed.Data.Media {
		for _, u := range []string{m.DownloadURL, m.URL, m.ViewURL} {
			if u != "" {
				if !strings.HasPrefix(u, "http") {
					u = mediaServer + u
				}
				return u, nil
			}
		}
	}
	return "", fmt.Errorf("no download URL found for %s", item.id)
}

// upload uploads a file
func (c *Client) upload(ctx context.Context, parentID, name string, size int64, modTime time.Time, in io.Reader, mediaID string) (*o2Item, error) {
	metadata := map[string]any{
		"name":             name,
		"size":             size,
		"modificationdate": "",
		"folderid":         parentID,
	}
	if mediaID != "" {
		metadata["id"] = mediaID
	}

	params := url.Values{"action": {"save"}}
	if size > 200*1024*1024 {
		params.Set("acceptasynchronous", "true")
	}

	dataJSON, _ := json.Marshal(map[string]any{"data": metadata})
	contentType := guessContentType(name)
	uploadURL := c.buildUploadURL(params)

	// Streaming multipart upload (no in-memory buffering of the file).
	boundary := "----O2RcloneBoundary12345"

	// Precompute the fixed part sizes so we can set an exact Content-Length
	// while still streaming the file body (avoids chunked transfer encoding).
	dataPart := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=\"data\"\r\n\r\n%s\r\n", boundary, dataJSON)
	fileHeader := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\nContent-Type: %s\r\n\r\n", boundary, name, contentType)
	closing := fmt.Sprintf("\r\n--%s--\r\n", boundary)
	contentLength := int64(len(dataPart)) + int64(len(fileHeader)) + size + int64(len(closing))

	pipeIn, pipeOut := io.Pipe()
	writeErr := make(chan error, 1)
	go func() {
		defer pipeOut.Close()
		if _, err := io.WriteString(pipeOut, dataPart); err != nil {
			writeErr <- err
			return
		}
		if _, err := io.WriteString(pipeOut, fileHeader); err != nil {
			writeErr <- err
			return
		}
		if _, err := io.Copy(pipeOut, in); err != nil {
			writeErr <- err
			return
		}
		if _, err := io.WriteString(pipeOut, closing); err != nil {
			writeErr <- err
			return
		}
		writeErr <- nil
	}()

	req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, pipeIn)
	if err != nil {
		pipeIn.Close()
		return nil, err
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	for k, v := range c.baseHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := c.f.httpClient.Do(req)
	if err != nil {
		pipeIn.Close()
		// If the write goroutine failed first, report that instead.
		select {
		case werr := <-writeErr:
			if werr != nil {
				return nil, fmt.Errorf("upload stream error: %w", werr)
			}
		default:
		}
		return nil, err
	}
	defer resp.Body.Close()
	_ = <-writeErr // drain the writer goroutine result

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		c.recoverSession(resp)
		return nil, fmt.Errorf("upload rejected: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)
		if len(bodyStr) > 300 {
			bodyStr = bodyStr[:300]
		}
		return nil, fmt.Errorf("upload failed: HTTP %d: %s", resp.StatusCode, bodyStr)
	}

	// parse response
	var parsed struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err == nil {
		if parsed.Data.ID != "" {
			parsed.ID = parsed.Data.ID
		}
	}

	// refresh session from response
	if auth := resp.Header.Get("Authorization"); auth != "" {
		c.mergeSessionHeaders(map[string]string{"Authorization": auth})
	}

	mediaKind := mediaKindForName(name)
	return &o2Item{
		id:        parsed.ID,
		name:      name,
		folderID:  parentID,
		isFolder:  false,
		size:      size,
		modTime:   modTime,
		mediaKind: mediaKind,
	}, nil
}

// simpleMultipart provides minimal multipart form writing
type simpleMultipart struct {
	boundary string
	writer   *io.PipeWriter
	closed   bool
}

func newMultipartWriter(w *io.PipeWriter) *simpleMultipart {
	boundary := "----O2RcloneBoundary12345"
	return &simpleMultipart{boundary: boundary, writer: w}
}

func (m *simpleMultipart) writeField(name, value string) error {
	_, err := fmt.Fprintf(m.writer, "--%s\r\n", m.boundary)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(m.writer, "Content-Disposition: form-data; name=%q\r\n\r\n", name)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(m.writer, "%s\r\n", value)
	return err
}

func (m *simpleMultipart) createFormFile(name, filename, contentType string) (io.Writer, error) {
	_, err := fmt.Fprintf(m.writer, "--%s\r\n", m.boundary)
	if err != nil {
		return nil, err
	}
	_, err = fmt.Fprintf(m.writer, "Content-Disposition: form-data; name=%q; filename=%q\r\n", name, filename)
	if err != nil {
		return nil, err
	}
	_, err = fmt.Fprintf(m.writer, "Content-Type: %s\r\n\r\n", contentType)
	return m.writer, err
}

func (m *simpleMultipart) close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	_, err := fmt.Fprintf(m.writer, "--%s--\r\n", m.boundary)
	return err
}

func (m *simpleMultipart) formDataContentType() string {
	return "multipart/form-data; boundary=" + m.boundary
}

func guessContentType(name string) string {
	ext := strings.ToLower(name)
	switch {
	case strings.HasSuffix(ext, ".jpg"), strings.HasSuffix(ext, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(ext, ".png"):
		return "image/png"
	case strings.HasSuffix(ext, ".gif"):
		return "image/gif"
	case strings.HasSuffix(ext, ".mp4"):
		return "video/mp4"
	case strings.HasSuffix(ext, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(ext, ".txt"):
		return "text/plain"
	case strings.HasSuffix(ext, ".json"):
		return "application/json"
	case strings.HasSuffix(ext, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(ext, ".xml"):
		return "application/xml"
	case strings.HasSuffix(ext, ".zip"):
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

// deleteItem soft-deletes a file or folder
func (c *Client) deleteItem(ctx context.Context, item *o2Item) error {
	mediaKind := item.mediaKind
	if mediaKind == "" {
		mediaKind = mediaKindForName(item.name)
	}

	if item.isFolder {
		params := url.Values{"action": {"softdelete"}}
		payload := map[string]any{"data": map[string]any{"ids": []string{item.id}}}
		resp, err := c.request(ctx, "POST", "media/folder", params, payload)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("delete folder: HTTP %d", resp.StatusCode)
		}
		return nil
	}

	params := url.Values{"action": {"delete"}, "softdelete": {"true"}}
	payload := map[string]any{"data": map[string]any{"files": []string{item.id}}}
	resp, err := c.request(ctx, "POST", "media/"+mediaKind, params, payload)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("delete file: HTTP %d", resp.StatusCode)
	}
	return nil
}

// createFolder creates a new folder
func (c *Client) createFolder(ctx context.Context, parentID, name string) (string, error) {
	payload := map[string]any{"data": map[string]any{
		"magic":    false,
		"offline":  false,
		"name":     name,
		"parentid": parentID,
	}}
	params := url.Values{"action": {"save"}}

	// try form-encoded first (the gateway does both)
	resp, err := c.request(ctx, "POST", "media/folder", params, payload)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("create folder: HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err == nil && parsed.Data.ID != "" {
		return parsed.Data.ID, nil
	}
	return "", nil
}

// moveItem moves/renames a file or folder
func (c *Client) moveItem(ctx context.Context, item *o2Item, newName string, newParentID string) error {
	mediaKind := item.mediaKind
	if mediaKind == "" {
		mediaKind = mediaKindForName(item.name)
	}

	if item.isFolder {
		payload := map[string]any{"data": map[string]any{
			"magic":    false,
			"offline":  false,
			"id":       item.id,
			"name":     newName,
			"parentid": newParentID,
		}}
		params := url.Values{"action": {"save"}}
		resp, err := c.request(ctx, "POST", "media/folder", params, payload)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("move folder: HTTP %d", resp.StatusCode)
		}
		return nil
	}

	payload := map[string]any{"data": map[string]any{
		"id":       item.id,
		"name":     newName,
		"folderid": newParentID,
	}}
	params := url.Values{"action": {"save-metadata"}, "acceptasynchronous": {"true"}}
	resp, err := c.request(ctx, "POST", "upload/"+mediaKind, params, payload)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("move file: HTTP %d", resp.StatusCode)
	}
	return nil
}

// updateModTime updates modification time via save-metadata
func (c *Client) updateModTime(ctx context.Context, item *o2Item, modTime time.Time) error {
	mediaKind := item.mediaKind
	if mediaKind == "" {
		mediaKind = mediaKindForName(item.name)
	}

	payload := map[string]any{"data": map[string]any{
		"id":               item.id,
		"modificationdate": modTime.Format("2006-01-02T15:04:05"),
	}}
	params := url.Values{"action": {"save-metadata"}}
	resp, err := c.request(ctx, "POST", "upload/"+mediaKind, params, payload)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// getRootFolderID gets the account root folder ID
func (c *Client) getRootFolderID(ctx context.Context) (string, error) {
	resp, err := c.request(ctx, "POST", "media/folder/root", url.Values{"action": {"get"}}, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("get root folder: HTTP %d", resp.StatusCode)
	}

	// parse raw JSON to handle id as either string or number
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", fmt.Errorf("get root folder decode: %w", err)
	}

	data, _ := raw["data"].(map[string]any)
	if data == nil {
		data = raw
	}

	// try to extract id from data.folders[0].id or data.id or root-level id
	id := ""
	if folders, ok := data["folders"].([]any); ok && len(folders) > 0 {
		if folder, ok := folders[0].(map[string]any); ok {
			id = anyToString(folder["id"])
		}
	}
	if id == "" {
		id = anyToString(data["id"])
	}
	if id == "" {
		id = anyToString(raw["id"])
	}
	if id == "" {
		return "", fmt.Errorf("no root folder id in response")
	}
	return id, nil
}

// anyToString converts any JSON value to string
func anyToString(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		return strconv.FormatInt(int64(val), 10)
	case int:
		return strconv.Itoa(val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// getQuota gets storage quota
func (c *Client) getQuota(ctx context.Context) (used, total int64, err error) {
	params := url.Values{"action": {"get-storage-space"}, "softdeleted": {"true"}}
	resp, err := c.request(ctx, "GET", "media", params, nil)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, 0, fmt.Errorf("get quota: HTTP %d", resp.StatusCode)
	}

	// parse raw JSON to handle flexible field names
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return 0, 0, fmt.Errorf("get quota decode: %w", err)
	}

	data, _ := raw["data"].(map[string]any)
	if data == nil {
		data = raw
	}

	used = parseInt64(data["used"])
	total = parseInt64(data["quota"])
	if total == 0 {
		total = parseInt64(data["total"])
	}
	return used, total, nil
}

// parseInt64 converts any JSON number to int64
func parseInt64(v any) int64 {
	switch val := v.(type) {
	case float64:
		return int64(val)
	case int64:
		return val
	case int:
		return int64(val)
	case string:
		if parsed, err := strconv.ParseInt(val, 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

// keepaliveRenewer runs periodic session renewals
func (c *Client) keepaliveRenewer(ctx context.Context, cancel context.CancelFunc) {
	if c.f.opt.KeepaliveSeconds <= 0 {
		return
	}
	ticker := time.NewTicker(time.Duration(c.f.opt.KeepaliveSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.oauthLogin(); err != nil {
				fs.Infof(c.f, "keepalive renewal failed: %v", err)
			} else {
				fs.Debugf(c.f, "keepalive renewal OK")
			}
		}
	}
}

func parseTime(dateStrs ...string) time.Time {
	for _, s := range dateStrs {
		if s == "" {
			continue
		}
		// try multiple formats
		for _, format := range []string{
			"2006-01-02T15:04:05",
			"2006-01-02T15:04:05Z",
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05.000Z",
			"20060102T150405Z",
		} {
			if t, err := time.Parse(format, s); err == nil {
				return t
			}
		}
	}
	return time.Now()
}

func mediaKindForName(name string) string {
	ext := strings.ToLower(name)
	switch {
	case strings.HasSuffix(ext, ".jpg"), strings.HasSuffix(ext, ".jpeg"), strings.HasSuffix(ext, ".png"), strings.HasSuffix(ext, ".gif"), strings.HasSuffix(ext, ".heic"), strings.HasSuffix(ext, ".webp"), strings.HasSuffix(ext, ".tiff"):
		return "picture"
	case strings.HasSuffix(ext, ".mp4"), strings.HasSuffix(ext, ".mov"), strings.HasSuffix(ext, ".avi"), strings.HasSuffix(ext, ".mkv"), strings.HasSuffix(ext, ".wmv"):
		return "video"
	case strings.HasSuffix(ext, ".mp3"), strings.HasSuffix(ext, ".m4a"), strings.HasSuffix(ext, ".wav"), strings.HasSuffix(ext, ".aac"):
		return "audio"
	default:
		return "file"
	}
}

// min returns the smaller of a or b
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
