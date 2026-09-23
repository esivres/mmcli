// Package extract turns downloaded attachments into plain text for an
// automated reader. Images are refused on purpose: there is no OCR, the
// reader is expected to look at them directly.
package extract

import (
	"archive/zip"
	"bytes"
	"context"
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
	MaxText       int   // bytes of text kept; the middle of longer text is cut
	MaxOutput     int64 // bytes of text produced before extraction stops
	MaxFileBytes  int64 // largest file, archive entry or document part read
	MaxTotalBytes int64 // total uncompressed bytes read, archives and parts included
	MaxEntries    int   // archive entries inspected
	MaxDepth      int   // nested archives followed
}

// DefaultLimits suit a chat attachment read by an agent.
var DefaultLimits = Limits{MaxText: 200_000, MaxOutput: 64 << 20, MaxFileBytes: 50 << 20, MaxTotalBytes: 200 << 20, MaxEntries: 500, MaxDepth: 2}

// Result is extracted text; Omitted counts bytes cut from the middle.
type Result struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
	Omitted   int64  `json:"omitted_bytes,omitempty"`
}

// maxColumns is the last column Excel allows (XFD).
const maxColumns = 16384

type extractor struct {
	ctx    context.Context
	lim    Limits
	budget int64
	out    *sink
}

// File extracts text from the file at filePath; name and mime come from the
// server and pick the format.
func File(ctx context.Context, filePath, name, mime string, lim Limits) (Result, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	data, err := readLimited(f, lim.MaxFileBytes)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", name, err)
	}
	x := &extractor{ctx: ctx, lim: lim, budget: lim.MaxTotalBytes - int64(len(data)), out: newSink(lim.MaxText, lim.MaxOutput)}
	err = x.extract(data, name, mime, 0)
	stopped := errors.Is(err, errOutputLimit) || x.out.failed()
	if err != nil && !stopped {
		return Result{}, err
	}
	return x.out.result(stopped), nil
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

