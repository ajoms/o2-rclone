// Package o2 provides an interface to the O2 Cloud storage system.
package o2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
)

const (
	minSleep      = 200 * time.Millisecond
	maxSleep      = 5 * time.Second
	decayConstant = 2
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "o2",
		Description: "O2 Cloud (Funambol MediaHub)",
		NewFs:       NewFs,
		Config:      Config,
		Options: []fs.Option{
			{
				Name:      "validation_key",
				Help:      "O2 Cloud validation key (from authorize command).",
				Sensitive: true,
				Required:  true,
			},
			{
				Name:      "oauth_bundle",
				Help:      "O2 Cloud OAuth access token bundle.",
				Sensitive: true,
				Required:  true,
			},
			{
				Name:       "cookie_jsessionid",
				Help:       "O2 Cloud JSESSIONID cookie (optional, auto-obtained on renewal).",
				Advanced:   true,
			},
			{
				Name:     "device_id",
				Help:     "Device identifier for O2 API.",
				Default:  "O2CloudRclone",
				Advanced: true,
			},
			{
				Name:     "device_name",
				Help:     "Device name for O2 API.",
				Default:  "O2Cloud",
				Advanced: true,
			},
			{
				Name:     "user_agent",
				Help:     "User-Agent for O2 API requests.",
				Default:  "O2CloudGateway/0.1",
				Advanced: true,
			},
			{
				Name:      "encryption_token",
				Help:      "O2 Cloud encryption token (auto-obtained on renewal).",
				Sensitive: true,
				Advanced:  true,
			},
			{
				Name:     "api_url",
				Help:     "O2 Cloud SAPI API base URL.",
				Default:  defaultAPIURL,
				Advanced: true,
			},
			{
				Name:     "upload_url",
				Help:     "O2 Cloud upload API base URL.",
				Default:  defaultUploadURL,
				Advanced: true,
			},
			{
				Name:     "keepalive_seconds",
				Help:     "Interval in seconds between automatic session renewals. 0 to disable.",
				Default:  keepaliveDefault,
				Advanced: true,
			},
			{
				Name:       "python3",
				Help:       "Path to python3 for the authorize helper.",
				Default:    "python3",
				Advanced:   true,
			},
			{
				Name:       "login_helper",
				Help:       "Path to the o2_authorize.py helper script. Leave empty to auto-detect.",
				Default:    "",
				Advanced:   true,
			},
			{
				Name:     config.ConfigEncoding,
				Help:     config.ConfigEncodingHelp,
				Advanced: true,
				Default: (encoder.Display |
					encoder.EncodeBackSlash |
					encoder.EncodeDoubleQuote |
					encoder.EncodeLtGt |
					encoder.EncodeInvalidUtf8),
			},
		},
		CommandHelp: []fs.CommandHelp{
			{
				Name:  "authorize",
				Short: "Authorize O2 Cloud via interactive browser login",
				Long: `Run this command to open a browser for O2 Cloud login and capture a session.

Usage:

  rclone backend authorize o2: <remote>

This will:
  1. Open a Chromium browser window for O2 Cloud login.
  2. Wait for you to complete the login (email + 2FA if needed).
  3. Capture the session and output the rclone config create command.

Run it once, then paste the output into your shell, or create the remote manually with the session fields.`,
			},
		},
	})
}

// Options defines the configuration for this backend
type Options struct {
	ValidationKey    string               `config:"validation_key"`
	OAuthBundle      string               `config:"oauth_bundle"`
	CookieJSessionID string               `config:"cookie_jsessionid"`
	DeviceID         string               `config:"device_id"`
	DeviceName       string               `config:"device_name"`
	UserAgent        string               `config:"user_agent"`
	EncryptionToken  string               `config:"encryption_token"`
	APIURL           string               `config:"api_url"`
	UploadURL        string               `config:"upload_url"`
	KeepaliveSeconds int                  `config:"keepalive_seconds"`
	PythonPath       string               `config:"python3"`
	LoginHelper      string               `config:"login_helper"`
	Enc              encoder.MultiEncoder `config:"encoding"`
}

