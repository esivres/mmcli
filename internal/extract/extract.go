// Package extract turns downloaded attachments into plain text for an
// automated reader. Images are refused on purpose: there is no OCR, the
// reader is expected to look at them directly.
package extract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrImage means the file is an image and has no text to extract.
var ErrImage = errors.New("image files have no text extraction (no OCR); download the file and view it")

// Limits bound the work done on untrusted input.
type Limits struct {
	MaxText       int   // characters kept in the result; the middle is cut
	MaxFileBytes  int64 // largest file or archive entry read
	MaxTotalBytes int64 // total uncompressed bytes read from an archive
	MaxEntries    int   // archive entries inspected
	MaxDepth      int   // nested archives followed
}

// DefaultLimits suit a chat attachment read by an agent.
var DefaultLimits = Limits{MaxText: 100_000, MaxFileBytes: 50 << 20, MaxTotalBytes: 200 << 20, MaxEntries: 500, MaxDepth: 2}

// Result is extracted text; Omitted counts characters cut from the middle.
type Result struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
	Omitted   int    `json:"omitted_chars,omitempty"`
}

// File extracts text from the file at path; name and mime come from the
// server and pick the format.
func File(filePath, name, mime string, lim Limits) (Result, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	data, err := readLimited(f, lim.MaxFileBytes)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", name, err)
	}
	budget := lim.MaxTotalBytes
	text, err := extract(data, name, mime, lim, 0, &budget)
	if err != nil {
		return Result{}, err
	}
	return truncate(text, lim.MaxText), nil
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("larger than %d bytes", max)
	}
	return data, nil
}

func extract(data []byte, name, mime string, lim Limits, depth int, budget *int64) (string, error) {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	switch {
	case strings.HasPrefix(mime, "image/") || imageExt[ext]:
		return "", ErrImage
	case ext == "pdf" || mime == "application/pdf":
		return pdfText(data)
	case ext == "docx":
		return docxText(data)
	case ext == "xlsx":
		return xlsxText(data)
	case ext == "zip" || mime == "application/zip":
		return zipText(data, lim, depth, budget)
	case isText(data, ext, mime):
		return strings.ToValidUTF8(string(data), "�"), nil
	default:
		return "", fmt.Errorf("unsupported file type %q (%s)", ext, mime)
	}
}

var imageExt = map[string]bool{"png": true, "jpg": true, "jpeg": true, "gif": true, "bmp": true, "webp": true, "tif": true, "tiff": true, "heic": true}

// isText accepts declared text types and anything that looks like UTF-8
// without NUL bytes (logs, configs, source code with unknown extensions).
func isText(data []byte, ext, mime string) bool {
	if strings.HasPrefix(mime, "text/") || strings.Contains(mime, "json") || strings.Contains(mime, "xml") || strings.Contains(mime, "yaml") {
		return true
	}
	head := data[:min(len(data), 8192)]
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	// A multi-byte rune may be cut at the sample edge.
	for i := 0; i < 4 && len(head) > 0 && !utf8.Valid(head); i++ {
		head = head[:len(head)-1]
	}
	return utf8.Valid(head)
}

// truncate keeps the start and the end: logs usually matter at both.
func truncate(text string, max int) Result {
	runes := []rune(text)
	if max <= 0 || len(runes) <= max {
		return Result{Text: text}
	}
	head, tail := max*2/3, max-max*2/3
	omitted := len(runes) - head - tail
	cut := string(runes[:head]) + fmt.Sprintf("\n[... %d characters omitted ...]\n", omitted) + string(runes[len(runes)-tail:])
	return Result{Text: cut, Truncated: true, Omitted: omitted}
}

func pdfText(data []byte) (string, error) {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", errors.New("pdftotext not found: install poppler (e.g. brew install poppler or apt install poppler-utils)")
	}
	tmp, err := os.CreateTemp("", "mmcli-*.pdf")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	var out, stderr bytes.Buffer
	cmd := exec.Command(bin, "-layout", "-q", tmp.Name(), "-")
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

func openZip(data []byte) (*zip.Reader, error) {
	return zip.NewReader(bytes.NewReader(data), int64(len(data)))
}

func zipFile(zr *zip.Reader, name string, max int64) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return readLimited(rc, max)
		}
	}
	return nil, fmt.Errorf("%s missing", name)
}

// docxText reads paragraphs of word/document.xml.
func docxText(data []byte) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", fmt.Errorf("docx: %w", err)
	}
	doc, err := zipFile(zr, "word/document.xml", DefaultLimits.MaxFileBytes)
	if err != nil {
		return "", fmt.Errorf("docx: %w", err)
	}
	var b strings.Builder
	dec := xml.NewDecoder(bytes.NewReader(doc))
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docx: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				b.WriteByte('\t')
			case "br":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
	return b.String(), nil
}