const (
	mimeDocx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeXlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

func (x *extractor) extract(data []byte, name, mime string, depth int) error {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	switch {
	case strings.HasPrefix(mime, "image/") || imageExt[ext]:
		return ErrImage
	case ext == "pdf" || mime == "application/pdf":
		return x.pdf(data)
	case ext == "docx" || mime == mimeDocx:
		return x.docx(data)
	case ext == "xlsx" || mime == mimeXlsx:
		return x.xlsx(data)
	case ext == "zip" || mime == "application/zip":
		return x.zip(data, depth)
	case isText(data, mime):
		_, err := x.out.Write(data)
		return err
	default:
		return fmt.Errorf("unsupported file type %q (%s)", ext, mime)
	}
}

var imageExt = map[string]bool{"png": true, "jpg": true, "jpeg": true, "gif": true, "bmp": true, "webp": true, "tif": true, "tiff": true, "heic": true}

// isText accepts UTF-8 without NUL bytes: logs, configs and source code often
// come with no or a generic MIME type, while office formats (zip) never pass.
func isText(data []byte, mime string) bool {
	head := data[:min(len(data), 8192)]
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	if strings.HasPrefix(mime, "text/") || strings.HasSuffix(mime, "/json") || strings.HasSuffix(mime, "+json") ||
		strings.HasSuffix(mime, "/xml") || strings.HasSuffix(mime, "+xml") || strings.Contains(mime, "yaml") {
		return true
	}
	// A multi-byte rune may be cut at the sample edge.
	for i := 0; i < 4 && len(head) > 0 && !utf8.Valid(head); i++ {
		head = head[:len(head)-1]
	}
	return utf8.Valid(head)
}

func (x *extractor) pdf(data []byte) error {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return errors.New("pdftotext not found: install poppler (e.g. brew install poppler or apt install poppler-utils)")
	}
	tmp, err := os.CreateTemp("", "mmcli-*.pdf")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	ctx, cancel := context.WithCancel(x.ctx)
	defer cancel()
	// Killing pdftotext once the output limit is hit bounds its CPU time too.
	x.out.onStop = cancel
	defer func() { x.out.onStop = nil }()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-layout", "-q", tmp.Name(), "-")
	cmd.Stdout, cmd.Stderr = x.out, &stderr
	if err := cmd.Run(); err != nil {
		if x.out.failed() {
			return errOutputLimit
		}
		return fmt.Errorf("pdftotext: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func openZip(data []byte) (*zip.Reader, error) {
	return zip.NewReader(bytes.NewReader(data), int64(len(data)))
}

// part reads one document part, charged to the shared byte budget.
func (x *extractor) part(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		if x.budget <= 0 {
			return nil, errors.New("size limit reached")
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		limit := min(x.lim.MaxFileBytes, x.budget)
		data, err := readLimited(rc, limit)
		if err != nil {
			x.budget -= limit
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		x.budget -= int64(len(data))
		return data, nil
	}
	return nil, fmt.Errorf("%s missing", name)
}

// docx writes paragraphs of word/document.xml; table cells are separated by
// tabs and rows by newlines.
func (x *extractor) docx(data []byte) error {
	zr, err := openZip(data)
	if err != nil {
		return fmt.Errorf("docx: %w", err)
	}
	doc, err := x.part(zr, "word/document.xml")
	if err != nil {
		return fmt.Errorf("docx: %w", err)
	}
	dec := xml.NewDecoder(bytes.NewReader(doc))
	inText, runDepth, cellDepth := false, 0, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("docx: %w", err)
		}
		var werr error
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "r":
				runDepth++
			case "tc":
				cellDepth++
			case "tab":
				// Tab stops in paragraph properties are not text.
				if runDepth > 0 {
					werr = x.out.WriteByte('\t')
				}
			case "br":
				if runDepth > 0 {
					werr = x.out.WriteByte('\n')
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "r":
				runDepth--
			case "p":
				if cellDepth > 0 {
					werr = x.out.WriteByte(' ')
				} else {
					werr = x.out.WriteByte('\n')
				}
			case "tc":
				cellDepth--
				werr = x.out.WriteByte('\t')
			case "tr":
				werr = x.out.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				_, werr = x.out.Write(t)
			}
		}
		if werr != nil {
			return werr
		}
	}
}