// Fs is the interface a cloud storage system must provide
type Fs struct {
	name       string
	root       string
	opt        Options
	features   *fs.Features
	client     *Client
	dirCache   *dircache.DirCache
	pacer      *fs.Pacer
	httpClient *http.Client
	keepaliveCtx    context.Context
	keepaliveCancel context.CancelFunc
	rootID     string
}

// FindLeaf finds a directory of name leaf in the folder with ID pathID
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (pathIDOut string, found bool, err error) {
	items, err := f.client.listFolders(ctx, pathID)
	if err != nil {
		return "", false, err
	}
	for _, item := range items {
		if item.isFolder && item.name == leaf {
			return item.id, true, nil
		}
	}
	return "", false, nil
}

// CreateDir makes a directory with pathID as parent and name leaf
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	return f.client.createFolder(ctx, pathID, leaf)
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("O2 Cloud root '%s'", f.root)
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return time.Second
}

// Hashes returns the supported hash types of the filesystem
func (f *Fs) Hashes() hash.Set {
	var h hash.Set
	return h
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// NewFs makes a new Fs object from the path
func NewFs(ctx context.Context, name string, root string, config configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(config, opt); err != nil {
		return nil, err
	}

	root = strings.Trim(root, "/")

	httpClient := fshttp.NewClient(ctx)
	pacer := fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep), pacer.DecayConstant(decayConstant)))

	f := &Fs{
		name:       name,
		root:       root,
		opt:        *opt,
		pacer:      pacer,
		httpClient: httpClient,
	}

	f.features = (&fs.Features{
		DuplicateFiles:          false,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	f.client = newClient(f, httpClient)

	// get root folder ID
	rootID, err := f.client.getRootFolderID(ctx)
	if err != nil {
		return nil, fmt.Errorf("get root folder: %w", err)
	}
	f.rootID = rootID

	f.dirCache = dircache.New(root, rootID, f)

	// find root
	err = f.dirCache.FindRoot(ctx, false)
	if err != nil {
		// assume it is a file
		newRoot, remote := dircache.SplitPath(root)
		tempF := *f
		tempF.dirCache = dircache.New(newRoot, rootID, &tempF)
		tempF.root = newRoot
		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			// no root so return old f
			return f, nil
		}
		_, err := tempF.NewObject(ctx, remote)
		if err != nil {
			if err == fs.ErrorObjectNotFound {
				return f, nil
			}
			return nil, err
		}
		f.features.Fill(ctx, &tempF)
		f.dirCache = tempF.dirCache
		f.root = tempF.root
		return f, fs.ErrorIsFile
	}

	// start keepalive renewal
	if f.opt.KeepaliveSeconds > 0 {
		f.keepaliveCtx, f.keepaliveCancel = context.WithCancel(context.Background())
		go f.client.keepaliveRenewer(f.keepaliveCtx, f.keepaliveCancel)
	}

	return f, nil
}

// List the objects and directories in dir into entries
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return nil, fs.ErrorDirNotFound
		}
		return nil, err
	}

	items, err := f.client.listFolder(ctx, dirID)
	if err != nil {
		return nil, err
	}

	dirPath, _ := f.dirCache.GetInv(dirID)
	for _, item := range items {
		remote := dirPath
		if remote != "" {
			remote += "/"
		}
		remote += item.name

		if item.isFolder {
			dirRemote := dirPath
			if dirRemote != "" {
				dirRemote += "/"
			}
			dirRemote += item.name
			entries = append(entries, fs.NewDir(dirRemote, item.modTime))
		} else {
			entries = append(entries, &Object{
				f:        f,
				remote:   remote,
				id:       item.id,
				size:     item.size,
				modTime:  item.modTime,
				mediaKind: item.mediaKind,
				directURL: item.directURL,
				node:     item.node,
				token:    item.token,
				parentID: dirID,
			})
		}
	}
	return entries, nil
}

// NewObject finds the Object at remote
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	leaf, directoryID, err := f.dirCache.FindPath(ctx, remote, false)
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}

	items, err := f.client.listFiles(ctx, directoryID)
	if err != nil {
		return nil, err
	}

	for _, item := range items {
		if item.name == leaf {
			return &Object{
				f:        f,
				remote:   remote,
				id:       item.id,
				size:     item.size,
				modTime:  item.modTime,
				mediaKind: item.mediaKind,
				directURL: item.directURL,
				node:     item.node,
				token:    item.token,
				parentID: directoryID,
			}, nil
		}
	}
	return nil, fs.ErrorObjectNotFound
}