// xlsxText renders every sheet as tab-separated rows, in workbook order.
func xlsxText(data []byte) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", fmt.Errorf("xlsx: %w", err)
	}
	max := DefaultLimits.MaxFileBytes
	var shared []string
	if raw, err := zipFile(zr, "xl/sharedStrings.xml", max); err == nil {
		var sst struct {
			SI []struct {
				Inner []byte `xml:",innerxml"`
			} `xml:"si"`
		}
		if err := xml.Unmarshal(raw, &sst); err != nil {
			return "", fmt.Errorf("xlsx shared strings: %w", err)
		}
		for _, si := range sst.SI {
			shared = append(shared, allText(si.Inner))
		}
	}

	wbRaw, err := zipFile(zr, "xl/workbook.xml", max)
	if err != nil {
		return "", fmt.Errorf("xlsx: %w", err)
	}
	var wb struct {
		Sheets []struct {
			Name  string     `xml:"name,attr"`
			Attrs []xml.Attr `xml:",any,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(wbRaw, &wb); err != nil {
		return "", fmt.Errorf("xlsx workbook: %w", err)
	}
	relsRaw, err := zipFile(zr, "xl/_rels/workbook.xml.rels", max)
	if err != nil {
		return "", fmt.Errorf("xlsx: %w", err)
	}
	var rels struct {
		Rel []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(relsRaw, &rels); err != nil {
		return "", fmt.Errorf("xlsx rels: %w", err)
	}
	targets := map[string]string{}
	for _, r := range rels.Rel {
		t := strings.TrimPrefix(r.Target, "/")
		if !strings.HasPrefix(t, "xl/") {
			t = "xl/" + t
		}
		targets[r.ID] = t
	}

	var b strings.Builder
	for _, sh := range wb.Sheets {
		var rid string
		for _, a := range sh.Attrs {
			if a.Name.Local == "id" {
				rid = a.Value
			}
		}
		raw, err := zipFile(zr, targets[rid], max)
		if err != nil {
			return "", fmt.Errorf("xlsx sheet %q: %w", sh.Name, err)
		}
		fmt.Fprintf(&b, "== Sheet: %s ==\n", sh.Name)
		if err := sheetRows(raw, shared, &b); err != nil {
			return "", fmt.Errorf("xlsx sheet %q: %w", sh.Name, err)
		}
	}
	return b.String(), nil
}

func sheetRows(raw []byte, shared []string, b *strings.Builder) error {
	var ws struct {
		Rows []struct {
			Cells []struct {
				Ref    string `xml:"r,attr"`
				Type   string `xml:"t,attr"`
				Value  string `xml:"v"`
				Inline struct {
					Inner []byte `xml:",innerxml"`
				} `xml:"is"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(raw, &ws); err != nil {
		return err
	}
	for _, row := range ws.Rows {
		var cells []string
		for _, c := range row.Cells {
			col := colIndex(c.Ref)
			for col > len(cells) {
				cells = append(cells, "")
			}
			v := c.Value
			switch c.Type {
			case "s":
				if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < len(shared) {
					v = shared[i]
				}
			case "inlineStr":
				v = allText(c.Inline.Inner)
			}
			cells = append(cells, strings.ReplaceAll(v, "\t", " "))
		}
		b.WriteString(strings.Join(cells, "\t"))
		b.WriteByte('\n')
	}
	return nil
}

// colIndex converts the letters of a cell reference ("C7") to a 0-based column.
func colIndex(ref string) int {
	n := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		n = n*26 + int(r-'A'+1)
	}
	return max(n-1, 0)
}

// allText concatenates the text of every <t> element in an XML fragment.
func allText(inner []byte) string {
	var b strings.Builder
	dec := xml.NewDecoder(bytes.NewReader(inner))
	in := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return b.String()
		}
		switch t := tok.(type) {
		case xml.StartElement:
			in = t.Name.Local == "t"
		case xml.EndElement:
			in = false
		case xml.CharData:
			if in {
				b.Write(t)
			}
		}
	}
}

// zipText extracts every supported entry, within the limits, and lists what
// was skipped so nothing disappears silently.
func zipText(data []byte, lim Limits, depth int, budget *int64) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", fmt.Errorf("zip: %w", err)
	}
	files := append([]*zip.File(nil), zr.File...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	var b strings.Builder
	seen := 0
	for _, f := range files {
		if f.FileInfo().IsDir() {
			continue
		}
		if seen == lim.MaxEntries {
			fmt.Fprintf(&b, "=== ... more entries skipped (limit %d) ===\n", lim.MaxEntries)
			break
		}
		seen++
		fmt.Fprintf(&b, "=== %s ===\n", f.Name)
		ext := strings.ToLower(path.Ext(f.Name))
		if ext == ".zip" && depth+1 > lim.MaxDepth {
			fmt.Fprintf(&b, "(skipped: nested archive deeper than %d)\n", lim.MaxDepth)
			continue
		}
		if *budget <= 0 {
			b.WriteString("(skipped: archive size limit reached)\n")
			continue
		}
		rc, err := f.Open()
		if err != nil {
			fmt.Fprintf(&b, "(skipped: %v)\n", err)
			continue
		}
		// Headers can lie about sizes; the reader enforces the real ones.
		limit := min(lim.MaxFileBytes, *budget)
		content, err := readLimited(rc, limit)
		rc.Close()
		if err != nil {
			fmt.Fprintf(&b, "(skipped: %v)\n", err)
			*budget -= limit // that much was read before giving up
			continue
		}
		*budget -= int64(len(content))
		text, err := extract(content, f.Name, "", lim, depth+1, budget)
		if err != nil {
			fmt.Fprintf(&b, "(skipped: %v)\n", err)
			continue
		}
		b.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}
