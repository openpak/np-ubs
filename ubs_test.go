package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"openpak/np-common/coreclient"
	"openpak/np-common/fakecore"
	"openpak/np-common/identity"
	"openpak/np-common/resource"
	"openpak/np-common/ticket"
)

var resourceKey = bytes.Repeat([]byte{0x5a}, 20)

type user struct {
	raw []byte
	jid string
}

func setup(t *testing.T, store Store, saveMax int64) (*httptest.Server, map[string]user) {
	const coreKey = "test-core-key-at-least-thirty-two-bytes"
	fake := fakecore.New(coreKey)
	fake.AddAccount(fakecore.Account{ID: "alice", DisplayName: "Alice", Country: "PT"})
	fake.AddAccount(fakecore.Account{ID: "bob", DisplayName: "Bobby", Country: "US"})
	core, err := coreclient.Dial("passthrough:///core", coreKey, fake.Serve(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.Close() })
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ticket.New(key, nil)
	users := map[string]user{}
	for _, id := range []string{"alice", "bob"} {
		acct, _ := core.GetAccount(context.Background(), id)
		l, err := identity.EnsureNPLink(context.Background(), core, acct)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := signer.Issue(ticket.Identity{AccountID: id, LinkID: l.LinkID, OnlineID: l.OnlineID, Country: "us", Service: "IV0001-NPXS01001_00"}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		users[id] = user{raw, l.OnlineID + "@us.np.community.playstation.net"}
	}
	mux := http.NewServeMux()
	(&Service{Core: core, Tickets: signer, Store: store, ResourceKey: resourceKey, FQDN: "ubs.example", IconBase: "http://ubs.example",
		SaveMax: saveMax, Log: slog.Default()}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, users
}

type resp struct {
	xml, data string
	hdr       http.Header
}

// post sends a request the way VSH does: XML, Sce-Np-Info-Length, then CRLFCRLF and parts.
func post(t *testing.T, srv *httptest.Server, op string, u *user, inner string, parts ...[]byte) resp {
	t.Helper()
	x := `<?xml version="1.0" encoding="utf-8"?><ubstorage platform="ps3" sv="4.93">`
	if u != nil {
		x += `<ticket>` + base64.StdEncoding.EncodeToString(u.raw) + `</ticket><ticketjid>` + u.jid + `</ticketjid><psid type="open">` +
			strings.Repeat("0", 32) + `</psid>`
	}
	x += inner + `</ubstorage>`
	b := []byte(x)
	for _, p := range parts {
		b = append(append(b, sep...), p...)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/ubs/sec/"+op, bytes.NewReader(b))
	req.Header.Set(infoHdr, strconv.Itoa(len(x)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || res.Header.Get("Content-Length") != strconv.Itoa(len(out)) {
		t.Fatalf("%s: HTTP %d, Content-Length %q for %d bytes", op, res.StatusCode, res.Header.Get("Content-Length"), len(out))
	}
	r := resp{xml: string(out), hdr: res.Header}
	if n, err := strconv.Atoi(res.Header.Get(infoHdr)); err == nil {
		if string(out[n:n+4]) != sep {
			t.Fatalf("%s: no CRLFCRLF after %d XML bytes", op, n)
		}
		r.xml, r.data = string(out[:n]), string(out[n+4:])
	}
	return r
}

func md5hex(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) }

func setXML(name string, icon, data []byte, dataMD5 string) string {
	atts := ""
	if icon != nil {
		atts += fmt.Sprintf(`<att name="icon" length="%d" md5sum="%s"/>`, len(icon), md5hex(icon))
	}
	atts += fmt.Sprintf(`<att name="data" length="%d" md5sum="%s"/>`, len(data), dataMD5)
	return `<folder type="ps3" seq="0"><data name="` + name + `"><info><owner-info>1</owner-info><title>Game &amp; Co</title>` +
		`<subtitle>Slot 1</subtitle><updated>2026-11-03T10:00:00.00Z</updated><lock>0</lock><size>` + strconv.Itoa(len(data)*2) +
		`</size><details>Chapter 3</details></info><atts>` + atts + `</atts></data></folder>`
}

func upload(t *testing.T, srv *httptest.Server, u *user, name string, icon, data []byte) resp {
	if icon != nil {
		return post(t, srv, "set_save_data", u, setXML(name, icon, data, md5hex(data)), icon, data)
	}
	return post(t, srv, "set_save_data", u, setXML(name, nil, data, md5hex(data)), data)
}

func want(t *testing.T, r resp, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(r.xml, s) {
			t.Fatalf("missing %q in %s", s, r.xml)
		}
	}
}

func TestScenario(t *testing.T) {
	fake := newFakeSaves(1 << 20)
	fs := httptest.NewServer(fake)
	t.Cleanup(fs.Close)
	srv, users := setup(t, NewSavesClient(fs.URL, fake.key), 1<<19)
	alice, bob := users["alice"], users["bob"]

	// Discovery: signed, names our host.
	res, err := http.Get(srv.URL + "/np/resource/ubstorage/ubstorage.xml")
	if err != nil {
		t.Fatal(err)
	}
	disc, _ := io.ReadAll(res.Body)
	res.Body.Close()
	i := bytes.IndexByte(disc, '\n')
	if !bytes.Equal(disc, resource.Sign(disc[i+1:], resourceKey)) || !bytes.Contains(disc, []byte(`<ubstorage status="alive"><ubstorage-fqdn>ubs.example</ubstorage-fqdn>`)) {
		t.Fatalf("discovery: %s", disc)
	}

	want(t, post(t, srv, "is_supported_title", nil, `<data name="BLUS30001"/>`), `result="00"`, `<supported>1</supported>`)
	want(t, post(t, srv, "get_save_data_info_list", &alice, `<folder type="ps3" start="1" max="50"/>`),
		`result="00"`, `<storage-info quotasize="1048576" totalsize="0"/>`, `start="1" results="0"`)

	icon := []byte("\x89PNG fake icon")
	data := []byte("save\r\n\r\nbytes\x00\xff")
	r := upload(t, srv, &alice, "BLUS30001-SLOT1", icon, data)
	want(t, r, `result="00"`, `totalsize="`+strconv.Itoa(len(data)+len(icon))+`"`, `total="1"`, `seq="1"`, `<data name="BLUS30001-SLOT1">`,
		`<title>Game &amp; Co</title>`, `<icon-url>http://ubs.example/ubs/icon/`, `<date>`)

	r = post(t, srv, "get_save_data_info_list", &alice, `<folder type="ps3" start="1" max="50"/>`)
	want(t, r, `results="1"`, `<length>`+strconv.Itoa(len(data))+`</length>`, `<size>`+strconv.Itoa(len(data)*2)+`</size>`,
		`<updated>2026-11-03T10:00:00.000Z</updated>`, `<details>Chapter 3</details>`, `<owner-info>1</owner-info>`, `<downloadable-date>`)
	j := strings.Index(r.xml, "/ubs/icon/")
	res, err = http.Get(srv.URL + r.xml[j:j+len("/ubs/icon/")+32])
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Equal(got, icon) {
		t.Fatalf("icon: %q", got)
	}

	// Download: info-length XML, separator, the exact bytes.
	get := `<folder type="ps3"><data name="BLUS30001-SLOT1"/></folder>`
	r = post(t, srv, "get_save_data", &alice, get)
	if r.data != string(data) || r.hdr.Get(infoHdr) == "" {
		t.Fatalf("download: %q / %q", r.xml, r.data)
	}
	want(t, r, `result="00"`, `<data name="BLUS30001-SLOT1">`)
	want(t, post(t, srv, "notify_downloaded", &alice, get), `result="00"`, `<downloadable-date>`)

	// Other accounts and folders do not see it.
	want(t, post(t, srv, "get_save_data", &bob, get), `result="4a"`)
	want(t, post(t, srv, "get_save_data", &alice, `<folder type="psp"><data name="BLUS30001-SLOT1"/></folder>`), `result="4a"`)

	// Replacing keeps one save and bumps seq; no icon this time.
	data2 := []byte("second version")
	want(t, upload(t, srv, &alice, "BLUS30001-SLOT1", nil, data2), `result="00"`, `total="1"`, `seq="2"`, `totalsize="14"`)
	if r = post(t, srv, "get_save_data", &alice, get); r.data != string(data2) || strings.Contains(r.xml, "icon-url") {
		t.Fatalf("after replace: %q %q", r.xml, r.data)
	}

	// Validation.
	bad := alice
	bad.raw = []byte("AAAA")
	want(t, post(t, srv, "get_save_data_info_list", &bad, `<folder type="ps3"/>`), `result="02"`)
	want(t, post(t, srv, "get_save_data_info_list", &alice, `<folder type="ps4"/>`), `result="ff"`)
	want(t, post(t, srv, "set_save_data", &alice, setXML("X", nil, data, strings.Repeat("0", 32)), data), `result="ff"`)
	want(t, post(t, srv, "get_save_data", &alice, `<folder type="ps3"><data name="X"/></folder>`), `result="4a"`)
	want(t, upload(t, srv, &alice, "BIG", nil, make([]byte, 1<<19+1)), `result="1a"`)
	want(t, upload(t, srv, &alice, "A", nil, make([]byte, 1<<19)), `result="00"`)
	want(t, upload(t, srv, &alice, "B", nil, make([]byte, 1<<19)), `result="5f"`)
	want(t, upload(t, srv, &bob, "B", nil, make([]byte, 1<<19)), `result="00"`)

	// Paging: alice holds A and BLUS30001-SLOT1 in ps3.
	want(t, upload(t, srv, &alice, "C", nil, []byte("c")), `result="00"`)
	r = post(t, srv, "get_save_data_info_list", &alice, `<folder type="ps3" start="1" max="2"/>`)
	want(t, r, `start="1" results="2"`, `total="3"`, `<data name="A">`, `<data name="BLUS30001-SLOT1">`)
	want(t, post(t, srv, "get_save_data_info_list", &alice, `<folder type="ps3" start="3" max="2"/>`), `start="3" results="1"`, `<data name="C">`)

	// Delete.
	want(t, post(t, srv, "delete_save_data", &alice, get), `result="00"`, `total="2"`)
	want(t, post(t, srv, "delete_save_data", &alice, get), `result="4a"`)
	want(t, post(t, srv, "get_save_data", &alice, get), `result="4a"`)
}

// fakeSaves is the neutral blob surface of the core saves service, in memory, with the same
// answers the real one gives (docs/protocol.md there).
type fakeSaves struct {
	key       string
	allowance int64
	mu        sync.Mutex
	blobs     map[string]*fakeBlob // account/folder/name
	seq       map[string]int64
}

type fakeBlob struct {
	icon, data []byte
	iconID     string
	meta       map[string]any
	date       time.Time
}

func newFakeSaves(allowance int64) *fakeSaves {
	return &fakeSaves{key: "saves-key", allowance: allowance, blobs: map[string]*fakeBlob{}, seq: map[string]int64{}}
}

func (b *fakeBlob) json(name string, seq int64) map[string]any {
	return map[string]any{"name": name, "size": len(b.data), "icon_size": len(b.icon), "icon_id": b.iconID, "date": b.date, "meta": b.meta, "seq": seq}
}

func (f *fakeSaves) used(acct string) (n int64) {
	for k, b := range f.blobs {
		if strings.HasPrefix(k, acct+"/") {
			n += int64(len(b.icon) + len(b.data))
		}
	}
	return
}

func (f *fakeSaves) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Internal-Key") != f.key {
		http.Error(w, "key", 401)
		return
	}
	out := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(p) == 4 && p[0] == "internal" && p[1] == "blobs" && p[2] == "icons" {
		for _, b := range f.blobs {
			if b.iconID == p[3] {
				w.Write(b.icon)
				return
			}
		}
		out(404, nil)
		return
	}
	// internal/accounts/{acct}/blobs/ps3/{folder}[/{name}[/info]]
	if len(p) < 6 || p[3] != "blobs" || p[4] != "ps3" {
		out(400, nil)
		return
	}
	acct, folder := p[2], p[5]
	sk := acct + "/" + folder
	if len(p) == 6 {
		q := r.URL.Query()
		off, _ := strconv.Atoi(q.Get("offset"))
		lim, _ := strconv.Atoi(q.Get("limit"))
		var names []string
		var size int64
		count := 0
		for k, b := range f.blobs {
			if strings.HasPrefix(k, acct+"/") {
				count++
			}
			if strings.HasPrefix(k, sk+"/") {
				names = append(names, strings.TrimPrefix(k, sk+"/"))
				size += int64(len(b.icon) + len(b.data))
			}
		}
		sort.Strings(names)
		items := []any{}
		for i := off; i < len(names) && i < off+lim; i++ {
			items = append(items, f.blobs[sk+"/"+names[i]].json(names[i], 0))
		}
		out(200, map[string]any{"items": items, "total": len(names), "total_size": size, "seq": f.seq[sk], "platform_count": count,
			"usage": map[string]any{"allowance_used": f.used(acct), "allowance": f.allowance}})
		return
	}
	name, key := p[6], sk+"/"+p[6]
	cur := f.blobs[key]
	switch {
	case r.Method == "PUT":
		il, _ := strconv.ParseInt(r.Header.Get("X-Icon-Length"), 10, 64)
		body, _ := io.ReadAll(r.Body)
		if int64(len(body)) != r.ContentLength || il > r.ContentLength {
			out(422, nil)
			return
		}
		icon, data := body[:il], body[il:]
		if m := r.Header.Get("X-Data-Md5"); m != "" && m != md5hex(data) {
			out(422, nil)
			return
		}
		old := int64(0)
		if cur != nil {
			old = int64(len(cur.icon) + len(cur.data))
		}
		if f.used(acct)-old+int64(len(body)) > f.allowance {
			out(507, map[string]any{"error": "quota_exceeded"})
			return
		}
		raw, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Meta"))
		b := &fakeBlob{icon: icon, data: data, date: time.Now().UTC()}
		json.Unmarshal(raw, &b.meta)
		if il > 0 {
			b.iconID = md5hex([]byte(key + b.date.String()))
		}
		f.blobs[key] = b
		f.seq[sk]++
		out(200, b.json(name, f.seq[sk]))
	case cur == nil:
		out(404, nil)
	case r.Method == "DELETE":
		delete(f.blobs, key)
		f.seq[sk]++
		out(200, map[string]any{"deleted": true})
	case len(p) == 8:
		out(200, cur.json(name, f.seq[sk]))
	default:
		raw, _ := json.Marshal(cur.json(name, f.seq[sk]))
		w.Header().Set("X-Blob-Info", base64.StdEncoding.EncodeToString(raw))
		w.Write(cur.data)
	}
}

