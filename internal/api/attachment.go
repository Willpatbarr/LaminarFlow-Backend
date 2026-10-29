/*
╔═ attachment.go ═══════════════════════════════════════════════════════════════════════
║  http handlers · attachment resource
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeAttachmentTooLarge        const
║      CodeAttachmentTypeNotAllowed  const
║      attachmentBody                struct
║      attachmentTooLarge            struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  create · download · delete · list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/attachment"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/storage"

	"github.com/danielgtaylor/huma/v2"
)

// The two refusals of a file itself. A missing, invisible or deleted attachment is the
// derived not_found, and a bad owner is the derived unprocessable_entity.
const (
	CodeAttachmentTooLarge       = "attachment_too_large"
	CodeAttachmentTypeNotAllowed = "attachment_type_not_allowed"
)

// formOverhead is how much more than the file limit a whole upload request may declare:
// the multipart framing and the two small owner fields.
const formOverhead = 64 << 10

// maxFormParts bounds how many parts are read looking for the file, and maxFieldBytes how
// long an owner field may be, so a request cannot stall the handler on junk before the
// file. A uuid is 36 characters.
const (
	maxFormParts  = 16
	maxFieldBytes = 64
)

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type attachmentBody struct {
	ID          string    `json:"id" format:"uuid"`
	TicketID    *string   `json:"ticket_id" format:"uuid" nullable:"true" doc:"The ticket the file is attached to. Exactly one of ticket_id and document_id is set."`
	DocumentID  *string   `json:"document_id" format:"uuid" nullable:"true" doc:"The document the file is attached to."`
	Filename    string    `json:"filename" doc:"What the uploader called the file, sanitised: no directory part, no control characters, at most 255 bytes. For display; the download names itself from it."`
	ContentType string    `json:"content_type" doc:"What the server decided the file is, from its contents - not what the client claimed. Always one of the stored types listed on POST /attachments." example:"image/png"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedBy   *string   `json:"created_by" format:"uuid" nullable:"true" doc:"The uploader. Null when their account has been deleted: render an attachment with no uploader."`
	CreatedAt   time.Time `json:"created_at"`
	URL         string    `json:"url" example:"/api/v1/attachments/0b7d4a3e-6d5f-4c1e-9a53-2f8d6b1e4712" doc:"Where the bytes are, relative to the origin. Stable for the life of the attachment, so it is the address to put in a Markdown image: ![](url). Needs the same credential as the rest of the API; a browser sends the session cookie on its own."`
}

func asAttachmentBody(a attachment.Attachment) attachmentBody {
	return attachmentBody{
		ID:          a.ID,
		TicketID:    a.TicketID,
		DocumentID:  a.DocumentID,
		Filename:    a.Filename,
		ContentType: a.ContentType,
		SizeBytes:   a.SizeBytes,
		CreatedBy:   a.CreatedBy,
		CreatedAt:   a.CreatedAt.UTC(),
		URL:         V1 + "/attachments/" + a.ID,
	}
}

// attachmentTooLarge is the 413. Like ticketConflict it is its own type because the
// envelope has no room for the extra member, and a member on the wire that is not in the
// document is invisible to a generated client.
type attachmentTooLarge struct {
	huma.ErrorModel
	Code       string `json:"code" enum:"attachment_too_large" example:"attachment_too_large"`
	LimitBytes int64  `json:"limit_bytes" doc:"The largest file this server accepts, in bytes."`
}

func tooLarge(limit int64) *attachmentTooLarge {
	return &attachmentTooLarge{
		ErrorModel: huma.ErrorModel{
			Status: http.StatusRequestEntityTooLarge,
			Title:  http.StatusText(http.StatusRequestEntityTooLarge),
			Detail: fmt.Sprintf("the file is larger than the %d byte limit", limit),
		},
		Code:       CodeAttachmentTooLarge,
		LimitBytes: limit,
	}
}

type createAttachmentInput struct {
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"128" pattern:"^[ -~]+$" example:"2b0a7c9e-5d1f-4c8e-9a53-0f2f8d6b1e47" doc:"Makes the upload safe to retry. The same key from the same account returns the attachment the first call created instead of storing the file again, with Idempotent-Replayed: true, and the request body is not read. 1 to 128 printable ASCII characters; a UUID is ideal. Keys are per account and live as long as the attachment. Omit it for a plain upload."`

	// The request body is deliberately not a huma body field: huma would read a whole
	// multipart request, spooling it to disk, before the handler runs, and the size limit
	// has to stop the read. Resolve takes the raw stream instead.
	contentType string
	length      string
	body        io.Reader
}

func (in *createAttachmentInput) Resolve(ctx huma.Context) []error {
	in.contentType = ctx.Header("Content-Type")
	in.length = ctx.Header("Content-Length")
	in.body = ctx.BodyReader()

	return resolveIdempotencyKey(ctx)
}

func (in *createAttachmentInput) key() *string {
	if in.IdempotencyKey == "" {
		return nil
	}

	return &in.IdempotencyKey
}

type createAttachmentOutput struct {
	Replayed string `header:"Idempotent-Replayed" enum:"true,false" doc:"Only when Idempotency-Key was sent. true: that key had been used and nothing was stored. false: this request stored the file."`
	Body     attachmentBody
}

type attachmentPath struct {
	ID string `path:"id" format:"uuid"`
}

// attachmentError maps what the service can refuse with. The 413 and the form-shape errors
// are built where they are raised.
func attachmentError(svc *attachment.Service, err error) error {
	switch {
	case errors.Is(err, attachment.ErrNotFound):
		return NewError(http.StatusNotFound, defaultCode(http.StatusNotFound), "no such attachment")
	case errors.Is(err, attachment.ErrTargetNotFound):
		return NewError(http.StatusNotFound, defaultCode(http.StatusNotFound), "no such ticket or document")
	case errors.Is(err, attachment.ErrTooLarge):
		return tooLarge(svc.MaxBytes())
	case errors.Is(err, attachment.ErrTypeNotAllowed):
		return NewError(http.StatusUnsupportedMediaType, CodeAttachmentTypeNotAllowed,
			"this kind of file is not accepted: images (png, jpeg, gif, webp), pdf, plain text, csv, markdown, json, zip, gzip and Word, Excel and PowerPoint files are")
	}

	var fe *attachment.FieldError
	if errors.As(err, &fe) {
		if fe.Field == "Idempotency-Key" {
			return newFieldError(defaultCode(http.StatusUnprocessableEntity),
				"the Idempotency-Key was not accepted", "header.Idempotency-Key", fe.Kind.Error())
		}

		return newFieldError(defaultCode(http.StatusUnprocessableEntity),
			"the "+fe.Field+" was not accepted", "body."+fe.Field, fe.Kind.Error())
	}

	return err
}

// readUpload walks the multipart stream: owner fields until the file part, then hands the
// file part itself to the service as a stream. Parts after the file are never read.
//
// The owner fields have to come before the file. A stream cannot be rewound, and the owner
// is checked before a byte is stored, so a file that arrives first has nowhere to be
// checked against. FormData sends fields in the order they were appended.
func readUpload(ctx context.Context, svc *attachment.Service, accountID string, in *createAttachmentInput) (attachment.Attachment, bool, error) {
	mt, params, err := mime.ParseMediaType(in.contentType)
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return attachment.Attachment{}, false, NewError(http.StatusUnsupportedMediaType, defaultCode(http.StatusUnsupportedMediaType),
			"send the upload as multipart/form-data")
	}

	// A declared length over the limit is refused before reading anything. Chunked
	// uploads declare none and are caught by the limit on the file as it streams.
	if n, err := strconv.ParseInt(in.length, 10, 64); err == nil && n > svc.MaxBytes()+formOverhead {
		return attachment.Attachment{}, false, tooLarge(svc.MaxBytes())
	}

	if in.body == nil {
		return attachment.Attachment{}, false, newFieldError(defaultCode(http.StatusUnprocessableEntity),
			"the file was not accepted", "body.file", "a file is required")
	}

	var ticketID, documentID *string
	mr := multipart.NewReader(in.body, params["boundary"])

	for range maxFormParts {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return attachment.Attachment{}, false, NewError(http.StatusBadRequest, defaultCode(http.StatusBadRequest),
				"the multipart body could not be read")
		}

		switch part.FormName() {
		case "ticket_id", "document_id":
			raw, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
			if err != nil {
				return attachment.Attachment{}, false, NewError(http.StatusBadRequest, defaultCode(http.StatusBadRequest),
					"the multipart body could not be read")
			}
			v := strings.TrimSpace(string(raw))
			if len(raw) > maxFieldBytes || !uuidShape.MatchString(v) {
				return attachment.Attachment{}, false, newFieldError(defaultCode(http.StatusUnprocessableEntity),
					"the "+part.FormName()+" was not accepted", "body."+part.FormName(), "must be a uuid")
			}
			if part.FormName() == "ticket_id" {
				ticketID = &v
			} else {
				documentID = &v
			}

		case "file":
			if (ticketID == nil) == (documentID == nil) {
				if ticketID != nil {
					return attachment.Attachment{}, false, newFieldError(defaultCode(http.StatusUnprocessableEntity),
						"send exactly one of ticket_id and document_id", "body.document_id", "send only one of ticket_id and document_id")
				}

				return attachment.Attachment{}, false, newFieldError(defaultCode(http.StatusUnprocessableEntity),
					"send exactly one of ticket_id and document_id, before the file", "body.ticket_id",
					"exactly one of ticket_id and document_id is required, and it must come before the file part")
			}

			return svc.Create(ctx, accountID, attachment.CreateParams{
				TicketID:       ticketID,
				DocumentID:     documentID,
				Filename:       part.FileName(),
				DeclaredType:   part.Header.Get("Content-Type"),
				File:           part,
				IdempotencyKey: in.key(),
			})
		}
	}

	return attachment.Attachment{}, false, newFieldError(defaultCode(http.StatusUnprocessableEntity),
		"the file was not accepted", "body.file", "a file is required")
}

// contentDisposition names a download. The quoted filename is an ASCII fallback for old
// clients; filename* carries the real name (RFC 6266, RFC 5987). Neither can hold a quote,
// a backslash or a control character, so a name cannot break out of the header.
func contentDisposition(inline bool, name string) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}

	var fallback strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == ' ':
			fallback.WriteRune(r)
		default:
			fallback.WriteByte('_')
		}
	}

	var encoded strings.Builder
	for _, b := range []byte(name) {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9',
			strings.IndexByte("!#$&+-.^_`|~", b) >= 0:
			encoded.WriteByte(b)
		default:
			fmt.Fprintf(&encoded, "%%%02X", b)
		}
	}

	return kind + `; filename="` + fallback.String() + `"; filename*=UTF-8''` + encoded.String()
}

/*
┌─ api ───────────────────────────────────────────
│  registers the attachment operations
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *attachment.Service    nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  POST /api/v1/attachments, GET /api/v1/attachments/{id}
*/

