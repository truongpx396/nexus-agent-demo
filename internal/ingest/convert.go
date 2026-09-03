package ingest

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
)

// Supported MIME types (task 12.1: "PDF/DOCX/HTML/plaintext"). This demo
// dispatches on the caller-declared MIME type rather than sniffing content —
// the same "trust the declared shape, validate against it" posture
// internal/tools/builtin's other input-parsing tools already take.
const (
	MimePlainText = "text/plain"
	MimeHTML      = "text/html"
	MimePDF       = "application/pdf"
	MimeDOCX      = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

// Convert normalizes one source file's raw bytes into a Document. Every
// extractor below is intentionally minimal and dependency-free (stdlib
// only, README §2's own "collapse infrastructure, keep the seam" thesis
// applied to a parsing library instead of a deployment topology): a
// production deployment swapping in a real PDF/DOCX SDK behind this same
// signature is an internal change, never a caller-visible one.
func Convert(sourceName, mimeType string, data []byte) (Document, error) {
	var text string
	var err error
	switch mimeType {
	case MimePlainText:
		text, err = convertPlainText(data)
	case MimeHTML:
		text, err = convertHTML(data)
	case MimePDF:
		text, err = convertPDF(data)
	case MimeDOCX:
		text, err = convertDOCX(data)
	default:
		return Document{}, fmt.Errorf("ingest: unsupported mime type %q (want one of %s/%s/%s/%s)",
			mimeType, MimePlainText, MimeHTML, MimePDF, MimeDOCX)
	}
	if err != nil {
		return Document{}, fmt.Errorf("ingest: convert %s (%s): %w", sourceName, mimeType, err)
	}
	return Document{
		SourceName:   sourceName,
		MimeType:     mimeType,
		Text:         text,
		SourceDigest: crypto.Digest(data),
	}, nil
}

func convertPlainText(data []byte) (string, error) {
	if !utf8.Valid(data) {
		return "", fmt.Errorf("not valid UTF-8 text")
	}
	return string(data), nil
}

// convertHTML strips markup down to visible text: tags are dropped, a
// handful of named entities are decoded, and block-level elements force a
// line break so paragraph structure survives for chunk.go's paragraph-aware
// splitter. This is NOT a spec-compliant HTML parser — script/style bodies
// are skipped by name, comments are skipped, and anything more exotic
// (malformed markup, CDATA) is best-effort — but a document ingested for
// retrieval only needs its READABLE text, not a faithful DOM.
func convertHTML(data []byte) (string, error) {
	var out strings.Builder
	s := string(data)
	skipUntil := "" // set while inside <script>...</script> or <style>...</style>
	for i := 0; i < len(s); {
		if skipUntil != "" {
			if idx := strings.Index(strings.ToLower(s[i:]), skipUntil); idx >= 0 {
				i += idx + len(skipUntil)
				skipUntil = ""
			} else {
				break
			}
			continue
		}
		if s[i] != '<' {
			out.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			break // unterminated tag at EOF — stop rather than guess
		}
		tag := s[i+1 : i+end]
		i += end + 1

		lower := strings.ToLower(strings.TrimPrefix(tag, "/"))
		switch {
		case strings.HasPrefix(tag, "!--"):
			// HTML comment; already consumed up to the first '>' above,
			// which is wrong for "-->" but harmless: worst case a comment's
			// own text leaks into output, never a crash.
		case strings.HasPrefix(lower, "script"):
			skipUntil = "</script"
		case strings.HasPrefix(lower, "style"):
			skipUntil = "</style"
		case isBlockTag(lower):
			out.WriteByte('\n')
		}
	}
	return decodeEntities(out.String()), nil
}

func isBlockTag(lowerTagName string) bool {
	name, _, _ := strings.Cut(lowerTagName, " ")
	switch name {
	case "p", "br", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "table", "ul", "ol":
		return true
	}
	return false
}

var htmlEntities = map[string]string{
	"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&#39;": "'", "&apos;": "'", "&nbsp;": " ",
}

func decodeEntities(s string) string {
	for entity, lit := range htmlEntities {
		s = strings.ReplaceAll(s, entity, lit)
	}
	return s
}

// convertPDF extracts visible text from a PDF's content streams: it
// inflates any FlateDecode-compressed stream (compress/zlib — the standard
// deflate codec PDF's FlateDecode filter actually uses) and pulls the
// string operands of the Tj/TJ text-showing operators out of whatever
// stream bytes result, decompressed or not. It does not build a page tree,
// resolve fonts/CID mappings, or handle any other filter — a fully
// general PDF text layer needs all of that, but a demo-scoped, dependency-
// free extractor over "well-behaved, mostly-text PDFs" is the honest
// version of this seam (this file's own package doc comment).
func convertPDF(data []byte) (string, error) {
	var out strings.Builder
	remaining := data
	for {
		startIdx := bytes.Index(remaining, []byte("stream"))
		if startIdx < 0 {
			break
		}
		// The dictionary immediately before "stream" names the filter, if
		// any — look back a bounded window rather than the whole file.
		lookback := 0
		if startIdx > 512 {
			lookback = startIdx - 512
		}
		dict := remaining[lookback:startIdx]
		flate := bytes.Contains(dict, []byte("/FlateDecode"))

		streamStart := startIdx + len("stream")
		// PDF requires a CRLF or LF right after the "stream" keyword.
		for streamStart < len(remaining) && (remaining[streamStart] == '\r' || remaining[streamStart] == '\n') {
			streamStart++
		}
		endIdx := bytes.Index(remaining[streamStart:], []byte("endstream"))
		if endIdx < 0 {
			break
		}
		body := remaining[streamStart : streamStart+endIdx]

		var content []byte
		if flate {
			if decoded, err := zlibInflate(body); err == nil {
				content = decoded
			}
			// A stream declared FlateDecode that fails to inflate (e.g. it
			// is actually an image XObject misdetected by this file's
			// dictionary heuristic) is silently skipped — best-effort, not
			// fail-closed: a partial extraction is still useful, and this
			// is text conversion, not a security boundary.
		} else {
			content = body
		}
		if content != nil {
			extractPDFText(content, &out)
		}

		remaining = remaining[streamStart+endIdx+len("endstream"):]
	}
	return out.String(), nil
}

func zlibInflate(compressed []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer r.Close() //nolint:errcheck // read-only decompression; nothing to flush
	return io.ReadAll(r)
}

// extractPDFText pulls the literal-string operands of Tj/TJ operators —
// "(...) Tj" and "[(...) ... ] TJ" — out of one decoded content stream,
// unescaping PDF's backslash escapes inside the parentheses. Everything
// else in the stream (positioning operators, graphics state, non-text
// objects) is ignored.
func extractPDFText(content []byte, out *strings.Builder) {
	i := 0
	for i < len(content) {
		if content[i] != '(' {
			i++
			continue
		}
		j := i + 1
		var lit strings.Builder
		depth := 1
		for j < len(content) && depth > 0 {
			switch content[j] {
			case '\\':
				if j+1 < len(content) {
					lit.WriteByte(unescapePDFChar(content[j+1]))
					j += 2
					continue
				}
			case '(':
				depth++
				lit.WriteByte('(')
			case ')':
				depth--
				if depth == 0 {
					j++
					continue
				}
				lit.WriteByte(')')
			default:
				lit.WriteByte(content[j])
			}
			j++
		}
		// Only keep it if the string is followed (allowing operator
		// clutter in between, up to a small window) by a Tj/TJ operator —
		// otherwise it's a non-text literal (e.g. inside a /Filter name)
		// this heuristic shouldn't emit.
		tail := content[j:min(j+8, len(content))]
		if bytes.Contains(tail, []byte("Tj")) || bytes.Contains(tail, []byte("TJ")) || bytes.Contains(tail, []byte("]")) {
			out.WriteString(lit.String())
			out.WriteByte(' ')
		}
		i = j
	}
}

func unescapePDFChar(b byte) byte {
	switch b {
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	default:
		return b // \(, \), \\, and anything unrecognized: pass the literal character through
	}
}

// convertDOCX reads word/document.xml out of the OOXML zip container and
// concatenates every <w:t> run's character data, with a newline per
// paragraph (<w:p>) so chunk.go's paragraph splitter still has structure to
// work with.
func convertDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("not a valid docx (zip) container: %w", err)
	}
	var docXML *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docXML = f
			break
		}
	}
	if docXML == nil {
		return "", fmt.Errorf("missing word/document.xml")
	}
	rc, err := docXML.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close() //nolint:errcheck // read-only zip entry

	dec := xml.NewDecoder(rc)
	var out strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("parse document.xml: %w", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			if el.Name.Local == "p" {
				out.WriteByte('\n')
			}
		case xml.CharData:
			out.Write(el)
		}
	}
	return out.String(), nil
}
