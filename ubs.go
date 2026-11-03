package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"openpak/np-common/resource"
	"openpak/np-common/ticket"
)

// Wire format: docs/protocol.md (static RE of VSH 4.93, research note ps3/ubs-protocol.md).
const (
	resultOK          = "00"
	resultBadTicket   = "02"
	resultInternal    = "07"
	resultTooLarge    = "1a"
	resultNoData      = "4a"
	resultFull        = "5f" // compared by the save-data plugin; meaning inferred
	resultUnspecified = "ff"

	maxXML   = 64 << 10
	maxIcon  = 4 << 20
	maxPage  = 50 // VSH rejects max > 50 before sending
	dateFmt  = "2006-01-02T15:04:05.000Z"
	sep      = "\r\n\r\n"
	infoHdr  = "Sce-Np-Info-Length"
	MaxSaves = 1000 // per account, all folders

	transferBudget = 30 * time.Minute
)

var (
	namePattern = regexp.MustCompile(`^[0-9A-Za-z_.\-]{1,31}$`)
	folders     = map[string]bool{"ps1": true, "ps2": true, "ps3": true, "psp": true, "minis": true}
)

type Service struct {
	Core        ticket.AccountCore
	Tickets     *ticket.Signer
	Store       Store
	ResourceKey []byte
	FQDN        string // ubstorage-fqdn in the discovery file
	IconBase    string // origin for icon-url, e.g. http://ubs.np.community.playstation.net
	SaveMax     int64  // bytes per save (the allowance itself is the saves service's)
	Log         *slog.Logger
}

func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /np/resource/ubstorage/ubstorage.xml", s.discovery)
	mux.HandleFunc("GET /ubs/icon/{id}", s.icon)
	mux.HandleFunc("POST /ubs/sec/is_supported_title", s.isSupported)
	for op, h := range map[string]func(*call){
		"set_save_data":           s.set,
		"get_save_data":           s.get,
		"get_save_data_info_list": s.list,
		"delete_save_data":        s.remove,
		"notify_downloaded":       s.notify,
	} {
		mux.HandleFunc("POST /ubs/sec/"+op, s.handle(h))
	}
}

func (s *Service) discovery(w http.ResponseWriter, _ *http.Request) {
	body := resource.Sign([]byte(`<?xml version="1.0" encoding="utf-8"?><ubstorage status="alive"><ubstorage-fqdn>`+
		esc(s.FQDN)+`</ubstorage-fqdn></ubstorage>`), s.ResourceKey)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

func (s *Service) icon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) != 32 {
		http.NotFound(w, r)
		return
	}
	b, err := s.Store.Icon(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Write(b)
}

type att struct {
	Name   string `xml:"name,attr"`
	Length int64  `xml:"length,attr"`
	MD5    string `xml:"md5sum,attr"`
}

type info struct {
	OwnerInfo int    `xml:"owner-info"`
	Title     string `xml:"title"`
	Subtitle  string `xml:"subtitle"`
	Updated   string `xml:"updated"`
	Lock      int    `xml:"lock"`
	Size      int64  `xml:"size"`
	Details   string `xml:"details"`
}

type data struct {
	Name string `xml:"name,attr"`
	Info *info  `xml:"info"`
	Atts []att  `xml:"atts>att"`
}

type request struct {
	XMLName   xml.Name `xml:"ubstorage"`
	Ticket    string   `xml:"ticket"`
	TicketJID string   `xml:"ticketjid"`
	Data      *data    `xml:"data"` // is_supported_title only
	Folder    *struct {
		Type  string `xml:"type,attr"`
		Seq   string `xml:"seq,attr"`
		Start string `xml:"start,attr"`
		Max   string `xml:"max,attr"`
		Data  *data  `xml:"data"`
	} `xml:"folder"`
}

type call struct {
	w     http.ResponseWriter
	r     *http.Request
	body  *bufio.Reader // what follows the XML and its separator
	req   request
	owner string
	typ   string
}