// Put in to the remote path with the modTime given of the given size
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	existingObj, err := f.NewObject(ctx, src.Remote())
	switch err {
	case nil:
		return existingObj, existingObj.Update(ctx, in, src, options...)
	case fs.ErrorObjectNotFound:
		// not found so create it
		leaf, dirID, err := f.dirCache.FindPath(ctx, src.Remote(), true)
		if err != nil {
			return nil, err
		}
		size := src.Size()
		if size < 0 {
			data, err := io.ReadAll(in)
			if err != nil {
				return nil, err
			}
			size = int64(len(data))
			in = strings.NewReader(string(data))
		}
		item, err := f.client.upload(ctx, dirID, leaf, size, src.ModTime(ctx), in, "")
		if err != nil {
			return nil, err
		}

		// build object from upload response directly
		obj := &Object{
			f:         f,
			remote:    src.Remote(),
			id:        item.id,
			size:      size,
			modTime:   src.ModTime(ctx),
			mediaKind: item.mediaKind,
			parentID:  dirID,
		}

		// wait for eventual consistency and verify
		f.dirCache.FlushDir(src.Remote())
		time.Sleep(2 * time.Second)
		for attempt := 0; attempt < 5; attempt++ {
			found, findErr := f.NewObject(ctx, src.Remote())
			if findErr == nil {
				return found, nil
			}
			time.Sleep(1 * time.Second)
		}

		// if verification fails, return the object from upload response
		return obj, nil
	default:
		return nil, err
	}
}

// Mkdir makes the directory
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

// Rmdir removes the directory if empty
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}
	items, err := f.client.listFolder(ctx, dirID)
	if err != nil {
		return err
	}
	if len(items) > 0 {
		return fmt.Errorf("directory not empty: %s", dir)
	}
	// softdelete the folder
	folderPath, _ := dircache.SplitPath(dir)
	parentPath, _ := dircache.SplitPath(folderPath)
	parentID, _ := f.dirCache.FindDir(ctx, parentPath, false)
	if parentID == "" {
		parentID = f.rootID
	}
	item := &o2Item{id: dirID, isFolder: true}
	err = f.client.deleteItem(ctx, item)
	if err != nil {
		return err
	}
	f.dirCache.FlushDir(dir)
	return nil
}

// Purge all files in the directory
func (f *Fs) Purge(ctx context.Context, dir string) error {
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}
	return f.purgeDir(ctx, dirID)
}

func (f *Fs) purgeDir(ctx context.Context, dirID string) error {
	items, err := f.client.listFolder(ctx, dirID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.isFolder {
			_ = f.purgeDir(ctx, item.id)
		}
		if err := f.client.deleteItem(ctx, &item); err != nil {
			_ = err
		}
	}
	return nil
}

// Move src to this remote using server-side move operations
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj := src.(*Object)

	leaf, dirID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}

	srcItem := &o2Item{
		id:       srcObj.id,
		name:     srcObj.remote,
		mediaKind: srcObj.mediaKind,
		isFolder: false,
	}

	if err := f.client.moveItem(ctx, srcItem, leaf, dirID); err != nil {
		return nil, err
	}

	f.dirCache.FlushDir(remote)
	return &Object{
		f:        f,
		remote:   remote,
		id:       srcObj.id,
		size:     srcObj.size,
		modTime:  srcObj.modTime,
		mediaKind: srcObj.mediaKind,
		parentID: dirID,
	}, nil
}

// DirMove moves src, srcRemote to this remote at dstRemote
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs := src.(*Fs)
	srcDirID, err := srcFs.dirCache.FindDir(ctx, srcRemote, false)
	if err != nil {
		return err
	}

	leaf, dstDirID, err := f.dirCache.FindPath(ctx, dstRemote, true)
	if err != nil {
		return err
	}

	srcItem := &o2Item{id: srcDirID, isFolder: true}
	return f.client.moveItem(ctx, srcItem, leaf, dstDirID)
}

