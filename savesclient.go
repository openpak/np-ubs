package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Save is one save-data directory in one of an account's folders (ps1, ps2, ps3, psp, minis).
// The bytes and the index live in the core saves service; everything here but Owner, Folder and
// Name rides in the blob's opaque meta.
type Save struct {
	Owner     string // core account id
	Folder    string
	Name      string // save directory name, < 32 chars
	OwnerInfo int
	Title     string
	Subtitle  string
	Details   string
	Updated   time.Time
	Lock      int
	Size      int64 // save size as the console reports it
	Length    int64 // bytes of the uploaded data part
	IconID    string
	Date      time.Time // upload time
}

// FolderInfo is the per-folder summary every reply carries.
type FolderInfo struct {
	Total     int
	TotalSize int64
	Seq       int64
}

// Page is one list call: the saves, the folder summary and the account's allowance.
type Page struct {
	Saves         []Save
	Info          FolderInfo
	Used, Quota   int64
	PlatformCount int // the account's PS3-family saves across all folders
}

var (
	ErrNotFound = errors.New("no such save")
	ErrFull     = errors.New("account allowance exceeded")
	ErrBadData  = errors.New("data does not match its md5 or length")
)

// Store is the core saves service as np-ubs uses it.
type Store interface {
	List(ctx context.Context, owner, folder string, offset, limit int) (Page, error)
	Get(ctx context.Context, owner, folder, name string) (Save, error)
	// Put streams icon bytes then data bytes (iconLen + dataLen of them) and returns the stored save.
	Put(ctx context.Context, s Save, iconLen, dataLen int64, iconMD5, dataMD5 string, body io.Reader) (Save, error)
	Open(ctx context.Context, owner, folder, name string) (io.ReadCloser, Save, error)
	Delete(ctx context.Context, owner, folder, name string) error
	Icon(ctx context.Context, id string) ([]byte, error)
}

// platform is the saves platform every PS3-family folder lives under; the folder type is the title.
const platform = "ps3"

// SavesClient talks to the saves service's neutral blob routes (docs/protocol.md there).
type SavesClient struct {
	URL, Key string
	HTTP     *http.Client
}

func NewSavesClient(base, key string) *SavesClient {
	// No overall timeout: a save streams for as long as the console takes; contexts bound it.
	return &SavesClient{URL: strings.TrimRight(base, "/"), Key: key, HTTP: &http.Client{}}
}

type wireInfo struct {
	Name   string         `json:"name"`
	Size   int64          `json:"size"`
	IconID string         `json:"icon_id"`
	Date   time.Time      `json:"date"`
	Meta   map[string]any `json:"meta"`
	Seq    int64          `json:"seq"`
}

func (c *SavesClient) path(owner, folder, name string) string {
	p := c.URL + "/internal/accounts/" + url.PathEscape(owner) + "/blobs/" + platform + "/" + url.PathEscape(folder)
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func (c *SavesClient) do(ctx context.Context, method, u string, body io.Reader, length int64, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = length
	}
	req.Header.Set("X-Internal-Key", c.Key)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return c.HTTP.Do(req)
}

// status maps a saves error answer.
func status(res *http.Response) error {
	if res.StatusCode/100 == 2 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	switch res.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusInsufficientStorage:
		return ErrFull
	case http.StatusUnprocessableEntity:
		return ErrBadData
	}
	return fmt.Errorf("saves: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
}