// readXML returns the XML part of a request. VSH sends Sce-Np-Info-Length with the XML size; without
// it the XML runs up to the first CRLFCRLF or the end of the body.
func readXML(r *http.Request, br *bufio.Reader) ([]byte, error) {
	if n, err := strconv.Atoi(r.Header.Get(infoHdr)); err == nil {
		if n <= 0 || n > maxXML {
			return nil, errors.New("bad info length")
		}
		head := make([]byte, n)
		if _, err := io.ReadFull(br, head); err != nil {
			return nil, err
		}
		if p, _ := br.Peek(len(sep)); string(p) == sep {
			br.Discard(len(sep))
		}
		return head, nil
	}
	var head []byte
	for len(head) <= maxXML+len(sep) {
		b, err := br.ReadByte()
		if err == io.EOF {
			return head, nil
		}
		if err != nil {
			return nil, err
		}
		head = append(head, b)
		if bytes.HasSuffix(head, []byte(sep)) {
			return head[:len(head)-len(sep)], nil
		}
	}
	return nil, errors.New("xml too large")
}

func (s *Service) handle(h func(*call)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// A save can be far larger than the server's 60 s read/write budget allows over a PS3 link.
		rc := http.NewResponseController(w)
		rc.SetReadDeadline(time.Now().Add(transferBudget))
		rc.SetWriteDeadline(time.Now().Add(transferBudget))
		c := &call{w: w, r: r, body: bufio.NewReaderSize(r.Body, 64<<10)}
		head, err := readXML(r, c.body)
		if err != nil || xml.Unmarshal(head, &c.req) != nil || c.req.Folder == nil || !folders[c.req.Folder.Type] {
			reply(w, resultUnspecified, "", 0, nil)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(c.req.Ticket), ""))
		var id ticket.Identity
		if err == nil {
			id, err = ticket.VerifyAccount(r.Context(), s.Core, s.Tickets, raw)
		}
		if err != nil {
			reply(w, resultBadTicket, "", 0, nil)
			return
		}
		c.owner, c.typ = id.AccountID, c.req.Folder.Type
		h(c)
	}
}

// name is the save directory named by the request, or "" (after replying) when it is missing or bad.
func (c *call) name() string {
	d := c.req.Folder.Data
	if d == nil || !namePattern.MatchString(d.Name) {
		reply(c.w, resultUnspecified, "", 0, nil)
		return ""
	}
	return d.Name
}

func (s *Service) fail(c *call, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		reply(c.w, resultNoData, "", 0, nil)
	case errors.Is(err, ErrFull):
		reply(c.w, resultFull, "", 0, nil)
	default:
		s.Log.Warn("ubs store", "err", err)
		reply(c.w, resultInternal, "", 0, nil)
	}
}

// head renders the storage-info and folder blocks; extra goes inside the folder element.
func (s *Service) head(c *call, folderAttrs, extra string) (string, error) {
	p, err := s.Store.List(c.r.Context(), c.owner, c.typ, 0, 1)
	if err != nil {
		return "", err
	}
	return s.headOf(c, p, folderAttrs, extra), nil
}

func (s *Service) headOf(c *call, p Page, folderAttrs, extra string) string {
	return fmt.Sprintf(`<storage-info quotasize="%d" totalsize="%d"/><folder type="%s" total="%d" totalsize="%d" seq="%d"%s>%s</folder>`,
		p.Quota, p.Used, c.typ, p.Info.Total, p.Info.TotalSize, p.Info.Seq, folderAttrs, extra)
}

