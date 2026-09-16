package breeze

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
)

// UploadedFile holds a parsed uploaded file's metadata and content.
const (
	maxMultipartFieldBytes = 1 << 20   // 1 MiB per non-file field
	maxMultipartParts      = 256       // protects parser/bookkeeping memory
	maxMultipartTotalBytes = 128 << 20 // 128 MiB aggregate parsed data
)

type UploadedFile struct {
	Field       string               // form field name
	Filename    string               // uploaded filename
	Header      textproto.MIMEHeader // original part headers
	ContentType string               // detected content type (from header or sniff)
	Size        int64                // size in bytes
	Content     []byte               // file bytes (nil if streamed)
}

// ParseMultipart parses a multipart/form-data request body and returns:
// - files: map[field][]*UploadedFile
// - fields: map[field][]string (other non-file form fields)
// If a file is bigger than maxFileSize, an error is returned for that part.
// Pass maxFileSize as 0 to allow unlimited (not recommended).
//
// This function DOES NOT use http.Request; it operates on your HTTPRequest struct.
func (req *HTTPRequest) ParseMultipart(maxFileSize int64) (map[string][]*UploadedFile, map[string][]string, error) {
	ct := req.Header["content-type"]
	if ct == "" {
		return nil, nil, errors.New("content-type missing")
	}

	mediatype, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid content-type: %w", err)
	}
	if !strings.HasPrefix(mediatype, "multipart/") {
		return nil, nil, fmt.Errorf("content-type is not multipart: %s", mediatype)
	}
	boundary, ok := params["boundary"]
	if !ok || boundary == "" {
		return nil, nil, errors.New("multipart boundary not found")
	}

	r := multipart.NewReader(bytes.NewReader(req.Body), boundary)

	files := map[string][]*UploadedFile{}
	fields := map[string][]string{}
	parts := 0
	var totalParsed int64

	for {
		part, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading multipart: %w", err)
		}
		parts++
		if parts > maxMultipartParts {
			_ = part.Close()
			return nil, nil, fmt.Errorf("multipart contains more than %d parts", maxMultipartParts)
		}

		// get form name
		formName := part.FormName()
		filename := part.FileName()
		_ = part.Header

		// If filename is empty => regular form field
		if filename == "" {
			val, readErr := io.ReadAll(io.LimitReader(part, maxMultipartFieldBytes+1))
			if readErr != nil {
				_ = part.Close()
				return nil, nil, fmt.Errorf("read form field %s: %w", formName, readErr)
			}
			if len(val) > maxMultipartFieldBytes {
				_ = part.Close()
				return nil, nil, fmt.Errorf("form field %s exceeds %d bytes", formName, maxMultipartFieldBytes)
			}
			totalParsed += int64(len(val))
			if totalParsed > maxMultipartTotalBytes {
				_ = part.Close()
				return nil, nil, fmt.Errorf("multipart payload exceeds %d parsed bytes", maxMultipartTotalBytes)
			}
			fields[formName] = append(fields[formName], string(val))
			_ = part.Close()
			continue
		}

		// It's a file part. Limit reader if maxFileSize > 0
		var lr io.Reader = part
		if maxFileSize > 0 {
			lr = io.LimitReader(part, maxFileSize+1) // +1 to detect overflow
		}

		data, readErr := io.ReadAll(lr)
		_ = part.Close()
		if readErr != nil {
			return nil, nil, fmt.Errorf("read file %s: %w", filename, readErr)
		}

		if maxFileSize > 0 && int64(len(data)) > maxFileSize {
			return nil, nil, fmt.Errorf("file %s exceeds maxFileSize (%d bytes)", filename, maxFileSize)
		}
		totalParsed += int64(len(data))
		if totalParsed > maxMultipartTotalBytes {
			return nil, nil, fmt.Errorf("multipart payload exceeds %d parsed bytes", maxMultipartTotalBytes)
		}

		// Determine content type: prefer part header, fallback to sniffing
		contentType := part.Header.Get("Content-Type")
		if contentType == "" {
			contentType = http.DetectContentType(data)
		}

		uf := &UploadedFile{
			Field:       formName,
			Filename:    filename,
			Header:      part.Header,
			ContentType: contentType,
			Size:        int64(len(data)),
			Content:     data,
		}
		files[formName] = append(files[formName], uf)
	}

	return files, fields, nil
}

// SaveUploadedFileFromRequest parses the multipart form and saves the first file
// from fieldName to destPath. maxFileSize limits each file size in bytes (0 = unlimited).
// Returns the saved filename (original) and error if any.
func (req *HTTPRequest) SaveUploadedFileFromRequest(fieldName, destPath string, maxFileSize int64) (string, error) {
	files, _, err := req.ParseMultipart(maxFileSize)
	if err != nil {
		return "", err
	}
	list, ok := files[fieldName]
	if !ok || len(list) == 0 {
		return "", fmt.Errorf("field %s not found", fieldName)
	}
	uf := list[0]
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(destPath, uf.Content, 0644); err != nil {
		return "", err
	}
	return uf.Filename, nil
}

//
// Context convenience helpers
//

// ParseMultipart stores parsed files and fields into context (so handlers can reuse).
// It returns files and fields same as req.ParseMultipart.
func (ctx *Context) ParseMultipart(maxFileSize int64) (map[string][]*UploadedFile, map[string][]string, error) {
	return ctx.Req.ParseMultipart(maxFileSize)
}

// SaveUploadedFile saves the first uploaded file from fieldName to destPath using ctx.Req.
// Returns the original filename and error if any.
func (ctx *Context) SaveUploadedFile(fieldName, destPath string, maxFileSize int64) (string, error) {
	return ctx.Req.SaveUploadedFileFromRequest(fieldName, destPath, maxFileSize)
}