// xlsx writes every sheet as tab-separated rows, in workbook order.
func (x *extractor) xlsx(data []byte) error {
	zr, err := openZip(data)
	if err != nil {
		return fmt.Errorf("xlsx: %w", err)
	}
	var shared []string
	if raw, err := x.part(zr, "xl/sharedStrings.xml"); err == nil {
		var sst struct {
			SI []struct {
				Inner []byte `xml:",innerxml"`
			} `xml:"si"`
		}
		if err := xml.Unmarshal(raw, &sst); err != nil {
			return fmt.Errorf("xlsx shared strings: %w", err)
		}
		for _, si := range sst.SI {
			shared = append(shared, allText(si.Inner))
		}
	}

	wbRaw, err := x.part(zr, "xl/workbook.xml")
	if err != nil {
		return fmt.Errorf("xlsx: %w", err)
	}
	var wb struct {
		Sheets []struct {
			Name  string     `xml:"name,attr"`
			Attrs []xml.Attr `xml:",any,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(wbRaw, &wb); err != nil {
		return fmt.Errorf("xlsx workbook: %w", err)
	}
	relsRaw, err := x.part(zr, "xl/_rels/workbook.xml.rels")
	if err != nil {
		return fmt.Errorf("xlsx: %w", err)
	}
	var rels struct {
		Rel []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(relsRaw, &rels); err != nil {
		return fmt.Errorf("xlsx rels: %w", err)
	}
	targets := map[string]string{}
	for _, r := range rels.Rel {
		t := strings.TrimPrefix(r.Target, "/")
		if !strings.HasPrefix(t, "xl/") {
			t = "xl/" + t
		}
		targets[r.ID] = t
	}

	done := map[string]bool{}
	for _, sh := range wb.Sheets {
		var rid string
		for _, a := range sh.Attrs {
			if a.Name.Local == "id" {
				rid = a.Value
			}
		}
		target := targets[rid]
		fmt.Fprintf(x.out, "== Sheet: %s ==\n", sh.Name)
		if done[target] {
			// Several sheets pointing at one part would multiply the work.
			fmt.Fprintf(x.out, "(same data as an earlier sheet)\n")
			continue
		}
		done[target] = true
		raw, err := x.part(zr, target)
		if err != nil {
			return fmt.Errorf("xlsx sheet %q: %w", sh.Name, err)
		}
		if err := x.sheetRows(raw, shared); err != nil {
			return err
		}
	}
	return nil
}

func (x *extractor) sheetRows(raw []byte, shared []string) error {
	var ws struct {
		Rows []struct {
			Num   int `xml:"r,attr"`
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
		return fmt.Errorf("xlsx sheet: %w", err)
	}
	prev := 0
	for _, row := range ws.Rows {
		if row.Num > prev+1 && prev > 0 {
			// One blank line marks skipped rows without expanding huge gaps.
			if err := x.out.WriteByte('\n'); err != nil {
				return err
			}
		}
		if row.Num > 0 {
			prev = row.Num
		} else {
			prev++
		}
		var cells []string
		for _, c := range row.Cells {
			col, ok := colIndex(c.Ref)
			if !ok {
				col = len(cells) // a missing or broken reference follows the previous cell
			}
			for col >= len(cells) {
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
			cells[col] = strings.ReplaceAll(v, "\t", " ")
		}
		if _, err := x.out.Write([]byte(strings.Join(cells, "\t") + "\n")); err != nil {
			return err
		}
	}
	return nil
}

// colIndex converts the letters of a cell reference ("C7") to a 0-based
// column; references beyond Excel's last column are rejected.
func colIndex(ref string) (int, bool) {
	n := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		n = n*26 + int(r-'A'+1)
		if n > maxColumns {
			return 0, false
		}
	}
	return n - 1, n > 0
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

// zip extracts every supported entry, within the limits, and lists what was
// skipped so nothing disappears silently.
func (x *extractor) zip(data []byte, depth int) error {
	zr, err := openZip(data)
	if err != nil {
		return fmt.Errorf("zip: %w", err)
	}
	files := append([]*zip.File(nil), zr.File...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	seen := 0
	for _, f := range files {
		if f.FileInfo().IsDir() {
			continue
		}
		if seen == x.lim.MaxEntries {
			fmt.Fprintf(x.out, "=== ... more entries skipped (limit %d) ===\n", x.lim.MaxEntries)
			return nil
		}
		seen++
		fmt.Fprintf(x.out, "=== %s ===\n", f.Name)
		if x.out.failed() {
			return errOutputLimit
		}
		if strings.ToLower(path.Ext(f.Name)) == ".zip" && depth+1 > x.lim.MaxDepth {
			fmt.Fprintf(x.out, "(skipped: nested archive deeper than %d)\n", x.lim.MaxDepth)
			continue
		}
		if x.budget <= 0 {
			fmt.Fprintf(x.out, "(skipped: archive size limit reached)\n")
			continue
		}
		rc, err := f.Open()
		if err != nil {
			fmt.Fprintf(x.out, "(skipped: %v)\n", err)
			continue
		}
		// Headers can lie about sizes; the reader enforces the real ones.
		limit := min(x.lim.MaxFileBytes, x.budget)
		content, err := readLimited(rc, limit)
		rc.Close()
		if err != nil {
			fmt.Fprintf(x.out, "(skipped: %v)\n", err)
			x.budget -= limit // that much was read before giving up
			continue
		}
		x.budget -= int64(len(content))
		before := x.out.total
		if err := x.extract(content, f.Name, "", depth+1); err != nil {
			if errors.Is(err, errOutputLimit) {
				return err
			}
			fmt.Fprintf(x.out, "(skipped: %v)\n", err)
			continue
		}
		if x.out.total > before {
			x.out.WriteString("\n")
		}
	}
	return nil
}