func (s *Service) record(sv Save) string {
	now := time.Now().UTC().Format(dateFmt)
	icon := ""
	if sv.IconID != "" {
		icon = `<icon-url>` + esc(strings.TrimRight(s.IconBase, "/")+"/ubs/icon/"+sv.IconID) + `</icon-url>`
	}
	return fmt.Sprintf(`<data name="%s"><length>%d</length><date>%s</date><info><owner-info>%d</owner-info><title>%s</title>`+
		`<subtitle>%s</subtitle><updated>%s</updated><lock>%d</lock><size>%d</size><details>%s</details>%s`+
		`<downloadable-date>%s</downloadable-date></info></data>`,
		esc(sv.Name), sv.Length, sv.Date.UTC().Format(dateFmt), sv.OwnerInfo, esc(sv.Title), esc(sv.Subtitle),
		sv.Updated.UTC().Format(dateFmt), sv.Lock, sv.Size, esc(sv.Details), icon, now)
}

func (s *Service) isSupported(w http.ResponseWriter, r *http.Request) {
	var req request
	head, err := readXML(r, bufio.NewReader(r.Body))
	if err != nil || xml.Unmarshal(head, &req) != nil || req.Data == nil || req.Data.Name == "" {
		reply(w, resultUnspecified, "", 0, nil)
		return
	}
	// Every title may use online storage; 2 would mean "not supported".
	reply(w, resultOK, `<supported>1</supported>`, 0, nil)
}

func (s *Service) list(c *call) {
	f := c.req.Folder
	start, _ := strconv.Atoi(f.Start)
	limit, err := strconv.Atoi(f.Max)
	if err != nil || limit <= 0 || limit > maxPage {
		limit = maxPage
	}
	// The start base is not visible in the firmware: 1-based when ≥ 1, 0 = first entry too.
	offset := max(start-1, 0)
	page, err := s.Store.List(c.r.Context(), c.owner, c.typ, offset, limit)
	if err != nil {
		s.fail(c, err)
		return
	}
	var b strings.Builder
	for _, sv := range page.Saves {
		b.WriteString(s.record(sv))
	}
	reply(c.w, resultOK, s.headOf(c, page, fmt.Sprintf(` start="%d" results="%d"`, start, len(page.Saves)), b.String()), 0, nil)
}

