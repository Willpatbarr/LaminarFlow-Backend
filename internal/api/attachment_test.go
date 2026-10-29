package api

import (
	"bytes"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
)

// LAM-90. The attachments API over the wire: upload, download with its safe headers,
// list, delete, the size limit while streaming, the type allowlist, a hostile filename,
// owner visibility and Idempotency-Key.

type attachmentWire struct {
	ID          string    `json:"id"`
	TicketID    *string   `json:"ticket_id"`
	DocumentID  *string   `json:"document_id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedBy   *string   `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	URL         string    `json:"url"`
}

// 1x1 transparent png.
var tinyPNG = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89}

type upload struct {
	fields      [][2]string // in order, before the file
	filename    string
	contentType string // the part's own, as a client declares it
	data        []byte
	fieldsLast  bool
}

func (u upload) request(t *testing.T, cookie string, header ...string) *http.Request {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	writeFields := func() {
		for _, f := range u.fields {
			if err := w.WriteField(f[0], f[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !u.fieldsLast {
		writeFields()
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(u.filename)+`"`)
	if u.contentType != "" {
		h.Set("Content-Type", u.contentType)
	}
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	part.Write(u.data)
	if u.fieldsLast {
		writeFields()
	}
	w.Close()

	req := httptest.NewRequest(http.MethodPost, V1+"/attachments", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: cookie})
	}
	if len(header) == 2 {
		req.Header.Set(header[0], header[1])
	}

	return req
}

func (f feed) upload(t *testing.T, cookie string, u upload, header ...string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, u.request(t, cookie, header...))

	return rec
}

func (f feed) attach(t *testing.T, cookie string, u upload) attachmentWire {
	t.Helper()

	rec := f.upload(t, cookie, u)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload %q: %d %.300s", u.filename, rec.Code, rec.Body)
	}
	var a attachmentWire
	decode(t, rec, &a)

	return a
}

func ownedBy(ticketID string) [][2]string { return [][2]string{{"ticket_id", ticketID}} }

func (f feed) download(t *testing.T, cookie, id string) *httptest.ResponseRecorder {
	t.Helper()

	return call(t, f.mux, http.MethodGet, V1+"/attachments/"+id, nil, cookie)
}

func (f feed) filesOnDisk(t *testing.T) []string {
	t.Helper()

	dir, _ := attachmentRoots.Load(f.mux)
	var out []string
	filepath.WalkDir(dir.(string), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}

		return nil
	})

	return out
}

func TestUploadDownloadListAndDelete(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Screenshot", "")

	a := f.attach(t, f.sarahC, upload{fields: ownedBy(tk.ID), filename: "shot.png", contentType: "image/png", data: tinyPNG})
	if a.ID == "" || a.TicketID == nil || *a.TicketID != tk.ID || a.DocumentID != nil || a.Filename != "shot.png" ||
		a.ContentType != "image/png" || a.SizeBytes != int64(len(tinyPNG)) ||
		a.CreatedBy == nil || *a.CreatedBy != f.sarah.AccountID || a.URL != V1+"/attachments/"+a.ID {
		t.Errorf("attachment = %+v", a)
	}

	// The bytes, with the stored type, inline because it is a raster image.
	rec := f.download(t, f.willC, a.ID)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), tinyPNG) {
		t.Fatalf("download: %d %q", rec.Code, rec.Body)
	}
	for header, want := range map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Content-Length":          strconv.Itoa(len(tinyPNG)),
		"Content-Security-Policy": "default-src 'none'; sandbox",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "inline;") || !strings.Contains(cd, "filename*=UTF-8''shot.png") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	list := func(cookie string) []attachmentWire {
		rec := call(t, f.mux, http.MethodPost, V1+"/attachments/list", map[string]any{"filter": byTicket(tk.ID)}, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %.300s", rec.Code, rec.Body)
		}
		var out struct {
			Items []attachmentWire `json:"items"`
		}
		decode(t, rec, &out)

		return out.Items
	}
	if got := list(f.willC); len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("list = %+v", got)
	}
	if got := list(f.carolC); len(got) != 0 {
		t.Errorf("a caller outside the workspace listed %+v", got)
	}

	if rec := call(t, f.mux, http.MethodDelete, V1+"/attachments/"+a.ID, nil, f.willC); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %.300s", rec.Code, rec.Body)
	}
	if rec := f.download(t, f.willC, a.ID); rec.Code != http.StatusNotFound {
		t.Errorf("download after delete = %d, want 404", rec.Code)
	}
	if rec := call(t, f.mux, http.MethodDelete, V1+"/attachments/"+a.ID, nil, f.willC); rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}
	if left := f.filesOnDisk(t); len(left) != 0 {
		t.Errorf("files left after delete: %v", left)
	}
}