// About gets quota information from the Fs
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	used, total, err := f.client.getQuota(ctx)
	if err != nil {
		return nil, err
	}
	usedVal := used
	totalVal := total
	freeVal := total - used
	return &fs.Usage{
		Used:  &usedVal,
		Total: &totalVal,
		Free:  &freeVal,
	}, nil
}

// Shutdown closes the keepalive goroutine
func (f *Fs) Shutdown(ctx context.Context) error {
	if f.keepaliveCancel != nil {
		f.keepaliveCancel()
	}
	return nil
}

// DirCacheFlush resets the directory cache
func (f *Fs) DirCacheFlush() {
	f.dirCache.Flush()
}

// Command runs a named backend command
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (any, error) {
	if name == "authorize" {
		return f.runAuthorize(ctx, arg)
	}
	return nil, fs.ErrorCommandNotFound
}

// Object describes an O2 Cloud file object
type Object struct {
	f        *Fs
	remote   string
	id       string
	size     int64
	modTime  time.Time
	mediaKind string
	directURL string
	node     string
	token    string
	parentID string
}

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.f
}

// String returns a string representation
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// ModTime returns the modification date
func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.modTime
}

// Size returns the size of the file
func (o *Object) Size() int64 {
	return o.size
}

// Hash returns the checksum of the file
func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

// Storable says whether this object can be stored
func (o *Object) Storable() bool {
	return true
}

// SetModTime sets the modification date
func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	item := &o2Item{id: o.id, name: o.remote, mediaKind: o.mediaKind, isFolder: false}
	return o.f.client.updateModTime(ctx, item, t)
}

// Open opens the file for read
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	url, err := o.f.client.resolveDownloadURL(ctx, &o2Item{id: o.id, name: o.remote, mediaKind: o.mediaKind, directURL: o.directURL})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	for k, v := range o.f.client.baseHeaders() {
		req.Header.Set(k, v)
	}

	for _, option := range options {
		switch opt := option.(type) {
		case *fs.RangeOption:
			key, value := opt.Header()
			req.Header.Set(key, value)
		case *fs.HTTPOption:
			key, value := opt.Header()
			req.Header.Set(key, value)
		}
	}

	resp, err := o.f.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == 416 {
		resp.Body.Close()
		return nil, fmt.Errorf("range not satisfiable: %s", req.Header.Get("Range"))
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	return resp.Body, nil
}

// Update in to the object with the modTime given of the given size
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	leaf, dirID, err := o.f.dirCache.FindPath(ctx, o.remote, true)
	if err != nil {
		return err
	}
	size := src.Size()
	if size < 0 {
		data, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		size = int64(len(data))
		in = strings.NewReader(string(data))
	}

	mediaID := o.id
	item, err := o.f.client.upload(ctx, dirID, leaf, size, src.ModTime(ctx), in, mediaID)
	if err != nil {
		return err
	}
	o.id = item.id
	o.size = size
	o.modTime = src.ModTime(ctx)
	o.directURL = ""
	o.f.dirCache.FlushDir(o.remote)
	return nil
}

// Remove removes this object
func (o *Object) Remove(ctx context.Context) error {
	item := &o2Item{id: o.id, name: o.remote, mediaKind: o.mediaKind, isFolder: false}
	return o.f.client.deleteItem(ctx, item)
}