func (s *Service) set(c *call) {
	name := c.name()
	if name == "" {
		return
	}
	d := c.req.Folder.Data
	var icon, dat *att
	for i := range d.Atts {
		switch d.Atts[i].Name {
		case "icon":
			icon = &d.Atts[i]
		case "data":
			dat = &d.Atts[i]
		}
	}
	if d.Info == nil || dat == nil || dat.Length < 0 || (icon != nil && (icon.Length < 0 || icon.Length > maxIcon)) {
		reply(c.w, resultUnspecified, "", 0, nil)
		return
	}
	if dat.Length > s.SaveMax {
		reply(c.w, resultTooLarge, "", 0, nil)
		return
	}
	ctx := c.r.Context()
	// A new name counts against the per-account cap; the allowance itself is checked by saves.
	if _, err := s.Store.Get(ctx, c.owner, c.typ, name); errors.Is(err, ErrNotFound) {
		p, err := s.Store.List(ctx, c.owner, c.typ, 0, 1)
		if err != nil {
			s.fail(c, err)
			return
		}
		if p.PlatformCount >= MaxSaves {
			reply(c.w, resultFull, "", 0, nil)
			return
		}
	} else if err != nil {
		s.fail(c, err)
		return
	}
	sv := Save{Owner: c.owner, Folder: c.typ, Name: name, OwnerInfo: d.Info.OwnerInfo, Title: d.Info.Title, Subtitle: d.Info.Subtitle,
		Details: d.Info.Details, Lock: d.Info.Lock, Size: d.Info.Size}
	sv.Updated, _ = time.Parse(time.RFC3339Nano, strings.TrimSpace(d.Info.Updated))
	if sv.Updated.IsZero() {
		sv.Updated = time.Now().UTC()
	}
	var iconBytes []byte
	var iconLen int64
	iconMD5 := ""
	if icon != nil {
		iconBytes = make([]byte, icon.Length)
		if _, err := io.ReadFull(c.body, iconBytes); err != nil || !md5ok(icon.MD5, md5sum(iconBytes)) {
			reply(c.w, resultUnspecified, "", 0, nil)
			return
		}
		if p, _ := c.body.Peek(len(sep)); string(p) == sep {
			c.body.Discard(len(sep))
		}
		iconLen, iconMD5 = icon.Length, icon.MD5
	}
	// The data streams straight through to saves; it never sits in memory here.
	cr := &countReader{r: io.LimitReader(c.body, dat.Length)}
	stored, err := s.Store.Put(ctx, sv, iconLen, dat.Length, iconMD5, dat.MD5, io.MultiReader(bytes.NewReader(iconBytes), cr))
	switch {
	case errors.Is(err, ErrBadData), err != nil && cr.n < dat.Length:
		reply(c.w, resultUnspecified, "", 0, nil)
		return
	case err != nil:
		s.fail(c, err)
		return
	}
	hd, err := s.head(c, "", s.record(stored))
	if err != nil {
		s.fail(c, err)
		return
	}
	reply(c.w, resultOK, hd, 0, nil)
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func md5sum(b []byte) hash.Hash {
	h := md5.New()
	h.Write(b)
	return h
}

// md5ok accepts an absent md5sum and compares case-insensitively otherwise.
func md5ok(want string, h hash.Hash) bool {
	return want == "" || strings.EqualFold(want, hex.EncodeToString(h.Sum(nil)))
}

func (s *Service) get(c *call) {
	name := c.name()
	if name == "" {
		return
	}
	sv, err := s.Store.Get(c.r.Context(), c.owner, c.typ, name)
	if err != nil {
		s.fail(c, err)
		return
	}
	hd, err := s.head(c, "", s.record(sv))
	if err != nil {
		s.fail(c, err)
		return
	}
	reply(c.w, resultOK, hd, sv.Length, func(w io.Writer) error {
		rc, _, err := s.Store.Open(c.r.Context(), c.owner, c.typ, name)
		if err != nil {
			return err
		}
		defer rc.Close()
		n, err := io.Copy(w, rc)
		if err == nil && n != sv.Length {
			err = io.ErrUnexpectedEOF
		}
		return err
	})
}

func (s *Service) remove(c *call) {
	name := c.name()
	if name == "" {
		return
	}
	if err := s.Store.Delete(c.r.Context(), c.owner, c.typ, name); err != nil {
		s.fail(c, err)
		return
	}
	hd, err := s.head(c, "", "")
	if err != nil {
		s.fail(c, err)
		return
	}
	reply(c.w, resultOK, hd, 0, nil)
}

// notify follows a finished download. Sony used downloadable-date for a once-per-day download
// limit; OpenPak has none, so the save is downloadable again at once.
func (s *Service) notify(c *call) {
	name := c.name()
	if name == "" {
		return
	}
	sv, err := s.Store.Get(c.r.Context(), c.owner, c.typ, name)
	if err != nil {
		s.fail(c, err)
		return
	}
	hd, err := s.head(c, "", s.record(sv))
	if err != nil {
		s.fail(c, err)
		return
	}
	reply(c.w, resultOK, hd, 0, nil)
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// reply answers HTTP 200 with a Content-Length. With a data writer the body is XML, CRLFCRLF and
// n data bytes, and Sce-Np-Info-Length gives the XML size (VSH needs it to find the data).
func reply(w http.ResponseWriter, result, inner string, n int64, data func(io.Writer) error) {
	b := []byte(`<?xml version="1.0" encoding="utf-8"?><ubstorage result="` + result + `">` + inner + `</ubstorage>`)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if data == nil {
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
		return
	}
	w.Header().Set(infoHdr, strconv.Itoa(len(b)))
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(b)+len(sep))+n, 10))
	w.Write(append(b, sep...))
	if err := data(w); err != nil {
		slog.Warn("ubs download aborted", "err", err)
		panic(http.ErrAbortHandler)
	}
}