// Auth: a session or a token. No new scope is minted: reads need ticket:read and writes
// ticket:write, as comments do (documents already sit under the ticket scopes), so a
// script that may edit tickets may attach files to them. A file is only reachable by
// someone who can see its ticket or document.
func registerAttachments(api huma.API, svc *attachment.Service) {
	registry := api.OpenAPI().Components.Schemas
	problem := func(t reflect.Type, name string) map[string]*huma.MediaType {
		return map[string]*huma.MediaType{"application/problem+json": {Schema: registry.Schema(t, true, name)}}
	}

	huma.Register(api, huma.Operation{
		OperationID: "create-attachment",
		Method:      http.MethodPost,
		Path:        V1 + "/attachments",
		Summary:     "Upload a file to a ticket or a document (safe to retry with Idempotency-Key)",
		Description: "multipart/form-data with `ticket_id` or `document_id` (exactly one, a uuid) and `file`. **Send the owner field before `file`**: the owner is checked before a byte is stored, and a stream cannot be rewound. " +
			"An owner the caller cannot see (absent, archived, another workspace) is a 404.\n\n" +
			"The size is limited per file (`MAX_ATTACHMENT_BYTES`, 25 MiB by default). It is enforced while the file is read, so an oversize upload is refused part-way through: `413 attachment_too_large`, with `limit_bytes` in the body.\n\n" +
			"The type is decided from the file's first bytes, not from the `Content-Type` the client put on the part, which only breaks ties between kinds of text and Office files. Stored types: png, jpeg, gif and webp images; pdf; plain text, csv, markdown and json; zip and gzip; Word, Excel and PowerPoint (.docx, .xlsx, .pptx); and application/octet-stream for other binary data. Anything else - HTML, SVG, XML, script, audio, video - is `415 attachment_type_not_allowed`. An empty file is a 422 at `body.file`.\n\n" +
			"`filename` is sanitised (directory part and control characters removed, 255 bytes at most) and used only to name the download. It is never part of a path on disk.\n\n" +
			"Send `Idempotency-Key` to make the upload safe to retry after a lost response: the first request with a key stores the file and answers `Idempotent-Replayed: false`; any later request with the same key from the same account stores nothing, does not read its body and answers `Idempotent-Replayed: true` with the original attachment, same status 200.\n\n" +
			"Needs a session or a token with the `ticket:write` scope.",
		Metadata: needs(auth.TicketWrite),
		Responses: map[string]*huma.Response{
			"413":     {Description: "The file is over the size limit. Nothing was stored.", Content: problem(reflect.TypeOf(attachmentTooLarge{}), "AttachmentTooLarge")},
			"415":     {Description: "The file is of a type that is not stored (code attachment_type_not_allowed), or the request is not multipart/form-data.", Content: problem(reflect.TypeOf(Error{}), "Error")},
			"default": {Description: "Error", Content: problem(reflect.TypeOf(Error{}), "Error")},
		},
	}, func(ctx context.Context, in *createAttachmentInput) (*createAttachmentOutput, error) {
		me, _ := auth.FromContext(ctx)

		a, replayed, err := readUpload(ctx, svc, me.AccountID, in)
		if err != nil {
			return nil, attachmentError(svc, err)
		}

		out := &createAttachmentOutput{Body: asAttachmentBody(a)}
		if in.key() != nil {
			out.Replayed = strconv.FormatBool(replayed)
		}

		return out, nil
	})

	// The request body is declared on the document after registering, not on the
	// Operation. huma reads and buffers (1 MiB at most) the body of any operation that
	// declares a structured request body, which is the opposite of what the size limit
	// needs; an operation that declares none leaves the stream alone for Resolve.
	api.OpenAPI().Paths[V1+"/attachments"].Post.RequestBody = &huma.RequestBody{
		Required: true,
		Content: map[string]*huma.MediaType{"multipart/form-data": {Schema: &huma.Schema{
			Type:     huma.TypeObject,
			Required: []string{"file"},
			Properties: map[string]*huma.Schema{
				"ticket_id":   {Type: huma.TypeString, Format: "uuid", Description: "The ticket to attach to. Exactly one of ticket_id and document_id; send it before file."},
				"document_id": {Type: huma.TypeString, Format: "uuid", Description: "The document to attach to."},
				"file":        {Type: huma.TypeString, Format: "binary", Description: "The file."},
			},
		}}},
	}

	huma.Register(api, huma.Operation{
		OperationID: "download-attachment",
		Method:      http.MethodGet,
		Path:        V1 + "/attachments/{id}",
		Summary:     "Download a file",
		Description: "The raw bytes, with the stored `Content-Type`. This is the stable address of the file - `url` on the attachment - and is what a Markdown image points at.\n\n" +
			"Always sent with `X-Content-Type-Options: nosniff` and `Content-Security-Policy: default-src 'none'; sandbox`. `Content-Disposition` is `inline` for png, jpeg, gif and webp, so `<img src>` works, and `attachment` for every other type, with the name in `filename*` (RFC 6266). Nothing a user uploaded is ever served as a page: HTML and SVG cannot be stored at all.\n\n" +
			"Needs a session or a token with the `ticket:read` scope. 404 `not_found` for an attachment that does not exist or whose ticket or document the caller cannot see.",
		Metadata: needs(auth.TicketRead),
		Responses: map[string]*huma.Response{
			"200": {Description: "The file's bytes.", Headers: map[string]*huma.Param{
				"Content-Disposition": {Description: "inline for png, jpeg, gif and webp; attachment otherwise.", Schema: &huma.Schema{Type: huma.TypeString}},
			}, Content: map[string]*huma.MediaType{"*/*": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}}},
			"default": {Description: "Error", Content: problem(reflect.TypeOf(Error{}), "Error")},
		},
	}, func(ctx context.Context, in *attachmentPath) (*huma.StreamResponse, error) {
		me, _ := auth.FromContext(ctx)

		a, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, attachmentError(svc, err)
		}

		rc, err := svc.Open(ctx, a)
		if errors.Is(err, storage.ErrNotFound) {
			// A row whose file is gone: a database restored without its files folder.
			log.Printf("attachment: %s has no file in storage (key %s)", a.ID, a.StorageKey)
			return nil, NewError(http.StatusNotFound, defaultCode(http.StatusNotFound), "the file's contents are missing from storage")
		}
		if err != nil {
			return nil, err
		}

		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer rc.Close()

			ct := a.ContentType
			if strings.HasPrefix(ct, "text/") {
				ct += "; charset=utf-8"
			}

			hctx.SetHeader("Content-Type", ct)
			hctx.SetHeader("Content-Length", strconv.FormatInt(a.SizeBytes, 10))
			hctx.SetHeader("Content-Disposition", contentDisposition(attachment.Inline(a.ContentType), a.Filename))
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			hctx.SetHeader("Content-Security-Policy", "default-src 'none'; sandbox")
			hctx.SetHeader("Cross-Origin-Resource-Policy", "same-origin")
			hctx.SetHeader("Cache-Control", "private, max-age=300")
			hctx.SetStatus(http.StatusOK)

			// Headers are out; a failure now can only truncate, and the client sees that
			// as a short body against Content-Length.
			if _, err := io.Copy(hctx.BodyWriter(), rc); err != nil {
				log.Printf("attachment: stream %s: %v", a.ID, err)
			}
		}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-attachment",
		Method:      http.MethodDelete,
		Path:        V1 + "/attachments/{id}",
		Summary:     "Delete an attachment",
		Description: "Removes the record, then the file. A file already missing from storage does not stop the record going. Any member who can see the ticket or document may delete its attachments. Needs a session or a token with the `ticket:write` scope.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *attachmentPath) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		if _, err := svc.Delete(ctx, me.AccountID, in.ID); err != nil {
			return nil, attachmentError(svc, err)
		}

		return nil, nil
	})

	registerList(api, listSpec[attachmentBody]{
		OperationID: "list-attachments",
		Permission:  auth.TicketRead,
		Path:        V1 + "/attachments/list",
		Summary:     "List attachments (a read - see the description for why it is a POST)",
		Fields:      attachment.Fields,
		Run: func(ctx context.Context, accountID string, req listRequest) (listPage[attachmentBody], error) {
			p := attachment.ListParams{Sort: req.Sort, Limit: req.Limit, Cursor: req.Cursor}
			if req.Filter != nil {
				p.Filter = *req.Filter
			}

			page, err := svc.List(ctx, accountID, p)
			if err != nil {
				return listPage[attachmentBody]{}, err
			}

			items := make([]attachmentBody, len(page.Attachments))
			for i, a := range page.Attachments {
				items[i] = asAttachmentBody(a)
			}

			return listPage[attachmentBody]{Items: items, NextCursor: page.NextCursor, HasMore: page.HasMore}, nil
		},
	})
}