func (w wireInfo) save(owner, folder string) Save {
	s := Save{Owner: owner, Folder: folder, Name: w.Name, Length: w.Size, IconID: w.IconID, Date: w.Date}
	m := w.Meta
	s.OwnerInfo, s.Lock = int(num(m["owner_info"])), int(num(m["lock"]))
	s.Size = num(m["size"])
	s.Title, _ = m["title"].(string)
	s.Subtitle, _ = m["subtitle"].(string)
	s.Details, _ = m["details"].(string)
	if u, _ := m["updated"].(string); u != "" {
		s.Updated, _ = time.Parse(time.RFC3339Nano, u)
	}
	if s.Updated.IsZero() {
		s.Updated = s.Date
	}
	return s
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func metaOf(s Save) map[string]any {
	return map[string]any{"owner_info": s.OwnerInfo, "title": s.Title, "subtitle": s.Subtitle, "details": s.Details,
		"updated": s.Updated.UTC().Format(time.RFC3339Nano), "lock": s.Lock, "size": s.Size}
}

func (c *SavesClient) List(ctx context.Context, owner, folder string, offset, limit int) (Page, error) {
	res, err := c.do(ctx, "GET", c.path(owner, folder, "")+"?offset="+strconv.Itoa(offset)+"&limit="+strconv.Itoa(limit), nil, 0, nil)
	if err != nil {
		return Page{}, err
	}
	defer res.Body.Close()
	if err := status(res); err != nil {
		return Page{}, err
	}
	var out struct {
		Items         []wireInfo `json:"items"`
		Total         int        `json:"total"`
		TotalSize     int64      `json:"total_size"`
		Seq           int64      `json:"seq"`
		PlatformCount int        `json:"platform_count"`
		Usage         struct {
			Used  int64 `json:"allowance_used"`
			Quota int64 `json:"allowance"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return Page{}, err
	}
	p := Page{Info: FolderInfo{Total: out.Total, TotalSize: out.TotalSize, Seq: out.Seq}, Used: out.Usage.Used, Quota: out.Usage.Quota, PlatformCount: out.PlatformCount}
	for _, w := range out.Items {
		p.Saves = append(p.Saves, w.save(owner, folder))
	}
	return p, nil
}

func (c *SavesClient) Get(ctx context.Context, owner, folder, name string) (Save, error) {
	res, err := c.do(ctx, "GET", c.path(owner, folder, name)+"/info", nil, 0, nil)
	if err != nil {
		return Save{}, err
	}
	defer res.Body.Close()
	if err := status(res); err != nil {
		return Save{}, err
	}
	var w wireInfo
	if err := json.NewDecoder(res.Body).Decode(&w); err != nil {
		return Save{}, err
	}
	return w.save(owner, folder), nil
}

func (c *SavesClient) Put(ctx context.Context, s Save, iconLen, dataLen int64, iconMD5, dataMD5 string, body io.Reader) (Save, error) {
	meta, _ := json.Marshal(metaOf(s))
	hdr := map[string]string{"X-Icon-Length": strconv.FormatInt(iconLen, 10), "X-Label": "PS3",
		"X-Meta": base64.StdEncoding.EncodeToString(meta), "Content-Type": "application/octet-stream"}
	if iconMD5 != "" {
		hdr["X-Icon-Md5"] = iconMD5
	}
	if dataMD5 != "" {
		hdr["X-Data-Md5"] = dataMD5
	}
	res, err := c.do(ctx, "PUT", c.path(s.Owner, s.Folder, s.Name), body, iconLen+dataLen, hdr)
	if err != nil {
		return Save{}, err
	}
	defer res.Body.Close()
	if err := status(res); err != nil {
		return Save{}, err
	}
	var w wireInfo
	if err := json.NewDecoder(res.Body).Decode(&w); err != nil {
		return Save{}, err
	}
	return w.save(s.Owner, s.Folder), nil
}

func (c *SavesClient) Open(ctx context.Context, owner, folder, name string) (io.ReadCloser, Save, error) {
	res, err := c.do(ctx, "GET", c.path(owner, folder, name), nil, 0, nil)
	if err != nil {
		return nil, Save{}, err
	}
	if err := status(res); err != nil {
		res.Body.Close()
		return nil, Save{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(res.Header.Get("X-Blob-Info"))
	var w wireInfo
	if err == nil {
		err = json.Unmarshal(raw, &w)
	}
	if err != nil {
		res.Body.Close()
		return nil, Save{}, fmt.Errorf("saves: bad X-Blob-Info: %w", err)
	}
	return res.Body, w.save(owner, folder), nil
}

func (c *SavesClient) Delete(ctx context.Context, owner, folder, name string) error {
	res, err := c.do(ctx, "DELETE", c.path(owner, folder, name), nil, 0, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return status(res)
}

func (c *SavesClient) Icon(ctx context.Context, id string) ([]byte, error) {
	res, err := c.do(ctx, "GET", c.URL+"/internal/blobs/icons/"+url.PathEscape(id), nil, 0, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if err := status(res); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(res.Body, maxIcon+1))
}