func TestAnAttachmentOnADocument(t *testing.T) {
	f := newFeed(t)
	doc := f.document(t, f.willC, map[string]any{"title": "Spec", "project_id": f.project.ID})

	a := f.attach(t, f.willC, upload{fields: [][2]string{{"document_id", doc.ID}}, filename: "notes.txt", contentType: "text/plain", data: []byte("hello")})
	if a.DocumentID == nil || *a.DocumentID != doc.ID || a.TicketID != nil || a.ContentType != "text/plain" {
		t.Errorf("attachment = %+v", a)
	}

	rec := call(t, f.mux, http.MethodPost, V1+"/attachments/list", map[string]any{"filter": byDocument(doc.ID)}, f.sarahC)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), a.ID) {
		t.Errorf("list by document: %d %.300s", rec.Code, rec.Body)
	}

	// Not an image, so it downloads rather than renders.
	dl := f.download(t, f.willC, a.ID)
	if cd := dl.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if ct := dl.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestTheFilenameNeverReachesThePath(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Hostile", "")

	a := f.attach(t, f.willC, upload{fields: ownedBy(tk.ID), filename: "../../../etc/cron.d/evil\"; X-Injected: 1.png", contentType: "image/png", data: tinyPNG})
	if strings.ContainsAny(a.Filename, "/\\\r\n") || a.Filename == "" {
		t.Errorf("stored filename = %q", a.Filename)
	}

	files := f.filesOnDisk(t)
	if len(files) != 1 {
		t.Fatalf("files on disk = %v", files)
	}
	if base := filepath.Base(files[0]); strings.Contains(base, "evil") || strings.Contains(base, "png") || len(base) != 36 {
		t.Errorf("on-disk name %q should be a bare generated uuid", base)
	}

	cd := f.download(t, f.willC, a.ID).Header().Get("Content-Disposition")
	if strings.ContainsAny(cd, "\r\n") || strings.Count(cd, `"`) != 2 {
		t.Errorf("Content-Disposition = %q", cd)
	}

	// A non-ASCII name travels in filename*.
	u := f.attach(t, f.willC, upload{fields: ownedBy(tk.ID), filename: "résumé 日本.txt", contentType: "text/plain", data: []byte("x")})
	if cd := f.download(t, f.willC, u.ID).Header().Get("Content-Disposition"); !strings.Contains(cd, "filename*=UTF-8''r%C3%A9sum%C3%A9%20%E6%97%A5%E6%9C%AC.txt") {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

func TestAnOversizeFileIs413AndNothingIsKept(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Big", "")

	// One byte over: refused while the file streams, since the request is under the
	// declared-length shortcut.
	big := append([]byte("%PDF-"), bytes.Repeat([]byte("a"), testMaxAttachmentBytes)...)
	rec := f.upload(t, f.willC, upload{fields: ownedBy(tk.ID), filename: "big.pdf", contentType: "application/pdf", data: big})
	assertTooLarge(t, rec)

	// Well over: refused from Content-Length, before a byte is read.
	huge := bytes.Repeat([]byte("a"), testMaxAttachmentBytes+200<<10)
	rec = f.upload(t, f.willC, upload{fields: ownedBy(tk.ID), filename: "huge.txt", data: huge})
	assertTooLarge(t, rec)

	// Exactly at the limit is fine.
	atLimit := append([]byte("%PDF-"), bytes.Repeat([]byte("a"), testMaxAttachmentBytes-5)...)
	if rec := f.upload(t, f.willC, upload{fields: ownedBy(tk.ID), filename: "ok.pdf", data: atLimit}); rec.Code != http.StatusOK {
		t.Errorf("a file of exactly the limit: %d %.200s", rec.Code, rec.Body)
	}

	if files := f.filesOnDisk(t); len(files) != 1 {
		t.Errorf("files on disk = %v, want only the one that fit", files)
	}
}

func assertTooLarge(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	var body struct {
		Code       string `json:"code"`
		LimitBytes int64  `json:"limit_bytes"`
	}
	decode(t, rec, &body)
	if rec.Code != http.StatusRequestEntityTooLarge || body.Code != CodeAttachmentTooLarge || body.LimitBytes != testMaxAttachmentBytes {
		t.Errorf("oversize = %d %+v (%.200s)", rec.Code, body, rec.Body)
	}
}

func TestOnlySafeTypesAreStored(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Types", "")

	for name, u := range map[string]upload{
		"html declared as a png":   {filename: "a.png", contentType: "image/png", data: []byte("<!DOCTYPE html><script>alert(1)</script>")},
		"html declared plainly":    {filename: "a.html", contentType: "text/html", data: []byte("<p>hi</p>")},
		"svg declared as svg":      {filename: "a.svg", contentType: "image/svg+xml", data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		"svg declared as a png":    {filename: "a.png", contentType: "image/png", data: []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)},
		"xml":                      {filename: "a.xml", contentType: "application/xml", data: []byte("<a/>")},
		"javascript declared text": {filename: "a.js", contentType: "text/javascript", data: []byte("alert(1)")},
	} {
		u.fields = ownedBy(tk.ID)
		rec := f.upload(t, f.willC, u)

		var body struct {
			Code string `json:"code"`
		}
		decode(t, rec, &body)
		if rec.Code != http.StatusUnsupportedMediaType || body.Code != CodeAttachmentTypeNotAllowed {
			t.Errorf("%s: %d %.200s", name, rec.Code, rec.Body)
		}
	}

	// The client's word is a hint only: png bytes declared as a pdf are stored as a png.
	a := f.attach(t, f.willC, upload{fields: ownedBy(tk.ID), filename: "x.pdf", contentType: "application/pdf", data: tinyPNG})
	if a.ContentType != "image/png" {
		t.Errorf("content_type = %q, want the sniffed image/png", a.ContentType)
	}
	// Markdown cannot be sniffed, so the declared type breaks the tie.
	m := f.attach(t, f.willC, upload{fields: ownedBy(tk.ID), filename: "x.md", contentType: "text/markdown; charset=utf-8", data: []byte("# hi")})
	if m.ContentType != "text/markdown" {
		t.Errorf("content_type = %q, want text/markdown", m.ContentType)
	}

	if rec := f.upload(t, f.willC, upload{fields: ownedBy(tk.ID), filename: "empty.txt", data: nil}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty file = %d, want 422", rec.Code)
	}
}

func TestTheOwnerIsValidated(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Owner", "")
	doc := f.document(t, f.willC, map[string]any{"title": "Spec", "project_id": f.project.ID})
	file := upload{filename: "a.png", contentType: "image/png", data: tinyPNG}

	locations := func(rec *httptest.ResponseRecorder) string {
		var body struct {
			Errors []struct {
				Location string `json:"location"`
			} `json:"errors"`
		}
		decode(t, rec, &body)
		if len(body.Errors) != 1 {
			return rec.Body.String()
		}

		return body.Errors[0].Location
	}

	none := file
	if rec := f.upload(t, f.willC, none); rec.Code != 422 || locations(rec) != "body.ticket_id" {
		t.Errorf("no owner: %d %s", rec.Code, locations(rec))
	}

	both := file
	both.fields = [][2]string{{"ticket_id", tk.ID}, {"document_id", doc.ID}}
	if rec := f.upload(t, f.willC, both); rec.Code != 422 || locations(rec) != "body.document_id" {
		t.Errorf("both owners: %d %s", rec.Code, locations(rec))
	}

	late := file
	late.fields, late.fieldsLast = ownedBy(tk.ID), true
	if rec := f.upload(t, f.willC, late); rec.Code != 422 || locations(rec) != "body.ticket_id" {
		t.Errorf("owner after the file: %d %s", rec.Code, locations(rec))
	}

	bad := file
	bad.fields = [][2]string{{"ticket_id", "not-a-uuid"}}
	if rec := f.upload(t, f.willC, bad); rec.Code != 422 || locations(rec) != "body.ticket_id" {
		t.Errorf("bad uuid: %d %s", rec.Code, locations(rec))
	}

	// An owner the caller cannot see is a 404, as is one that does not exist.
	for name, owner := range map[string]string{"another workspace": tk.ID, "absent": "00000000-0000-4000-8000-000000000000"} {
		cookie := f.carolC
		if name == "absent" {
			cookie = f.willC
		}
		u := file
		u.fields = ownedBy(owner)
		if rec := f.upload(t, cookie, u); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d %.200s", name, rec.Code, rec.Body)
		}
	}

	if files := f.filesOnDisk(t); len(files) != 0 {
		t.Errorf("a refused upload left %v", files)
	}
}

func TestAnAttachmentIsOnlyReachableByTheWorkspace(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Private", "")
	a := f.attach(t, f.willC, upload{fields: ownedBy(tk.ID), filename: "a.png", data: tinyPNG})

	if rec := f.download(t, f.carolC, a.ID); rec.Code != http.StatusNotFound {
		t.Errorf("download from outside = %d, want 404", rec.Code)
	}
	if rec := call(t, f.mux, http.MethodDelete, V1+"/attachments/"+a.ID, nil, f.carolC); rec.Code != http.StatusNotFound {
		t.Errorf("delete from outside = %d, want 404", rec.Code)
	}
	if rec := f.download(t, "", a.ID); rec.Code != http.StatusUnauthorized {
		t.Errorf("download with no credential = %d, want 401", rec.Code)
	}
	if rec := f.upload(t, "", upload{fields: ownedBy(tk.ID), filename: "a.png", data: tinyPNG}); rec.Code != http.StatusUnauthorized {
		t.Errorf("upload with no credential = %d, want 401", rec.Code)
	}
}

func TestAnIdempotencyKeyStoresTheFileOnce(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Retry", "")
	u := upload{fields: ownedBy(tk.ID), filename: "a.png", contentType: "image/png", data: tinyPNG}

	first := f.upload(t, f.willC, u, "Idempotency-Key", "k-1")
	second := f.upload(t, f.willC, u, "Idempotency-Key", "k-1")
	if first.Code != 200 || second.Code != 200 {
		t.Fatalf("statuses %d, %d", first.Code, second.Code)
	}
	if first.Header().Get("Idempotent-Replayed") != "false" || second.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("replayed headers: %q, %q", first.Header().Get("Idempotent-Replayed"), second.Header().Get("Idempotent-Replayed"))
	}
	var a, b attachmentWire
	decode(t, first, &a)
	decode(t, second, &b)
	if a.ID != b.ID {
		t.Errorf("replay made a second attachment: %s and %s", a.ID, b.ID)
	}
	if files := f.filesOnDisk(t); len(files) != 1 {
		t.Errorf("files on disk = %v, want 1", files)
	}

	// Another account's key is its own.
	other := f.upload(t, f.sarahC, u, "Idempotency-Key", "k-1")
	var c attachmentWire
	decode(t, other, &c)
	if other.Header().Get("Idempotent-Replayed") != "false" || c.ID == a.ID {
		t.Errorf("a different account replayed: %s", other.Body)
	}

	if rec := f.upload(t, f.willC, u, "Idempotency-Key", ""); rec.Code != 422 {
		t.Errorf("empty key = %d, want 422", rec.Code)
	}
}

