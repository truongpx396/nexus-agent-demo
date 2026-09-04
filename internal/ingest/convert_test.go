package ingest

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestConvert_PlainText(t *testing.T) {
	doc, err := Convert("notes.txt", MimePlainText, []byte("hello world"))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if doc.Text != "hello world" {
		t.Errorf("Text = %q, want %q", doc.Text, "hello world")
	}
	if len(doc.SourceDigest) != 32 {
		t.Errorf("SourceDigest length = %d, want 32 (sha256)", len(doc.SourceDigest))
	}
}

func TestConvert_PlainText_InvalidUTF8Rejected(t *testing.T) {
	if _, err := Convert("bad.txt", MimePlainText, []byte{0xff, 0xfe, 0x00}); err == nil {
		t.Fatal("expected an error for invalid UTF-8, got nil")
	}
}

func TestConvert_UnsupportedMimeType(t *testing.T) {
	if _, err := Convert("f.bin", "application/octet-stream", []byte("x")); err == nil {
		t.Fatal("expected an error for an unsupported mime type, got nil")
	}
}

func TestConvert_HTML_StripsTagsAndScripts(t *testing.T) {
	html := `<html><head><style>.x{color:red}</style></head><body>
<h1>Title</h1>
<p>Hello <b>world</b>.</p>
<script>alert('should not appear')</script>
<p>Second &amp; third paragraph.</p>
</body></html>`
	doc, err := Convert("page.html", MimeHTML, []byte(html))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if strings.Contains(doc.Text, "alert") {
		t.Errorf("script content leaked into extracted text: %q", doc.Text)
	}
	if strings.Contains(doc.Text, "color:red") {
		t.Errorf("style content leaked into extracted text: %q", doc.Text)
	}
	if !strings.Contains(doc.Text, "Title") || !strings.Contains(doc.Text, "Hello") || !strings.Contains(doc.Text, "world") {
		t.Errorf("expected visible text to survive, got %q", doc.Text)
	}
	if !strings.Contains(doc.Text, "Second & third paragraph") {
		t.Errorf("expected entity decoding, got %q", doc.Text)
	}
}

// minimalPDF builds a syntactically-minimal, UNCOMPRESSED PDF content
// stream containing one text-showing operator — enough to exercise
// convertPDF's stream-scan + Tj-extraction path without a real PDF writer.
func minimalPDF(text string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	b.WriteString("1 0 obj << /Length 100 >>\nstream\n")
	b.WriteString("BT /F1 12 Tf 72 720 Td (" + text + ") Tj ET\n")
	b.WriteString("endstream\nendobj\n")
	b.WriteString("%%EOF")
	return b.Bytes()
}

func TestConvert_PDF_ExtractsUncompressedText(t *testing.T) {
	doc, err := Convert("doc.pdf", MimePDF, minimalPDF("Hello PDF World"))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(doc.Text, "Hello PDF World") {
		t.Errorf("expected extracted text to contain the shown string, got %q", doc.Text)
	}
}

// minimalDOCX builds a syntactically-minimal OOXML zip container with just
// enough of word/document.xml for convertDOCX to find and extract.
func minimalDOCX(t *testing.T, paragraphs ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	var xmlBody strings.Builder
	xmlBody.WriteString(`<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="ns"><w:body>`)
	for _, p := range paragraphs {
		xmlBody.WriteString(`<w:p><w:r><w:t>` + p + `</w:t></w:r></w:p>`)
	}
	xmlBody.WriteString(`</w:body></w:document>`)
	if _, err := w.Write([]byte(xmlBody.String())); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestConvert_DOCX_ExtractsParagraphs(t *testing.T) {
	data := minimalDOCX(t, "First paragraph.", "Second paragraph.")
	doc, err := Convert("doc.docx", MimeDOCX, data)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(doc.Text, "First paragraph.") || !strings.Contains(doc.Text, "Second paragraph.") {
		t.Errorf("expected both paragraphs in extracted text, got %q", doc.Text)
	}
}

func TestConvert_DOCX_NotAZipFails(t *testing.T) {
	if _, err := Convert("doc.docx", MimeDOCX, []byte("not a zip")); err == nil {
		t.Fatal("expected an error for a non-zip docx, got nil")
	}
}