func parseMediaItem(raw []byte, folderID, mediaServer string) *o2Item {
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil
	}

	// check if it looks like a folder
	if typ, ok := item["type"].(string); ok && strings.Contains(strings.ToLower(typ), "folder") {
		return nil
	}

	id := ""
	for _, key := range []string{"id", "mediaid", "mediaId", "fdoid", "uuid"} {
		if v, ok := item[key].(string); ok && v != "" {
			id = v
			break
		}
	}

	name := ""
	for _, key := range []string{"name", "filename", "title"} {
		if v, ok := item[key].(string); ok && v != "" {
			name = v
			break
		}
	}

	if id == "" || name == "" {
		return nil
	}

	size := int64(0)
	for _, key := range []string{"size", "filesize", "fileSize", "contentlength", "contentLength"} {
		switch v := item[key].(type) {
		case float64:
			size = int64(v)
		case int64:
			size = v
		case string:
			if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
				size = parsed
			}
		}
		if size > 0 {
			break
		}
	}

	mediaKind := ""
	rawType := ""
	for _, key := range []string{"type", "mediatype", "mimetype", "contenttype"} {
		if v, ok := item[key].(string); ok && v != "" {
			rawType = v
			break
		}
	}

	modTime := time.Now()
	for _, key := range []string{"modificationdate", "creationdate", "uploaded", "date"} {
		if v, ok := item[key].(string); ok && v != "" {
			modTime = parseTime(v)
			break
		}
	}

	directURL := ""
	for _, key := range []string{"url", "downloadurl"} {
		if v, ok := item[key].(string); ok && v != "" {
			if !strings.HasPrefix(v, "http") {
				directURL = mediaServer + v
			} else {
				directURL = v
			}
			break
		}
	}

	mediaKind = mediaKindForName(name)
	if rawType != "" && strings.Contains(rawType, "video") {
		mediaKind = "video"
	} else if rawType != "" && strings.Contains(rawType, "audio") {
		mediaKind = "audio"
	} else if rawType != "" && strings.Contains(rawType, "image") {
		mediaKind = "picture"
	}

	return &o2Item{
		id:        id,
		name:      name,
		folderID:  folderID,
		isFolder:  false,
		size:      size,
		modTime:   modTime,
		mediaKind: mediaKind,
		directURL: directURL,
	}
}

func Config(ctx context.Context, name string, m configmap.Mapper, conf fs.ConfigIn) (*fs.ConfigOut, error) {
	switch conf.State {
	case "":
		// ask user how they want to get the session
		return &fs.ConfigOut{
			State: "get_session",
			Option: &fs.Option{
				Name:     "session_source",
				Help:     "How do you want to provide the O2 Cloud session?",
				Default:  "paste",
				Required: true,
				Examples: fs.OptionExamples{
					{Value: "paste", Help: "I already have a session JSON (from rclone backend authorize o2:)"},
					{Value: "helper", Help: "Run the browser login helper now (opens Chromium)"},
				},
			},
		}, nil
	case "get_session":
		source := conf.Result
		if source == "helper" {
			return &fs.ConfigOut{
				State: "run_helper",
			}, nil
		}
		return &fs.ConfigOut{
			State: "paste_json",
		}, nil
	case "run_helper":
		pythonPath, _ := m.Get("python3")
		if pythonPath == "" {
			pythonPath = "python3"
		}
		helperPath, _ := m.Get("login_helper")

		return &fs.ConfigOut{
			State: "paste_json",
			Error: fmt.Sprintf("Run this command in another terminal to capture the session:\n\n  %s %s\n\nThen paste the JSON output below.", pythonPath, helperPath),
		}, nil
	case "paste_json":
		return &fs.ConfigOut{
			State: "session_json",
			Option: &fs.Option{
				Name:      "session_json",
				Help:      "Paste the session JSON output from the authorize helper:",
				Required:  true,
			},
		}, nil
	case "session_json":
		var session struct {
			ValidationKey    string `json:"validationKey"`
			OAuthBundle      string `json:"oauthBundle"`
			CookieJSessionID string `json:"jsessionid"`
			DeviceID         string `json:"deviceId"`
			DeviceName       string `json:"deviceName"`
			UserAgent        string `json:"userAgent"`
			EncryptionToken  string `json:"encryptionToken"`
		}
		if err := json.Unmarshal([]byte(conf.Result), &session); err != nil {
			return &fs.ConfigOut{
				State: "paste_json",
				Error: fmt.Sprintf("Invalid JSON: %v", err),
			}, nil
		}

		if session.ValidationKey == "" || session.OAuthBundle == "" {
			return &fs.ConfigOut{
				State: "paste_json",
				Error: "JSON must contain validationKey and oauthBundle",
			}, nil
		}

		m.Set("validation_key", session.ValidationKey)
		m.Set("oauth_bundle", session.OAuthBundle)
		m.Set("cookie_jsessionid", session.CookieJSessionID)
		m.Set("device_id", session.DeviceID)
		m.Set("device_name", session.DeviceName)
		m.Set("user_agent", session.UserAgent)
		m.Set("encryption_token", session.EncryptionToken)

		return nil, nil
	}
	return nil, fmt.Errorf("unknown state: %s", conf.State)
}