func TestDeletingWhenTheFileIsAlreadyGone(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Gone", "")
	a := f.attach(t, f.willC, upload{fields: ownedBy(tk.ID), filename: "a.png", data: tinyPNG})

	for _, p := range f.filesOnDisk(t) {
		os.Remove(p)
	}

	if rec := f.download(t, f.willC, a.ID); rec.Code != http.StatusNotFound {
		t.Errorf("download with the file missing = %d, want 404", rec.Code)
	}
	if rec := call(t, f.mux, http.MethodDelete, V1+"/attachments/"+a.ID, nil, f.willC); rec.Code != http.StatusNoContent {
		t.Errorf("delete with the file missing = %d, want 204", rec.Code)
	}
	if rec := call(t, f.mux, http.MethodPost, V1+"/attachments/list", map[string]any{"filter": byTicket(tk.ID)}, f.willC); strings.Contains(rec.Body.String(), a.ID) {
		t.Errorf("the row survived: %s", rec.Body)
	}
}

func TestANonMultipartUploadIsRefused(t *testing.T) {
	f := newFeed(t)

	rec := call(t, f.mux, http.MethodPost, V1+"/attachments", map[string]any{"ticket_id": "x"}, f.willC)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("json upload = %d, want 415", rec.Code)
	}
}
