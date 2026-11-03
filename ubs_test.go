package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

func setup(t *testing.T, store Store, quota, saveMax int64) (*httptest.Server, map[string]user) {
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
		Quota: quota, SaveMax: saveMax, Log: slog.Default()}).Register(mux)
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

func runScenario(t *testing.T, store Store) {
	srv, users := setup(t, store, 1<<20, 1<<19)
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
	want(t, r, `result="00"`, `totalsize="`+strconv.Itoa(len(data))+`"`, `total="1"`, `seq="1"`, `<data name="BLUS30001-SLOT1">`,
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

func TestMemStore(t *testing.T) { runScenario(t, &MemStore{}) }

func TestPGStore(t *testing.T) {
	dbURL := os.Getenv("NP_UBS_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("NP_UBS_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(), `DROP TABLE IF EXISTS ubs_chunks, ubs_saves, ubs_folders`); err != nil {
		t.Fatal(err)
	}
	s := &PGStore{DB: pool}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runScenario(t, s)

	// Multi-chunk round trip.
	big := make([]byte, chunkSize*2+123)
	rand.Read(big)
	sv := Save{Owner: "z", Folder: "ps3", Name: "BIG", Length: int64(len(big)), Date: time.Now(), Updated: time.Now()}
	if err := s.Put(context.Background(), sv, nil, bytes.NewReader(big), func() error { return nil }, 1<<30); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := s.WriteData(context.Background(), "z", "ps3", "BIG", &out); err != nil || !bytes.Equal(out.Bytes(), big) {
		t.Fatal("chunked round trip", err, out.Len())
	}
}
